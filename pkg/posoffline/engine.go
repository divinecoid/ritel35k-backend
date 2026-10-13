package posoffline

import (
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/ritel35k/backend/pkg/apperr"
	"github.com/ritel35k/backend/pkg/promoengine"
)

var hundred = decimal.NewFromInt(100)

// ---- cart / quote contract (JSON identical to the online /pos/quote so the POS screen cannot tell them apart)

type ItemInput struct {
	VariantID       *uuid.UUID      `json:"variant_id"`
	Barcode         string          `json:"barcode"`
	EPC             string          `json:"epc"`
	Qty             decimal.Decimal `json:"qty"`
	DiscountPercent decimal.Decimal `json:"discount_percent"`
}

type CartInput struct {
	TerminalID          uuid.UUID       `json:"terminal_id"`
	Items               []ItemInput     `json:"items"`
	CartDiscountPercent decimal.Decimal `json:"cart_discount_percent"`
	CustomerID          *uuid.UUID      `json:"customer_id"`
	VoucherCode         string          `json:"voucher_code"`
}

type Line struct {
	LineNo          int                   `json:"line_no"`
	VariantID       uuid.UUID             `json:"variant_id"`
	SKU             string                `json:"sku"`
	Name            string                `json:"name"`
	Barcode         string                `json:"barcode"`
	EPC             string                `json:"epc,omitempty"`
	Qty             decimal.Decimal       `json:"qty"`
	UnitPrice       decimal.Decimal       `json:"unit_price"`
	PriceSource     string                `json:"price_source"`
	DiscountPercent decimal.Decimal       `json:"discount_percent"`
	Gross           decimal.Decimal       `json:"gross"`
	PromoDiscount   decimal.Decimal       `json:"promo_discount"`
	Promotions      []promoengine.Applied `json:"promotions"`
	VoucherDiscount decimal.Decimal       `json:"voucher_discount"`
	DiscountAmount  decimal.Decimal       `json:"discount_amount"`
	TaxRate         decimal.Decimal       `json:"tax_rate"`
	TaxAmount       decimal.Decimal       `json:"tax_amount"`
	LineTotal       decimal.Decimal       `json:"line_total"`
}

type CustomerInfo struct {
	ID             uuid.UUID       `json:"id"`
	MemberNo       string          `json:"member_no"`
	Name           string          `json:"name"`
	Phone          string          `json:"phone"`
	LevelName      string          `json:"level_name"`
	PointsBalance  int64           `json:"points_balance"`
	EarnMultiplier decimal.Decimal `json:"earn_multiplier"`
}

type Quote struct {
	Lines              []Line                `json:"lines"`
	Subtotal           decimal.Decimal       `json:"subtotal"`
	DiscountTotal      decimal.Decimal       `json:"discount_total"`
	PromoDiscount      decimal.Decimal       `json:"promo_discount"`
	VoucherDiscount    decimal.Decimal       `json:"voucher_discount"`
	TaxTotal           decimal.Decimal       `json:"tax_total"`
	GrandTotal         decimal.Decimal       `json:"grand_total"`
	PriceIncludesTax   bool                  `json:"price_includes_tax"`
	MaxDiscountPercent decimal.Decimal       `json:"max_discount_percent"`
	ApprovalRequired   bool                  `json:"approval_required"`
	PaymentMethods     []string              `json:"payment_methods"`
	Promotions         []promoengine.Applied `json:"promotions"`
	Customer           *CustomerInfo         `json:"customer,omitempty"`
	PointsEarn         int64                 `json:"points_earn"`
	MaxRedeemPoints    int64                 `json:"max_redeem_points"`
	RedeemValue        decimal.Decimal       `json:"redeem_value"`
	MinRedeemPoints    int64                 `json:"min_redeem_points"`

	Offline         bool   `json:"offline"`
	SnapshotVersion string `json:"snapshot_version"`
}

// TagResolver maps an RFID tag to its variant (the edge answers from its tag snapshot). ok=false: unknown or not
// sellable here, with the reason as an error.
type TagResolver func(epc string) (variantID uuid.UUID, err error)

// Engine prices carts against one snapshot. It is immutable after New and safe for concurrent use.
type Engine struct {
	snap       *Snapshot
	byVariant  map[uuid.UUID]*Item
	byBarcode  map[string]uuid.UUID
	prices     map[priceKey][]Price // (variant, list) -> rows
	defaultID  uuid.UUID
	levelList  map[uuid.UUID]uuid.UUID // membership level -> price list
	members    map[uuid.UUID]*Member
	byPhone    map[string]*Member
	byMemberNo map[string]*Member
	loc        *time.Location
}

