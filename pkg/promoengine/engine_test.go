package promoengine

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

func d(n int64) decimal.Decimal { return decimal.NewFromInt(n) }

func line(no int, qty, price int64) Line {
	return Line{No: no, VariantID: uuid.New(), Qty: d(qty), UnitPrice: d(price)}
}

func promo(name string, t Type, c Config) Promotion {
	return Promotion{ID: uuid.New(), Code: name, Name: name, Type: t, Config: c, Priority: 100, Stackable: true}
}

var monday = time.Date(2026, 10, 12, 14, 30, 0, 0, time.UTC) // a Monday, 14:30

func total(r Result) int64 { return r.Total.IntPart() }

func TestPercentAndFixed(t *testing.T) {
	lines := []Line{line(1, 2, 50000), line(2, 1, 10000)} // 110000
	r := Evaluate([]Promotion{promo("p20", Percent, Config{Percent: d(20)})}, lines, Context{Now: monday})
	if total(r) != 22000 {
		t.Fatalf("20%% of 110000 = 22000, got %s", r.Total)
	}
	r = Evaluate([]Promotion{promo("f", Fixed, Config{Amount: d(5000)})}, lines, Context{Now: monday}) // per unit: 3 units
	if total(r) != 15000 {
		t.Fatalf("5000/unit x3 = 15000, got %s", r.Total)
	}
	r = Evaluate([]Promotion{promo("o", Fixed, Config{Amount: d(10000), Per: "order"})}, lines, Context{Now: monday})
	if total(r) != 10000 || r.Lines[0].Discount.Add(r.Lines[1].Discount).IntPart() != 10000 {
		t.Fatalf("order-level 10000 spread exactly, got %+v", r)
	}
	// a discount can never exceed the line value
	r = Evaluate([]Promotion{promo("big", Fixed, Config{Amount: d(99999)})}, []Line{line(1, 1, 1000)}, Context{Now: monday})
	if total(r) != 1000 {
		t.Fatalf("capped at line value: %s", r.Total)
	}
}

func TestBuyXGetY(t *testing.T) {
	// buy 2 get 1 free across the whole cart: 3 units (30k, 20k, 10k) -> cheapest (10k) free
	lines := []Line{line(1, 1, 30000), line(2, 1, 20000), line(3, 1, 10000)}
	r := Evaluate([]Promotion{promo("b2g1", BxGy, Config{BuyQty: 2, GetQty: 1})}, lines, Context{Now: monday})
	if total(r) != 10000 || r.Lines[2].Discount.IntPart() != 10000 || r.Lines[0].Discount.Sign() != 0 {
		t.Fatalf("cheapest unit free: %+v", r.Lines)
	}
	// only 2 units: not a full group -> nothing
	if r := Evaluate([]Promotion{promo("b2g1", BxGy, Config{BuyQty: 2, GetQty: 1})}, lines[:2], Context{Now: monday}); total(r) != 0 {
		t.Fatal("incomplete group must not discount")
	}
	// 6 units of one item at 10k: two groups -> 2 free; 50% on the "get" units halves that
	r = Evaluate([]Promotion{promo("b2g1", BxGy, Config{BuyQty: 2, GetQty: 1, GetDiscountPercent: d(50)})}, []Line{line(1, 6, 10000)}, Context{Now: monday})
	if total(r) != 10000 {
		t.Fatalf("2 units at 50%% = 10000, got %s", r.Total)
	}
}

func TestBundleAndThreshold(t *testing.T) {
	// 3 for 100k: 4 units at 40k -> one bundle (120k -> 100k = 20k off), 4th unit full price
	r := Evaluate([]Promotion{promo("3for100", Bundle, Config{Qty: 3, Price: d(100000)})}, []Line{line(1, 4, 40000)}, Context{Now: monday})
	if total(r) != 20000 {
		t.Fatalf("bundle: %s", r.Total)
	}
	th := promo("spend300", Threshold, Config{MinSpend: d(300000), Amount: d(30000)})
	if r := Evaluate([]Promotion{th}, []Line{line(1, 1, 299999)}, Context{Now: monday}); total(r) != 0 {
		t.Fatal("below threshold")
	}
	if r := Evaluate([]Promotion{th}, []Line{line(1, 2, 150000)}, Context{Now: monday}); total(r) != 30000 {
		t.Fatalf("at threshold: %s", r.Total)
	}
	// threshold works on what is left after earlier promos
	p10 := promo("p10", Percent, Config{Percent: d(10)})
	p10.Priority = 1
	r = Evaluate([]Promotion{th, p10}, []Line{line(1, 1, 320000)}, Context{Now: monday}) // 10% -> 288000 < 300000
	if total(r) != 32000 {
		t.Fatalf("threshold must see the post-promo value: %s", r.Total)
	}
}

