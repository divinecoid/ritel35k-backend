package pos

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/ritel35k/backend/db/gen"
	"github.com/ritel35k/backend/internal/access"
	customerpkg "github.com/ritel35k/backend/internal/customer"
	"github.com/ritel35k/backend/internal/pricing"
	"github.com/ritel35k/backend/internal/promotion"
	"github.com/ritel35k/backend/internal/rfid"
	"github.com/ritel35k/backend/internal/settings"
	"github.com/ritel35k/backend/internal/voucher"
	"github.com/ritel35k/backend/pkg/apperr"
)

type ItemInput struct {
	VariantID       *uuid.UUID      `json:"variant_id"`
	Barcode         string          `json:"barcode" validate:"max=64"`
	EPC             string          `json:"epc" validate:"max=80"` // an RFID tag scanned at the POS: identifies the exact item (qty is always 1)
	Qty             decimal.Decimal `json:"qty"`
	DiscountPercent decimal.Decimal `json:"discount_percent"`
}

type CartInput struct {
	TerminalID          uuid.UUID       `json:"terminal_id" validate:"required"`
	Items               []ItemInput     `json:"items" validate:"required,min=1,max=200,dive"`
	CartDiscountPercent decimal.Decimal `json:"cart_discount_percent"`
	Approval            *ApprovalInput  `json:"approval"`
	CustomerID          *uuid.UUID      `json:"customer_id"`
	VoucherCode         string          `json:"voucher_code" validate:"max=40"`
}

