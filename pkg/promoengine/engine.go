// Package promoengine holds the promotion engine: a PURE function from (cart, context, promotions) to
// per-line discounts. It touches no database and no clock, so it is exhaustively unit-testable and can run
// unchanged at the edge for offline POS.
//
// Stacking rules (documented because they decide real money):
//   - Promotions run in ascending `priority`.
//   - A stackable promotion can combine with others on the same line; a non-stackable one locks the line
//     (nothing else may discount it) and can only apply to lines that have no discount yet.
//   - An exclusive promotion cannot combine with anything. The engine computes the stacked result of all
//     non-exclusive promotions AND each exclusive promotion alone, then gives the customer whichever is larger.
//   - Percent / threshold promotions discount the line's REMAINING value (after earlier promotions).
package promoengine

import (
	"encoding/json"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

type Type string

const (
	Percent   Type = "percent"
	Fixed     Type = "fixed"
	BxGy      Type = "bxgy"
	Bundle    Type = "bundle"
	Threshold Type = "threshold"
)

// Config is the union of all type-specific settings; unused fields stay zero.
type Config struct {
	Percent            decimal.Decimal `json:"percent"`              // percent, threshold (alt. to amount)
	Amount             decimal.Decimal `json:"amount"`               // fixed, threshold
	Per                string          `json:"per"`                  // fixed: "unit" (default) | "order"
	BuyQty             int             `json:"buy_qty"`              // bxgy
	GetQty             int             `json:"get_qty"`              // bxgy
	GetDiscountPercent decimal.Decimal `json:"get_discount_percent"` // bxgy (default 100 = free)
	Qty                int             `json:"qty"`                  // bundle size
	Price              decimal.Decimal `json:"price"`                // bundle price
	MinSpend           decimal.Decimal `json:"min_spend"`            // threshold
}

type Scopes struct {
	Products, Categories, Brands, Levels map[uuid.UUID]bool
}

type Promotion struct {
	ID         uuid.UUID
	Code, Name string
	Type       Type
	Config     Config
	Priority   int
	Stackable  bool
	Exclusive  bool
	StartsAt   *time.Time
	EndsAt     *time.Time
	DaysOfWeek []int  // 0=Sunday; empty = every day
	TimeFrom   string // "15:04"; empty = whole day
	TimeTo     string
	Scopes     Scopes
}

type Line struct {
	No         int
	VariantID  uuid.UUID
	CategoryID *uuid.UUID
	BrandID    *uuid.UUID
	Qty        decimal.Decimal
	UnitPrice  decimal.Decimal
}

type Context struct {
	Now     time.Time // in the store's local time (days/time-of-day are evaluated against it)
	LevelID *uuid.UUID
}

type Applied struct {
	PromotionID uuid.UUID       `json:"promotion_id"`
	Name        string          `json:"name"`
	Amount      decimal.Decimal `json:"amount"`
}

type LineResult struct {
	No       int             `json:"line_no"`
	Discount decimal.Decimal `json:"discount"`
	Applied  []Applied       `json:"applied"`
}

type Result struct {
	Lines   []LineResult    `json:"lines"`
	Total   decimal.Decimal `json:"total"`
	Applied []Applied       `json:"applied"` // per promotion, summed over lines
}

func (c Config) MarshalJSON() ([]byte, error) { type alias Config; return json.Marshal(alias(c)) }

// Active reports whether the promotion is in its validity window at ctx.Now.
func (p Promotion) Active(now time.Time) bool {
	if p.StartsAt != nil && now.Before(*p.StartsAt) {
		return false
	}
	if p.EndsAt != nil && !now.Before(*p.EndsAt) {
		return false
	}
	if len(p.DaysOfWeek) > 0 {
		ok := false
		for _, d := range p.DaysOfWeek {
			if int(now.Weekday()) == d {
				ok = true
			}
		}
		if !ok {
			return false
		}
	}
	if p.TimeFrom != "" && p.TimeTo != "" {
		hm := now.Format("15:04")
		if p.TimeFrom <= p.TimeTo {
			if hm < p.TimeFrom || hm >= p.TimeTo {
				return false
			}
		} else if hm < p.TimeFrom && hm >= p.TimeTo { // overnight window, e.g. 22:00-02:00
			return false
		}
	}
	return true
}

func (p Promotion) appliesTo(l Line) bool {
	s := p.Scopes
	if len(s.Products)+len(s.Categories)+len(s.Brands) == 0 {
		return true
	}
	if s.Products[l.VariantID] {
		return true
	}
	if l.CategoryID != nil && s.Categories[*l.CategoryID] {
		return true
	}
	return l.BrandID != nil && s.Brands[*l.BrandID]
}

func (p Promotion) levelOK(ctx Context) bool {
	if len(p.Scopes.Levels) == 0 {
		return true
	}
	return ctx.LevelID != nil && p.Scopes.Levels[*ctx.LevelID]
}

type lineState struct {
	line     Line
	gross    decimal.Decimal
	discount decimal.Decimal
	locked   bool // a non-stackable promotion has been applied
	count    int
	applied  []Applied
}

func (s *lineState) remaining() decimal.Decimal { return s.gross.Sub(s.discount) }

func newStates(lines []Line) []*lineState {
	out := make([]*lineState, len(lines))
	for i, l := range lines {
		out[i] = &lineState{line: l, gross: l.UnitPrice.Mul(l.Qty).Round(0)}
	}
	return out
}

// grant records `amount` against a line, capped by what is left on it.
func (s *lineState) grant(p Promotion, amount decimal.Decimal) decimal.Decimal {
	amount = decimal.Min(amount.Round(0), s.remaining())
	if !amount.IsPositive() {
		return decimal.Zero
	}
	s.discount = s.discount.Add(amount)
	s.count++
	if !p.Stackable {
		s.locked = true
	}
	s.applied = append(s.applied, Applied{PromotionID: p.ID, Name: p.Name, Amount: amount})
	return amount
}

func eligible(p Promotion, st []*lineState) []*lineState {
	var out []*lineState
	for _, s := range st {
		if s.locked || !p.appliesTo(s.line) {
			continue
		}
		if !p.Stackable && s.count > 0 {
			continue
		}
		out = append(out, s)
	}
	return out
}

func spread(p Promotion, lines []*lineState, total decimal.Decimal) {
	sum := decimal.Zero
	for _, s := range lines {
		sum = sum.Add(s.remaining())
	}
	if !sum.IsPositive() || !total.IsPositive() {
		return
	}
	total = decimal.Min(total, sum)
	left := total
	for i, s := range lines {
		share := total.Mul(s.remaining()).Div(sum).Round(0)
		if i == len(lines)-1 {
			share = left
		}
		share = decimal.Min(share, left)
		left = left.Sub(s.grant(p, share))
	}
}

type unit struct {
	st    *lineState
	price decimal.Decimal
}

func units(lines []*lineState) []unit {
	var out []unit
	for _, s := range lines {
		n := s.line.Qty.IntPart()
		if n <= 0 || s.remaining().IsZero() {
			continue
		}
		per := s.remaining().Div(s.line.Qty) // remaining value per unit
		for i := int64(0); i < n; i++ {
			out = append(out, unit{s, per})
		}
	}
	return out
}

func apply(p Promotion, st []*lineState) {
	el := eligible(p, st)
	if len(el) == 0 {
		return
	}
	c := p.Config
	hundred := decimal.NewFromInt(100)
	switch p.Type {
	case Percent:
		for _, s := range el {
			s.grant(p, s.remaining().Mul(c.Percent).Div(hundred))
		}
	case Fixed:
		if c.Per == "order" {
			spread(p, el, c.Amount)
			return
		}
		for _, s := range el {
			s.grant(p, c.Amount.Mul(s.line.Qty))
		}
	case BxGy:
		if c.BuyQty <= 0 || c.GetQty <= 0 {
			return
		}
		pct := c.GetDiscountPercent
		if pct.IsZero() {
			pct = hundred
		}
		us := units(el)
		sort.SliceStable(us, func(i, j int) bool { return us[i].price.LessThan(us[j].price) })
		free := (len(us) / (c.BuyQty + c.GetQty)) * c.GetQty
		per := map[*lineState]decimal.Decimal{}
		for i := 0; i < free; i++ {
			per[us[i].st] = per[us[i].st].Add(us[i].price.Mul(pct).Div(hundred))
		}
		for _, s := range el {
			if d, ok := per[s]; ok {
				s.grant(p, d)
			}
		}
	case Bundle:
		if c.Qty <= 0 || !c.Price.IsPositive() {
			return
		}
		us := units(el)
		sort.SliceStable(us, func(i, j int) bool { return us[i].price.GreaterThan(us[j].price) })
		per := map[*lineState]decimal.Decimal{}
		for i := 0; i+c.Qty <= len(us); i += c.Qty {
			group := us[i : i+c.Qty]
			sum := decimal.Zero
			for _, u := range group {
				sum = sum.Add(u.price)
			}
			off := sum.Sub(c.Price)
			if !off.IsPositive() {
				continue
			}
			for _, u := range group {
				per[u.st] = per[u.st].Add(off.Mul(u.price).Div(sum))
			}
		}
		for _, s := range el {
			if d, ok := per[s]; ok {
				s.grant(p, d)
			}
		}
	case Threshold:
		sum := decimal.Zero
		for _, s := range el {
			sum = sum.Add(s.remaining())
		}
		if sum.LessThan(c.MinSpend) {
			return
		}
		off := c.Amount
		if c.Percent.IsPositive() {
			off = sum.Mul(c.Percent).Div(hundred)
		}
		spread(p, el, off)
	}
}

func result(lines []Line, st []*lineState) Result {
	r := Result{Lines: make([]LineResult, len(st))}
	byPromo := map[uuid.UUID]*Applied{}
	var order []uuid.UUID
	for i, s := range st {
		r.Lines[i] = LineResult{No: lines[i].No, Discount: s.discount, Applied: s.applied}
		r.Total = r.Total.Add(s.discount)
		for _, a := range s.applied {
			if t, ok := byPromo[a.PromotionID]; ok {
				t.Amount = t.Amount.Add(a.Amount)
			} else {
				cp := a
				byPromo[a.PromotionID] = &cp
				order = append(order, a.PromotionID)
			}
		}
	}
	for _, id := range order {
		r.Applied = append(r.Applied, *byPromo[id])
	}
	return r
}

// Evaluate applies promotions to a cart. Input slices are not modified.
func Evaluate(promos []Promotion, lines []Line, ctx Context) Result {
	var stack, exclusive []Promotion
	for _, p := range promos {
		if !p.Active(ctx.Now) || !p.levelOK(ctx) {
			continue
		}
		if p.Exclusive {
			exclusive = append(exclusive, p)
		} else {
			stack = append(stack, p)
		}
	}
	less := func(a []Promotion) func(i, j int) bool {
		return func(i, j int) bool {
			if a[i].Priority != a[j].Priority {
				return a[i].Priority < a[j].Priority
			}
			return a[i].Code < a[j].Code
		}
	}
	sort.SliceStable(stack, less(stack))
	sort.SliceStable(exclusive, less(exclusive))

	st := newStates(lines)
	for _, p := range stack {
		apply(p, st)
	}
	best := result(lines, st)
	for _, p := range exclusive {
		alone := newStates(lines)
		apply(p, alone)
		if r := result(lines, alone); r.Total.GreaterThan(best.Total) {
			best = r
		}
	}
	return best
}