type priceKey struct{ variant, list uuid.UUID }

func New(s *Snapshot) *Engine {
	e := &Engine{snap: s, byVariant: map[uuid.UUID]*Item{}, byBarcode: map[string]uuid.UUID{}, prices: map[priceKey][]Price{},
		levelList: map[uuid.UUID]uuid.UUID{}, members: map[uuid.UUID]*Member{}, byPhone: map[string]*Member{}, byMemberNo: map[string]*Member{}}
	for i := range s.Items {
		it := &s.Items[i]
		e.byVariant[it.VariantID] = it
		for _, b := range it.Barcodes {
			e.byBarcode[b] = it.VariantID
		}
	}
	for _, pl := range s.PriceLists {
		if pl.IsDefault {
			e.defaultID = pl.ID
		}
		if pl.LevelID != nil {
			e.levelList[*pl.LevelID] = pl.ID
		}
	}
	for _, p := range s.Prices {
		k := priceKey{p.VariantID, p.ListID}
		e.prices[k] = append(e.prices[k], p)
	}
	for i := range s.Members {
		m := &s.Members[i]
		e.members[m.ID] = m
		e.byPhone[m.Phone] = m
		e.byMemberNo[strings.ToUpper(m.MemberNo)] = m
	}
	e.loc, _ = time.LoadLocation(s.Timezone)
	if e.loc == nil {
		e.loc = time.UTC
	}
	return e
}

func (e *Engine) Snapshot() *Snapshot { return e.snap }

func (e *Engine) Location() *time.Location { return e.loc }

// resolvePrice mirrors SQL ResolvePrice: within one list, nearest org unit first, then the highest qualifying
// volume tier, then the most recently effective row.
func (e *Engine) resolvePrice(variant, list uuid.UUID, qty decimal.Decimal, at time.Time) (Price, bool) {
	var best Price
	found := false
	for _, p := range e.prices[priceKey{variant, list}] {
		if p.MinQty.GreaterThan(qty) || p.From.After(at) || (p.To != nil && !p.To.After(at)) {
			continue
		}
		if !found || p.Depth > best.Depth ||
			(p.Depth == best.Depth && (p.MinQty.GreaterThan(best.MinQty) || (p.MinQty.Equal(best.MinQty) && p.From.After(best.From)))) {
			best, found = p, true
		}
	}
	return best, found
}

// resolveBest mirrors pricing.ResolveBest: the lowest price across the default list and the member's level list.
func (e *Engine) resolveBest(variant uuid.UUID, qty decimal.Decimal, level *uuid.UUID, at time.Time) (Price, bool) {
	lists := []uuid.UUID{e.defaultID}
	if level != nil {
		if l, ok := e.levelList[*level]; ok && l != e.defaultID {
			lists = append(lists, l)
		}
	}
	var best Price
	found := false
	for _, l := range lists {
		if p, ok := e.resolvePrice(variant, l, qty, at); ok && (!found || p.Price.LessThan(best.Price)) {
			best, found = p, true
		}
	}
	return best, found
}

func pct(v decimal.Decimal) bool { return v.IsNegative() || v.GreaterThan(hundred) }

func itoa(i int) string { return strconv.Itoa(i) }

// FindMember looks a member up by id, member number or phone (already normalised like the server does).
func (e *Engine) FindMember(q string) []Member {
	q = strings.TrimSpace(q)
	if q == "" {
		return nil
	}
	var out []Member
	seen := map[uuid.UUID]bool{}
	add := func(m *Member) {
		if m != nil && !seen[m.ID] {
			seen[m.ID] = true
			out = append(out, *m)
		}
	}
	add(e.byMemberNo[strings.ToUpper(q)])
	add(e.byPhone[q])
	if len(out) == 0 {
		lq := strings.ToLower(q)
		for i := range e.snap.Members {
			m := &e.snap.Members[i]
			if strings.Contains(strings.ToLower(m.Name), lq) || strings.HasSuffix(m.Phone, q) {
				add(m)
				if len(out) >= 10 {
					break
				}
			}
		}
	}
	return out
}

