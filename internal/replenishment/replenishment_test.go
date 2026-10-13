package replenishment

import (
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

func d(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func row(onHand, inTransit, sold, recent string) Row {
	sid := uuid.New()
	return Row{StoreID: &sid, VariantID: uuid.New(), Sku: "S", ProductName: "Kopi", OnHand: d(onHand), InTransit: d(inTransit), Sold: d(sold), SoldRecent: d(recent),
		Moq: d("1"), LeadDays: 3, SupplierPrice: d("10000")}
}

func TestPlanSizesTheOrderFromDemandLeadTimeAndCover(t *testing.T) {
	// 280 sold in 28 days = 10/day; lead 3 + cover 7 = 100 needed; 40 on hand + 20 coming = 60 -> order 40
	r := row("40", "20", "280", "70")
	s, ok := Plan(r, Params{})
	if !ok || s.SuggestedQty.String() != "40" || s.DailyDemand.String() != "10" || s.Target.String() != "100" || s.EstimatedCost.String() != "400000" {
		t.Fatalf("%+v ok=%v", s, ok)
	}
	if s.Urgency != Reorder || s.DaysOfCover == nil || s.DaysOfCover.String() != "4" || s.Trend != "flat" {
		t.Fatalf("urgency %s cover %v trend %s", s.Urgency, s.DaysOfCover, s.Trend)
	}
}

func TestPlanUrgencyBands(t *testing.T) {
	if s, _ := Plan(row("0", "0", "280", "70"), Params{}); s.Urgency != Stockout {
		t.Fatalf("empty shelf that sells: %s", s.Urgency)
	}
	if s, _ := Plan(row("20", "0", "280", "70"), Params{}); s.Urgency != Critical { // 2 days of cover < 3 days lead time
		t.Fatalf("2 days of cover with a 3 day lead time: %s (%v)", s.Urgency, s.DaysOfCover)
	}
	if s, _ := Plan(row("50", "0", "280", "70"), Params{}); s.Urgency != Reorder {
		t.Fatalf("5 days of cover: %s", s.Urgency)
	}
}

func TestPlanNothingToOrderWhenCovered(t *testing.T) {
	if _, ok := Plan(row("100", "0", "280", "70"), Params{}); ok {
		t.Fatal("stock already meets the target")
	}
	if _, ok := Plan(row("0", "100", "280", "70"), Params{}); ok {
		t.Fatal("stock on the way counts")
	}
}

func TestPlanSafetyStockMaxStockAndMOQ(t *testing.T) {
	r := row("0", "0", "28", "7") // 1/day -> 10 + safety 5 = 15
	r.MinStock = d("5")
	r.Moq = d("12")
	s, ok := Plan(r, Params{})
	if !ok || s.SuggestedQty.String() != "24" { // 15 rounded up to a multiple of 12
		t.Fatalf("moq rounding: %+v", s)
	}
	r.MaxStock = d("12") // never plan above the shelf's maximum
	s, _ = Plan(r, Params{})
	if s.Target.String() != "12" || s.SuggestedQty.String() != "12" {
		t.Fatalf("max stock cap: %+v", s)
	}
}

func TestPlanReorderPointWithoutSalesAndTrend(t *testing.T) {
	r := row("2", "0", "0", "0") // never sold in the window, but at its reorder point
	r.ReorderPoint = d("5")
	s, ok := Plan(r, Params{})
	if !ok || s.SuggestedQty.String() != "3" || s.DaysOfCover != nil {
		t.Fatalf("reorder point: %+v ok=%v", s, ok)
	}
	if s, _ := Plan(row("0", "0", "280", "140"), Params{}); s.Trend != "up" { // last week sold double the average
		t.Fatalf("trend %s", s.Trend)
	}
	if s, _ := Plan(row("0", "0", "280", "20"), Params{}); s.Trend != "down" {
		t.Fatalf("trend %s", s.Trend)
	}
}

func TestPlanFallsBackToDefaultLeadAndVariantPrice(t *testing.T) {
	r := row("0", "0", "280", "70")
	r.LeadDays, r.SupplierPrice, r.VariantPrice = 0, d("0"), d("7000")
	s, _ := Plan(r, Params{})
	if s.LeadDays != DefaultLeadDays || s.UnitPrice.String() != "7000" || s.SupplierID != nil {
		t.Fatalf("%+v", s)
	}
}
