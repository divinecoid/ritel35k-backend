// Package replenishment suggests what to reorder: demand per SKU and store from recent completed sales, set against
// stock on hand and on the way, the supplier's lead time and the SKU's safety stock. Suggestions are advice, never
// automatic orders — a person turns them into draft purchase orders.
package replenishment

import (
	"context"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/ritel35k/backend/db/gen"
	"github.com/ritel35k/backend/internal/access"
	"github.com/ritel35k/backend/internal/platform/tx"
	"github.com/ritel35k/backend/internal/purchasing"
	"github.com/ritel35k/backend/pkg/apperr"
)

const (
	PermView = "inventory.view"

	DefaultWindowDays = 28
	DefaultCoverDays  = 7
	DefaultLeadDays   = 3
	maxRows           = 2000
)

// Urgency, most urgent first.
const (
	Stockout = "stockout" // nothing on the shelf and it sells
	Critical = "critical" // will run out before an order could arrive
	Reorder  = "reorder"  // below what the lead time and cover days call for
)

var rank = map[string]int{Stockout: 0, Critical: 1, Reorder: 2}

type Params struct {
	WindowDays  int // history used to measure demand
	CoverDays   int // how many days of sales an order should cover once it arrives
	DefaultLead int
}

type Row = gen.ReplenishmentRowsRow

type Suggestion struct {
	StoreID       uuid.UUID        `json:"store_id"`
	StoreCode     string           `json:"store_code"`
	StoreName     string           `json:"store_name"`
	WarehouseID   uuid.UUID        `json:"warehouse_id"`
	WarehouseCode string           `json:"warehouse_code"`
	VariantID     uuid.UUID        `json:"variant_id"`
	SKU           string           `json:"sku"`
	Name          string           `json:"name"`
	OnHand        decimal.Decimal  `json:"on_hand"`
	InTransit     decimal.Decimal  `json:"in_transit"`
	Sold          decimal.Decimal  `json:"sold"`
	DailyDemand   decimal.Decimal  `json:"daily_demand"`
	Trend         string           `json:"trend"` // up | down | flat: last 7 days against the window's average
	DaysOfCover   *decimal.Decimal `json:"days_of_cover"`
	LeadDays      int              `json:"lead_days"`
	ReorderPoint  decimal.Decimal  `json:"reorder_point"`
	Target        decimal.Decimal  `json:"target"`
	SuggestedQty  decimal.Decimal  `json:"suggested_qty"`
	Urgency       string           `json:"urgency"`
	SupplierID    *uuid.UUID       `json:"supplier_id"`
	SupplierName  string           `json:"supplier_name"`
	UnitPrice     decimal.Decimal  `json:"unit_price"`
	EstimatedCost decimal.Decimal  `json:"estimated_cost"`
}

func (p Params) norm() Params {
	if p.WindowDays <= 0 {
		p.WindowDays = DefaultWindowDays
	}
	if p.CoverDays <= 0 {
		p.CoverDays = DefaultCoverDays
	}
	if p.DefaultLead <= 0 {
		p.DefaultLead = DefaultLeadDays
	}
	return p
}

// Plan is the whole decision for one SKU in one store, as a pure function (unit-tested without a database).
//
//	daily  = sold in window / window days
//	target = daily × (lead days + cover days) + safety stock (min_stock)        capped at max_stock when one is set
//	order  = target − (on hand + on the way), rounded UP to the supplier's minimum order quantity
//
// ok=false means nothing needs ordering.
func Plan(r Row, p Params) (Suggestion, bool) {
	p = p.norm()
	window := decimal.NewFromInt(int64(p.WindowDays))
	daily := r.Sold.Div(window)
	lead := int(r.LeadDays)
	if lead <= 0 {
		lead = p.DefaultLead
	}
	target := daily.Mul(decimal.NewFromInt(int64(lead + p.CoverDays))).Add(r.MinStock)
	if r.MaxStock.IsPositive() && target.GreaterThan(r.MaxStock) {
		target = r.MaxStock
	}
	if target.LessThan(r.ReorderPoint) {
		target = r.ReorderPoint
	}
	position := r.OnHand.Add(r.InTransit)
	need := target.Sub(position)

	s := Suggestion{WarehouseID: r.WarehouseID, WarehouseCode: r.WarehouseCode, VariantID: r.VariantID, SKU: r.Sku, StoreCode: r.StoreCode, StoreName: r.StoreName,
		OnHand: r.OnHand, InTransit: r.InTransit, Sold: r.Sold, DailyDemand: daily.Round(2), LeadDays: lead, ReorderPoint: r.ReorderPoint, Target: target.Round(0)}
	if r.StoreID != nil {
		s.StoreID = *r.StoreID
	}
	s.Name = r.ProductName
	if r.VariantName != "" {
		s.Name += " " + r.VariantName
	}
	s.Trend = "flat"
	if daily.IsPositive() {
		recent := r.SoldRecent.Div(decimal.NewFromInt(7))
		switch {
		case recent.GreaterThan(daily.Mul(decimal.RequireFromString("1.25"))):
			s.Trend = "up"
		case recent.LessThan(daily.Mul(decimal.RequireFromString("0.75"))):
			s.Trend = "down"
		}
		cover := r.OnHand.Div(daily).Round(1)
		if cover.IsNegative() {
			cover = decimal.Zero
		}
		s.DaysOfCover = &cover
	}
	if !need.IsPositive() {
		return s, false
	}
	moq := r.Moq
	if !moq.IsPositive() {
		moq = decimal.NewFromInt(1)
	}
	qty := need.Div(moq).Ceil().Mul(moq)
	s.SuggestedQty = qty
	switch {
	case !r.OnHand.IsPositive() && daily.IsPositive():
		s.Urgency = Stockout
	case s.DaysOfCover != nil && s.DaysOfCover.LessThan(decimal.NewFromInt(int64(lead))):
		s.Urgency = Critical
	default:
		s.Urgency = Reorder
	}
	s.UnitPrice = r.SupplierPrice
	if !s.UnitPrice.IsPositive() {
		s.UnitPrice = r.VariantPrice
	}
	if r.SupplierID != uuid.Nil {
		id := r.SupplierID
		s.SupplierID, s.SupplierName = &id, r.SupplierName
	}
	s.EstimatedCost = qty.Mul(s.UnitPrice).Round(0)
	return s, true
}