// Quote prices a cart exactly like the server's priceCart, minus what cannot work offline (vouchers, points
// redemption, supervisor PIN): see the plan's offline rules. `at` is the moment of sale.
func (e *Engine) Quote(in CartInput, at time.Time, tags TagResolver) (*Quote, error) {
	if pct(in.CartDiscountPercent) {
		return nil, apperr.Validation(map[string]string{"cart_discount_percent": "0-100"})
	}
	if strings.TrimSpace(in.VoucherCode) != "" {
		return nil, apperr.Conflict("OFFLINE_NO_VOUCHER", "Vouchers need a connection; remove the voucher or wait until the link is back")
	}
	if len(in.Items) == 0 {
		return nil, apperr.Validation(map[string]string{"items": "at least one item"})
	}
	local := at.In(e.loc)
	st := e.snap.Settings

	var member *Member
	var levelID *uuid.UUID
	if in.CustomerID != nil {
		m, ok := e.members[*in.CustomerID]
		if !ok {
			return nil, apperr.New(404, "CUSTOMER_NOT_FOUND", "Member not found in the offline list")
		}
		member = m
		lv := m.LevelID
		levelID = &lv
	}

	quote := &Quote{PriceIncludesTax: st.PriceIncludesTax, MaxDiscountPercent: st.MaxDiscountPercent, PaymentMethods: st.PaymentMethods,
		Lines: []Line{}, Promotions: []promoengine.Applied{}, Offline: true, SnapshotVersion: e.snap.Version}
	if quote.PaymentMethods == nil {
		quote.PaymentMethods = []string{}
	}

	type row struct {
		item    ItemInput
		variant uuid.UUID
		it      *Item
		barcode string
		epc     string
		price   Price
		gross   decimal.Decimal
	}
	rows := make([]row, len(in.Items))
	pl := make([]promoengine.Line, len(in.Items))
	seenEPC := map[string]bool{}
	for i, it := range in.Items {
		field := func(k string) string { return "items[" + itoa(i) + "]." + k }
		if pct(it.DiscountPercent) {
			return nil, apperr.Validation(map[string]string{field("discount_percent"): "0-100"})
		}
		epc := ""
		if strings.TrimSpace(it.EPC) != "" {
			if tags == nil {
				return nil, apperr.Conflict("OFFLINE_NO_TAGS", "RFID tags cannot be checked offline on this terminal; scan the barcode")
			}
			n := strings.ToUpper(strings.TrimSpace(it.EPC))
			if seenEPC[n] {
				return nil, apperr.Conflict("DUPLICATE_TAG", "The same tag is scanned twice")
			}
			vid, err := tags(n)
			if err != nil {
				return nil, err
			}
			seenEPC[n] = true
			epc = n
			it.Qty = decimal.NewFromInt(1)
			it.VariantID = &vid
			it.Barcode = ""
		}
		if !it.Qty.IsPositive() {
			return nil, apperr.Validation(map[string]string{field("qty"): "must be positive"})
		}
		variant, barcode := uuid.Nil, strings.TrimSpace(it.Barcode)
		switch {
		case it.VariantID != nil:
			variant = *it.VariantID
		case barcode != "":
			v, ok := e.byBarcode[barcode]
			if !ok {
				return nil, apperr.New(404, "UNKNOWN_BARCODE", "Barcode not found: "+barcode)
			}
			variant = v
		default:
			return nil, apperr.Validation(map[string]string{field("variant_id"): "variant_id or barcode required"})
		}
		item, ok := e.byVariant[variant]
		if !ok {
			return nil, apperr.Validation(map[string]string{field("variant_id"): "unknown variant"})
		}
		if !item.AllowsDecimal && !it.Qty.Equal(it.Qty.Truncate(0)) {
			return nil, apperr.Validation(map[string]string{field("qty"): "this item is sold in whole units"})
		}
		if barcode == "" && len(item.Barcodes) > 0 {
			barcode = item.Barcodes[0]
		}
		price, ok := e.resolveBest(variant, it.Qty, levelID, at)
		if !ok {
			return nil, apperr.Conflict("NO_PRICE", "No selling price defined for "+item.SKU)
		}
		gross := price.Price.Mul(it.Qty).Round(0)
		rows[i] = row{item: it, variant: variant, it: item, barcode: barcode, epc: epc, price: price, gross: gross}
		pl[i] = promoengine.Line{No: i + 1, VariantID: variant, CategoryID: item.CategoryID, BrandID: item.BrandID, Qty: it.Qty, UnitPrice: price.Price}
	}

	res := promoengine.Evaluate(e.snap.Promotions, pl, promoengine.Context{Now: local, LevelID: levelID})
	if len(res.Applied) > 0 {
		quote.Promotions = res.Applied
	}

	maxPct := in.CartDiscountPercent
	net := make([]decimal.Decimal, len(rows))
	for i, r := range rows {
		remaining := r.gross.Sub(res.Lines[i].Discount)
		net[i] = remaining.Mul(hundred.Sub(r.item.DiscountPercent)).Div(hundred).Mul(hundred.Sub(in.CartDiscountPercent)).Div(hundred).Round(0)
		if r.item.DiscountPercent.GreaterThan(maxPct) {
			maxPct = r.item.DiscountPercent
		}
	}

	for i, r := range rows {
		n := net[i]
		rate := decimal.Zero
		if r.it.TaxCode == "PPN" {
			rate = st.PPNPercent
		}
		var tax, total decimal.Decimal
		if st.PriceIncludesTax {
			tax = n.Mul(rate).Div(hundred.Add(rate)).Round(0)
			total = n
		} else {
			tax = n.Mul(rate).Div(hundred).Round(0)
			total = n.Add(tax)
		}
		applied := res.Lines[i].Applied
		if applied == nil {
			applied = []promoengine.Applied{}
		}
		quote.Lines = append(quote.Lines, Line{LineNo: i + 1, VariantID: r.variant, SKU: r.it.SKU, Name: r.it.Name, Barcode: r.barcode, EPC: r.epc, Qty: r.item.Qty,
			UnitPrice: r.price.Price, PriceSource: r.price.SourceName, DiscountPercent: r.item.DiscountPercent, Gross: r.gross, PromoDiscount: res.Lines[i].Discount,
			Promotions: applied, VoucherDiscount: decimal.Zero, DiscountAmount: r.gross.Sub(n), TaxRate: rate, TaxAmount: tax, LineTotal: total})
		quote.Subtotal = quote.Subtotal.Add(r.gross)
		quote.DiscountTotal = quote.DiscountTotal.Add(r.gross.Sub(n))
		quote.PromoDiscount = quote.PromoDiscount.Add(res.Lines[i].Discount)
		quote.TaxTotal = quote.TaxTotal.Add(tax)
		quote.GrandTotal = quote.GrandTotal.Add(total)
	}
	// Nobody can type a supervisor PIN to a server that is not there: any discount above the cashier limit is refused.
	quote.ApprovalRequired = maxPct.GreaterThan(quote.MaxDiscountPercent)

	if member != nil {
		quote.Customer = &CustomerInfo{ID: member.ID, MemberNo: member.MemberNo, Name: member.Name, Phone: member.Phone, LevelName: member.LevelName,
			PointsBalance: member.PointsBalance, EarnMultiplier: member.EarnMultiplier}
		quote.PointsEarn = e.pointsFor(quote.GrandTotal, member.EarnMultiplier)
	}
	return quote, nil
}