func TestScopesLevelsAndWindows(t *testing.T) {
	shoes, other := uuid.New(), uuid.New()
	a, b := line(1, 1, 100000), line(2, 1, 100000)
	a.CategoryID, b.CategoryID = &shoes, &other
	p := promo("shoes", Percent, Config{Percent: d(50)})
	p.Scopes.Categories = map[uuid.UUID]bool{shoes: true}
	r := Evaluate([]Promotion{p}, []Line{a, b}, Context{Now: monday})
	if r.Lines[0].Discount.IntPart() != 50000 || r.Lines[1].Discount.Sign() != 0 {
		t.Fatalf("category scope: %+v", r.Lines)
	}

	gold, silver := uuid.New(), uuid.New()
	m := promo("members", Percent, Config{Percent: d(15)})
	m.Scopes.Levels = map[uuid.UUID]bool{gold: true}
	if r := Evaluate([]Promotion{m}, []Line{a}, Context{Now: monday}); total(r) != 0 {
		t.Fatal("no customer -> no member promo")
	}
	if r := Evaluate([]Promotion{m}, []Line{a}, Context{Now: monday, LevelID: &silver}); total(r) != 0 {
		t.Fatal("wrong level")
	}
	if r := Evaluate([]Promotion{m}, []Line{a}, Context{Now: monday, LevelID: &gold}); total(r) != 15000 {
		t.Fatal("gold gets it")
	}

	w := promo("window", Percent, Config{Percent: d(10)})
	start, end := monday.Add(-time.Hour), monday.Add(time.Hour)
	w.StartsAt, w.EndsAt = &start, &end
	if total(Evaluate([]Promotion{w}, []Line{a}, Context{Now: monday})) != 10000 {
		t.Fatal("inside window")
	}
	if total(Evaluate([]Promotion{w}, []Line{a}, Context{Now: end})) != 0 {
		t.Fatal("end is exclusive")
	}
	h := promo("happy", Percent, Config{Percent: d(10)})
	h.DaysOfWeek, h.TimeFrom, h.TimeTo = []int{1}, "14:00", "16:00" // Mondays 14-16
	if total(Evaluate([]Promotion{h}, []Line{a}, Context{Now: monday})) != 10000 {
		t.Fatal("monday 14:30 is in")
	}
	if total(Evaluate([]Promotion{h}, []Line{a}, Context{Now: monday.AddDate(0, 0, 1)})) != 0 {
		t.Fatal("tuesday is out")
	}
	if total(Evaluate([]Promotion{h}, []Line{a}, Context{Now: monday.Add(2 * time.Hour)})) != 0 {
		t.Fatal("16:30 is out")
	}
}

func TestStackingRules(t *testing.T) {
	l := []Line{line(1, 1, 100000)}
	s1 := promo("s1", Percent, Config{Percent: d(10)})
	s1.Priority = 1
	s2 := promo("s2", Percent, Config{Percent: d(10)})
	s2.Priority = 2
	// stackable promotions compound on the remaining value: 10000 + 9000
	if r := Evaluate([]Promotion{s1, s2}, l, Context{Now: monday}); total(r) != 19000 {
		t.Fatalf("stack: %s", r.Total)
	}
	// a non-stackable promotion locks the line against later ones
	ns := promo("ns", Percent, Config{Percent: d(30)})
	ns.Stackable, ns.Priority = false, 1
	if r := Evaluate([]Promotion{ns, s2}, l, Context{Now: monday}); total(r) != 30000 {
		t.Fatalf("non-stackable locks: %s", r.Total)
	}
	// ...and cannot join a line that already has a discount
	ns.Priority = 5
	if r := Evaluate([]Promotion{s1, ns}, l, Context{Now: monday}); total(r) != 10000 {
		t.Fatalf("non-stackable after another: %s", r.Total)
	}
	// exclusive: customer gets the better of "exclusive alone" and "everything stacked"
	ex := promo("ex", Percent, Config{Percent: d(25)})
	ex.Exclusive = true
	if r := Evaluate([]Promotion{s1, s2, ex}, l, Context{Now: monday}); total(r) != 25000 || len(r.Applied) != 1 || r.Applied[0].Name != "ex" {
		t.Fatalf("exclusive wins (25000 > 19000): %+v", r)
	}
	ex.Config.Percent = d(15)
	if r := Evaluate([]Promotion{s1, s2, ex}, l, Context{Now: monday}); total(r) != 19000 {
		t.Fatalf("stack wins (19000 > 15000): %s", r.Total)
	}
}

func TestEvaluateDoesNotMutateInput(t *testing.T) {
	lines := []Line{line(1, 2, 50000)}
	ps := []Promotion{promo("p", Percent, Config{Percent: d(10)})}
	a := Evaluate(ps, lines, Context{Now: monday})
	b := Evaluate(ps, lines, Context{Now: monday})
	if !a.Total.Equal(b.Total) || a.Total.IntPart() != 10000 {
		t.Fatal("must be deterministic")
	}
}