type Service struct {
	tx  *tx.Manager
	po  *purchasing.Service
	now func() time.Time
}

func NewService(t *tx.Manager, po *purchasing.Service) *Service {
	return &Service{tx: t, po: po, now: time.Now}
}

type Filter struct {
	StoreID *uuid.UUID
	Params
}

type Result struct {
	WindowDays  int             `json:"window_days"`
	CoverDays   int             `json:"cover_days"`
	From        string          `json:"from"`
	To          string          `json:"to"`
	Suggestions []Suggestion    `json:"suggestions"`
	TotalCost   decimal.Decimal `json:"total_cost"`
}

func (s *Service) Suggest(ctx context.Context, f Filter) (*Result, error) {
	p := access.MustPrincipal(ctx)
	sc := p.Grants.Scope(PermView)
	if sc.Empty() {
		return nil, apperr.Forbidden("no access to inventory")
	}
	par := f.Params.norm()
	if par.WindowDays > 180 || par.CoverDays > 90 {
		return nil, apperr.Validation(map[string]string{"window_days": "at most 180 days of history and 90 days of cover"})
	}
	all, paths := sc.SQL()
	to := s.now()
	from := to.AddDate(0, 0, -par.WindowDays)
	rows, err := s.tx.Queries().ReplenishmentRows(ctx, gen.ReplenishmentRowsParams{CompanyID: p.CompanyID, AllScope: all, ScopePaths: paths, StoreID: f.StoreID,
		FromDate: from, ToDate: to, RecentFrom: to.AddDate(0, 0, -6), RowLimit: maxRows})
	if err != nil {
		return nil, err
	}
	out := &Result{WindowDays: par.WindowDays, CoverDays: par.CoverDays, From: from.Format("2006-01-02"), To: to.Format("2006-01-02"), Suggestions: []Suggestion{}}
	for _, r := range rows {
		if sg, ok := Plan(r, par); ok {
			out.Suggestions = append(out.Suggestions, sg)
			out.TotalCost = out.TotalCost.Add(sg.EstimatedCost)
		}
	}
	sort.SliceStable(out.Suggestions, func(i, j int) bool {
		a, b := out.Suggestions[i], out.Suggestions[j]
		if rank[a.Urgency] != rank[b.Urgency] {
			return rank[a.Urgency] < rank[b.Urgency]
		}
		return a.EstimatedCost.GreaterThan(b.EstimatedCost)
	})
	return out, nil
}

type DraftLine struct {
	WarehouseID uuid.UUID       `json:"warehouse_id" validate:"required"`
	SupplierID  uuid.UUID       `json:"supplier_id" validate:"required"`
	VariantID   uuid.UUID       `json:"variant_id" validate:"required"`
	Qty         decimal.Decimal `json:"qty"`
}

type DraftInput struct {
	Lines []DraftLine `json:"lines" validate:"required,min=1,max=500,dive"`
}

type Draft struct {
	ID             uuid.UUID       `json:"id"`
	DocumentNumber string          `json:"document_number"`
	SupplierID     uuid.UUID       `json:"supplier_id"`
	WarehouseID    uuid.UUID       `json:"warehouse_id"`
	Lines          int             `json:"lines"`
	Total          decimal.Decimal `json:"total"`
}

// DraftPOs turns chosen suggestions into DRAFT purchase orders, one per (supplier, warehouse). Drafts still need
// the normal submit/approve flow; nothing is ordered from here.
func (s *Service) DraftPOs(ctx context.Context, in DraftInput) ([]Draft, error) {
	type key struct{ sup, wh uuid.UUID }
	groups := map[key][]purchasing.POLineInput{}
	var order []key
	for _, l := range in.Lines {
		if !l.Qty.IsPositive() {
			return nil, apperr.Validation(map[string]string{"qty": "must be positive"})
		}
		k := key{l.SupplierID, l.WarehouseID}
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], purchasing.POLineInput{VariantID: l.VariantID, Qty: l.Qty})
	}
	var out []Draft
	for _, k := range order {
		po, err := s.po.CreatePO(ctx, purchasing.POInput{SupplierID: k.sup, WarehouseID: k.wh, Note: "Dibuat dari saran pengisian stok", Lines: groups[k]})
		if err != nil {
			return out, err
		}
		out = append(out, Draft{ID: po.ID, DocumentNumber: po.DocumentNumber, SupplierID: k.sup, WarehouseID: k.wh, Lines: len(po.Lines), Total: po.Total})
	}
	return out, nil
}