// pointsFor mirrors customer.Rules.PointsFor. Points are earned on the server at sync; this is only the preview.
func (e *Engine) pointsFor(amount, multiplier decimal.Decimal) int64 {
	s := e.snap.Settings
	if !s.LoyaltyEnabled || !s.EarnPerAmount.IsPositive() || !amount.IsPositive() {
		return 0
	}
	return amount.Div(s.EarnPerAmount).Floor().Mul(multiplier).Floor().IntPart()
}

// PriceFor is the shelf price of one variant (default lists, no member level) for search results.
func (e *Engine) PriceFor(variant uuid.UUID, qty decimal.Decimal, at time.Time) (decimal.Decimal, bool) {
	p, ok := e.resolveBest(variant, qty, nil, at)
	return p.Price, ok
}

// SearchItems finds items by exact barcode/SKU or by name (case-insensitive, every word must match), best first.
func (e *Engine) SearchItems(q string, limit int) []Item {
	q = strings.TrimSpace(q)
	if q == "" || limit <= 0 {
		return nil
	}
	if id, ok := e.byBarcode[q]; ok {
		return []Item{*e.byVariant[id]}
	}
	lq := strings.ToLower(q)
	words := strings.Fields(lq)
	var exact, rest []Item
	for _, it := range e.snap.Items {
		if strings.ToLower(it.SKU) == lq {
			exact = append(exact, it)
			continue
		}
		name := strings.ToLower(it.Name + " " + it.SKU)
		match := true
		for _, w := range words {
			if !strings.Contains(name, w) {
				match = false
				break
			}
		}
		if match {
			rest = append(rest, it)
		}
	}
	out := append(exact, rest...)
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}