type Line struct {
	LineNo          int                 `json:"line_no"`
	VariantID       uuid.UUID           `json:"variant_id"`
	SKU             string              `json:"sku"`
	Name            string              `json:"name"`
	Barcode         string              `json:"barcode"`
	EPC             string              `json:"epc,omitempty"`
	Qty             decimal.Decimal     `json:"qty"`
	UnitPrice       decimal.Decimal     `json:"unit_price"`
	PriceSource     string              `json:"price_source"`
	DiscountPercent decimal.Decimal     `json:"discount_percent"`
	Gross           decimal.Decimal     `json:"gross"`
	PromoDiscount   decimal.Decimal     `json:"promo_discount"`
	Promotions      []promotion.Applied `json:"promotions"`
	VoucherDiscount decimal.Decimal     `json:"voucher_discount"`
	DiscountAmount  decimal.Decimal     `json:"discount_amount"` // everything taken off: promos + manual + voucher
	TaxRate         decimal.Decimal     `json:"tax_rate"`
	TaxAmount       decimal.Decimal     `json:"tax_amount"`
	LineTotal       decimal.Decimal     `json:"line_total"`
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

type VoucherInfo struct {
	Code     string          `json:"code"`
	Discount decimal.Decimal `json:"discount"`
}

type Quote struct {
	Lines              []Line              `json:"lines"`
	Subtotal           decimal.Decimal     `json:"subtotal"`
	DiscountTotal      decimal.Decimal     `json:"discount_total"`
	PromoDiscount      decimal.Decimal     `json:"promo_discount"`
	VoucherDiscount    decimal.Decimal     `json:"voucher_discount"`
	TaxTotal           decimal.Decimal     `json:"tax_total"`
	GrandTotal         decimal.Decimal     `json:"grand_total"`
	PriceIncludesTax   bool                `json:"price_includes_tax"`
	MaxDiscountPercent decimal.Decimal     `json:"max_discount_percent"` // cashier limit (manual discounts) without approval
	ApprovalRequired   bool                `json:"approval_required"`
	PaymentMethods     []string            `json:"payment_methods"`
	Promotions         []promotion.Applied `json:"promotions"` // per promotion, over the whole cart
	Voucher            *VoucherInfo        `json:"voucher,omitempty"`
	Customer           *CustomerInfo       `json:"customer,omitempty"`
	PointsEarn         int64               `json:"points_earn"`
	MaxRedeemPoints    int64               `json:"max_redeem_points"`
	RedeemValue        decimal.Decimal     `json:"redeem_value"`
	MinRedeemPoints    int64               `json:"min_redeem_points"`
}

// extras carries what checkout needs beyond the quote (rows already loaded and validated).
type extras struct {
	customer *gen.GetCustomerRow
	voucher  *gen.Voucher
	rules    customerpkg.Rules
	promos   map[int][]promotion.Applied // line index -> promotions applied
	epcs     map[int]string              // line index -> RFID tag sold on that line
}

// ctxStore bundles what pricing and checkout need to know about the terminal's store.
type ctxStore struct {
	term  gen.GetTerminalRow
	store gen.GetStoreRow
	snap  settings.Snapshot
}

func loadStore(ctx context.Context, q *gen.Queries, p *access.Principal, perm string, terminalID uuid.UUID) (*ctxStore, error) {
	cs, err := loadTerminalStore(ctx, q, p.CompanyID, terminalID)
	if err != nil {
		return nil, err
	}
	if !p.Grants.Can(perm, cs.term.Path) {
		return nil, apperr.Forbidden("not allowed to sell in this store")
	}
	return cs, nil
}

// loadTerminalStore resolves a terminal to its store and settings without any user permission check (device sync).
func loadTerminalStore(ctx context.Context, q *gen.Queries, companyID, terminalID uuid.UUID) (*ctxStore, error) {
	return loadTerminalStoreMode(ctx, q, companyID, terminalID, true)
}

// loadTerminalStoreMode with strict=false skips the active-terminal/store checks: a sale that already happened
// must still be recorded even if the terminal was switched off in the meantime.
func loadTerminalStoreMode(ctx context.Context, q *gen.Queries, companyID, terminalID uuid.UUID, strict bool) (*ctxStore, error) {
	term, err := q.GetTerminal(ctx, gen.GetTerminalParams{ID: terminalID, CompanyID: companyID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apperr.NotFound("terminal")
	}
	if err != nil {
		return nil, err
	}
	if strict && term.Status != "active" {
		return nil, apperr.Conflict("TERMINAL_INACTIVE", "Terminal is inactive")
	}
	st, err := q.GetStore(ctx, gen.GetStoreParams{ID: term.StoreID, CompanyID: companyID})
	if err != nil {
		return nil, err
	}
	if strict && st.Status != "active" {
		return nil, apperr.Conflict("STORE_INACTIVE", "Store is not active")
	}
	snap, err := settings.LoadSnapshot(ctx, q, companyID, st.OrgUnitID)
	if err != nil {
		return nil, err
	}
	return &ctxStore{term: term, store: st, snap: snap}, nil
}

func pct(v decimal.Decimal) bool { return v.IsNegative() || v.GreaterThan(hundred) }

type row struct {
	item      ItemInput
	variantID uuid.UUID
	v         gen.LookupVariantForSaleRow
	barcode   string
	price     pricing.Resolved
	gross     decimal.Decimal
}

// priceCart resolves every line against the live catalog, price lists, promotions, voucher and tax settings.
// The client's numbers are never trusted: only variant/barcode, qty, discount percentages, customer and
// voucher code are inputs.
//
// Order of operations per line (so the receipt math is explainable):
//
//	gross = unit price (best applicable price list, volume tier) × qty
//	− promotions (engine, see package promotion)
//	− manual line discount %   − cart discount %      (these two need supervisor approval above a limit)
//	− voucher (allocated across lines)                 → net, then tax.
func (s *Service) priceCart(ctx context.Context, q *gen.Queries, p *access.Principal, cs *ctxStore, in CartInput) (*Quote, *extras, error) {
	return s.priceCartAt(ctx, q, p, cs, in, s.now())
}

// priceCartAt prices as of `now` (offline sync re-prices a sale at the moment it happened).
func (s *Service) priceCartAt(ctx context.Context, q *gen.Queries, p *access.Principal, cs *ctxStore, in CartInput, now time.Time) (*Quote, *extras, error) {
	if pct(in.CartDiscountPercent) {
		return nil, nil, apperr.Validation(map[string]string{"cart_discount_percent": "0-100"})
	}
	loc, err := time.LoadLocation(cs.store.Timezone)
	if err != nil {
		loc = time.UTC
	}
	local := now.In(loc)
	ex := &extras{rules: customerpkg.RulesFrom(cs.snap), promos: map[int][]promotion.Applied{}, epcs: map[int]string{}}

	var levelID *uuid.UUID
	if in.CustomerID != nil {
		c, err := q.GetCustomer(ctx, gen.GetCustomerParams{ID: *in.CustomerID, CompanyID: p.CompanyID})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, apperr.New(404, "CUSTOMER_NOT_FOUND", "Customer not found")
		}
		if err != nil {
			return nil, nil, err
		}
		if c.Status != "active" {
			return nil, nil, apperr.Conflict("CUSTOMER_INACTIVE", "Customer is inactive")
		}
		ex.customer = &c
		levelID = &c.LevelID
	}

	inclusive := cs.snap.Bool("tax.price_includes_tax")
	ppn := cs.snap.Decimal("tax.ppn_percent")
	quote := &Quote{PriceIncludesTax: inclusive, MaxDiscountPercent: cs.snap.Decimal("pos.max_discount_percent"), PaymentMethods: cs.snap.Strings("payment.methods"),
		RedeemValue: ex.rules.RedeemValue, MinRedeemPoints: ex.rules.MinRedeemPoints}

	// pass 1: variants and prices
	rows := make([]row, len(in.Items))
	seenEPC := map[string]bool{}
	pl := make([]promotion.Line, len(in.Items))
	for i, it := range in.Items {
		field := func(k string) string { return "items[" + itoa(i) + "]." + k }
		if pct(it.DiscountPercent) {
			return nil, nil, apperr.Validation(map[string]string{field("discount_percent"): "0-100"})
		}
		epc := ""
		if strings.TrimSpace(it.EPC) != "" {
			n, err := rfid.NormalizeEPC(it.EPC)
			if err != nil {
				return nil, nil, apperr.Validation(map[string]string{field("epc"): "invalid tag"})
			}
			if seenEPC[n] {
				return nil, nil, apperr.Conflict("DUPLICATE_TAG", "The same tag is scanned twice")
			}
			tag, err := q.GetTagByEPC(ctx, gen.GetTagByEPCParams{CompanyID: p.CompanyID, Epc: n})
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, nil, apperr.New(404, "UNKNOWN_TAG", "Tag not found: "+n)
			}
			if err != nil {
				return nil, nil, err
			}
			if !rfid.Sellable(tag.Status) || tag.StoreID == nil || *tag.StoreID != cs.store.ID {
				return nil, nil, apperr.Conflict("TAG_NOT_SELLABLE", "This tag cannot be sold here ("+tag.Status+")").WithDetails(map[string]string{"epc": n, "status": tag.Status})
			}
			seenEPC[n] = true
			epc = n
			it.Qty = decimal.NewFromInt(1)
			vid := tag.VariantID
			it.VariantID = &vid
			it.Barcode = ""
		}
		if !it.Qty.IsPositive() {
			return nil, nil, apperr.Validation(map[string]string{field("qty"): "must be positive"})
		}
		variantID, barcode := uuid.Nil, strings.TrimSpace(it.Barcode)
		switch {
		case it.VariantID != nil:
			variantID = *it.VariantID
		case barcode != "":
			b, err := q.LookupBarcode(ctx, gen.LookupBarcodeParams{CompanyID: p.CompanyID, Barcode: barcode})
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, nil, apperr.New(404, "UNKNOWN_BARCODE", "Barcode not found: "+barcode)
			}
			if err != nil {
				return nil, nil, err
			}
			variantID = b.VariantID
		default:
			return nil, nil, apperr.Validation(map[string]string{field("variant_id"): "variant_id or barcode required"})
		}
		v, err := q.LookupVariantForSale(ctx, gen.LookupVariantForSaleParams{ID: variantID, CompanyID: p.CompanyID})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, apperr.Validation(map[string]string{field("variant_id"): "unknown variant"})
		}
		if err != nil {
			return nil, nil, err
		}
		if v.ProductStatus != "active" || v.VariantStatus != "active" {
			return nil, nil, apperr.Conflict("ITEM_NOT_SELLABLE", v.ProductName+" is not active")
		}
		if v.IsRfidTracked && epc == "" && cs.snap.Bool("rfid.require_tag_at_pos") {
			return nil, nil, apperr.Conflict("TAG_REQUIRED", v.ProductName+" must be sold by scanning its RFID tag")
		}
		if epc != "" {
			ex.epcs[i] = epc
		}
		if !v.AllowsDecimal && !it.Qty.Equal(it.Qty.Truncate(0)) {
			return nil, nil, apperr.Validation(map[string]string{field("qty"): "this item is sold in whole units"})
		}
		if barcode == "" {
			if b, err := q.PrimaryBarcode(ctx, variantID); err == nil {
				barcode = b
			}
		}
		price, ok, err := pricing.ResolveBest(ctx, q, p.CompanyID, cs.store.OrgUnitID, variantID, it.Qty, levelID, now)
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			return nil, nil, apperr.Conflict("NO_PRICE", "No selling price defined for "+v.Sku)
		}
		gross := price.Price.Mul(it.Qty).Round(0)
		rows[i] = row{item: it, variantID: variantID, v: v, barcode: barcode, price: price, gross: gross}
		pl[i] = promotion.Line{No: i + 1, VariantID: variantID, CategoryID: v.CategoryID, BrandID: v.BrandID, Qty: it.Qty, UnitPrice: price.Price}
	}

	// promotions
	promos, err := promotion.Load(ctx, q, p.CompanyID, cs.store.OrgUnitID, now)
	if err != nil {
		return nil, nil, err
	}
	res := promotion.Evaluate(promos, pl, promotion.Context{Now: local, LevelID: levelID})
	quote.Promotions = res.Applied
	if quote.Promotions == nil {
		quote.Promotions = []promotion.Applied{}
	}

	// pass 2: manual + cart discounts on what the promotions left
	maxPct := in.CartDiscountPercent
	net := make([]decimal.Decimal, len(rows))
	hundredV := hundred
	for i, r := range rows {
		promoOff := res.Lines[i].Discount
		remaining := r.gross.Sub(promoOff)
		net[i] = remaining.Mul(hundredV.Sub(r.item.DiscountPercent)).Div(hundredV).Mul(hundredV.Sub(in.CartDiscountPercent)).Div(hundredV).Round(0)
		if r.item.DiscountPercent.GreaterThan(maxPct) {
			maxPct = r.item.DiscountPercent
		}
		ex.promos[i] = res.Lines[i].Applied
	}

	// voucher, allocated across lines in proportion to their net value (remainder on the last line)
	voucherOff := make([]decimal.Decimal, len(rows))
	if code := strings.TrimSpace(in.VoucherCode); code != "" {
		base := decimal.Zero
		for _, n := range net {
			base = base.Add(n)
		}
		v, d, err := voucher.Check(ctx, q, p.CompanyID, code, base, in.CustomerID, now)
		if err != nil {
			return nil, nil, err
		}
		ex.voucher = v
		quote.Voucher = &VoucherInfo{Code: v.Code, Discount: d}
		left := d
		for i := range rows {
			share := d.Mul(net[i]).Div(base).Round(0)
			if i == len(rows)-1 {
				share = left
			}
			share = decimal.Min(share, decimal.Min(left, net[i]))
			voucherOff[i] = share
			left = left.Sub(share)
		}
	}

	for i, r := range rows {
		n := net[i].Sub(voucherOff[i])
		rate := decimal.Zero
		if r.v.TaxCode == "PPN" {
			rate = ppn
		}
		var tax, total decimal.Decimal
		if inclusive {
			tax = n.Mul(rate).Div(hundredV.Add(rate)).Round(0)
			total = n
		} else {
			tax = n.Mul(rate).Div(hundredV).Round(0)
			total = n.Add(tax)
		}
		name := r.v.ProductName
		if r.v.VariantName != "" {
			name += " " + r.v.VariantName
		}
		applied := res.Lines[i].Applied
		if applied == nil {
			applied = []promotion.Applied{}
		}
		quote.Lines = append(quote.Lines, Line{LineNo: i + 1, VariantID: r.variantID, SKU: r.v.Sku, Name: name, Barcode: r.barcode, EPC: ex.epcs[i], Qty: r.item.Qty,
			UnitPrice: r.price.Price, PriceSource: r.price.SourceName, DiscountPercent: r.item.DiscountPercent, Gross: r.gross, PromoDiscount: res.Lines[i].Discount,
			Promotions: applied, VoucherDiscount: voucherOff[i], DiscountAmount: r.gross.Sub(n), TaxRate: rate, TaxAmount: tax, LineTotal: total})
		quote.Subtotal = quote.Subtotal.Add(r.gross)
		quote.DiscountTotal = quote.DiscountTotal.Add(r.gross.Sub(n))
		quote.PromoDiscount = quote.PromoDiscount.Add(res.Lines[i].Discount)
		quote.VoucherDiscount = quote.VoucherDiscount.Add(voucherOff[i])
		quote.TaxTotal = quote.TaxTotal.Add(tax)
		quote.GrandTotal = quote.GrandTotal.Add(total)
	}
	quote.ApprovalRequired = maxPct.GreaterThan(quote.MaxDiscountPercent) && !p.Grants.Can(PermDiscount, cs.term.Path)

	if c := ex.customer; c != nil {
		quote.Customer = &CustomerInfo{ID: c.ID, MemberNo: c.MemberNo, Name: c.Name, Phone: c.Phone, LevelName: c.LevelName, PointsBalance: c.PointsBalance, EarnMultiplier: c.EarnMultiplier}
		quote.PointsEarn = ex.rules.PointsFor(quote.GrandTotal, c.EarnMultiplier)
		quote.MaxRedeemPoints = ex.rules.MaxRedeemable(c.PointsBalance, quote.GrandTotal)
	}
	return quote, ex, nil
}

// Quote prices a cart without saving anything (drives the POS screen).
func (s *Service) Quote(ctx context.Context, in CartInput) (*Quote, error) {
	p := access.MustPrincipal(ctx)
	q := s.tx.Queries()
	cs, err := loadStore(ctx, q, p, PermCreate, in.TerminalID)
	if err != nil {
		return nil, err
	}
	quote, _, err := s.priceCart(ctx, q, p, cs, in)
	return quote, err
}

func itoa(i int) string { return decimal.NewFromInt(int64(i)).String() }
