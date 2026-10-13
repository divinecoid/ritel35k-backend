package report

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"
	"golang.org/x/sync/errgroup"

	"github.com/ritel35k/backend/db/gen"
	"github.com/ritel35k/backend/internal/access"
	"github.com/ritel35k/backend/internal/platform/tx"
)

const PermDashboard = "dashboard.view"

type Service struct {
	tx  *tx.Manager
	now func() time.Time
}

func NewService(t *tx.Manager) *Service { return &Service{tx: t, now: time.Now} }

// Totals is one period of headline numbers. Cost and profit are zero unless the caller may see profit.
type Totals struct {
	Transactions int64           `json:"transactions"`
	Voided       int64           `json:"voided"`
	ItemsSold    decimal.Decimal `json:"items_sold"`
	GrossSales   decimal.Decimal `json:"gross_sales"`
	Discount     decimal.Decimal `json:"discount"`
	Refund       decimal.Decimal `json:"refund"`
	NetSales     decimal.Decimal `json:"net_sales"`
	AvgBasket    decimal.Decimal `json:"avg_basket"`
	GrossProfit  decimal.Decimal `json:"gross_profit"`
	MarginPct    decimal.Decimal `json:"margin_pct"`
}

type DayPoint struct {
	Date         string          `json:"date"`
	Transactions int64           `json:"transactions"`
	NetSales     decimal.Decimal `json:"net_sales"`
	GrossProfit  decimal.Decimal `json:"gross_profit"`
}

type HourPoint struct {
	Hour         int16           `json:"hour"`
	Transactions int64           `json:"transactions"`
	NetSales     decimal.Decimal `json:"net_sales"`
}

type ProductRow struct {
	VariantID uuid.UUID       `json:"variant_id"`
	SKU       string          `json:"sku"`
	Name      string          `json:"name"`
	Category  string          `json:"category"`
	Qty       decimal.Decimal `json:"qty"`
	NetSales  decimal.Decimal `json:"net_sales"`
	Profit    decimal.Decimal `json:"gross_profit"`
}

type CategoryRow struct {
	Name     string          `json:"name"`
	Qty      decimal.Decimal `json:"qty"`
	NetSales decimal.Decimal `json:"net_sales"`
	SharePct decimal.Decimal `json:"share_pct"`
}

type PaymentRow struct {
	Method   string          `json:"method"`
	Payments int64           `json:"payments"`
	Amount   decimal.Decimal `json:"amount"`
	SharePct decimal.Decimal `json:"share_pct"`
}

type BreakdownRow struct {
	UnitID       uuid.UUID       `json:"unit_id"`
	Code         string          `json:"code"`
	Name         string          `json:"name"`
	Type         string          `json:"type"`
	Path         string          `json:"path"`
	Stores       int64           `json:"stores"`
	Transactions int64           `json:"transactions"`
	NetSales     decimal.Decimal `json:"net_sales"`
	GrossProfit  decimal.Decimal `json:"gross_profit"`
	SharePct     decimal.Decimal `json:"share_pct"`
}

type StockInfo struct {
	StockValue decimal.Decimal `json:"stock_value"`
	LowStock   int64           `json:"low_stock"`
	OutOfStock int64           `json:"out_of_stock"`
}

type Overview struct {
	From           string         `json:"from"`
	To             string         `json:"to"`
	PrevFrom       string         `json:"prev_from"`
	PrevTo         string         `json:"prev_to"`
	Current        Totals         `json:"current"`
	Previous       Totals         `json:"previous"`
	ShowProfit     bool           `json:"show_profit"`
	Trend          []DayPoint     `json:"trend"`
	Hourly         []HourPoint    `json:"hourly"`
	TopProducts    []ProductRow   `json:"top_products"`
	Categories     []CategoryRow  `json:"categories"`
	Payments       []PaymentRow   `json:"payments"`
	Breakdown      []BreakdownRow `json:"breakdown"`
	BreakdownLevel string         `json:"breakdown_level"` // type of the rows: region | store | business_unit …
	Stock          *StockInfo     `json:"stock,omitempty"`
	OpenShifts     *int64         `json:"open_shifts,omitempty"`
	UpdatedAt      time.Time      `json:"updated_at"`
}

func totalsOf(r gen.DashKpiTotalsRow, profit bool) Totals {
	t := Totals{Transactions: r.Transactions, Voided: r.Voided, ItemsSold: r.ItemsSold, GrossSales: r.GrossSales, Discount: r.Discount, Refund: r.Refund, NetSales: r.NetSales}
	if r.Transactions > 0 {
		t.AvgBasket = r.GrandTotal.Div(decimal.NewFromInt(r.Transactions)).Round(0)
	}
	if profit {
		t.GrossProfit = r.GrossProfit
		if r.NetSales.IsPositive() {
			t.MarginPct = r.GrossProfit.Div(r.NetSales).Mul(decimal.NewFromInt(100)).Round(1)
		}
	}
	return t
}

func pct(part, whole decimal.Decimal) decimal.Decimal {
	if !whole.IsPositive() {
		return decimal.Zero
	}
	return part.Div(whole).Mul(decimal.NewFromInt(100)).Round(1)
}

// Overview builds the dashboard for the caller's scope narrowed by f. All reads hit summary tables and run concurrently.
func (s *Service) Overview(ctx context.Context, f Filter) (*Overview, error) {
	p := access.MustPrincipal(ctx)
	sc, ok := scopeOf(p, PermDashboard)
	if !ok {
		return nil, errNoScope
	}
	q := s.tx.Queries()
	if err := resolveStorePath(ctx, q, p, &f); err != nil {
		return nil, err
	}
	profit := showProfit(p, PermDashboard, f)
	pf, pt := f.Previous()
	out := &Overview{From: f.From.Format("2006-01-02"), To: f.To.Format("2006-01-02"), PrevFrom: pf.Format("2006-01-02"), PrevTo: pt.Format("2006-01-02"), ShowProfit: profit,
		Trend: []DayPoint{}, Hourly: []HourPoint{}, TopProducts: []ProductRow{}, Categories: []CategoryRow{}, Payments: []PaymentRow{}, Breakdown: []BreakdownRow{}, UpdatedAt: s.now().UTC()}

	kpi := func(from, to time.Time) gen.DashKpiTotalsParams {
		return gen.DashKpiTotalsParams{CompanyID: p.CompanyID, FromDate: from, ToDate: to, AllScope: sc.All, ScopePaths: sc.Paths, UnderPath: f.UnderPath, StoreID: f.StoreID}
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(4) // a dashboard must not monopolise the connection pool

	g.Go(func() error {
		r, err := q.DashKpiTotals(gctx, kpi(f.From, f.To))
		out.Current = totalsOf(r, profit)
		return err
	})
	g.Go(func() error {
		r, err := q.DashKpiTotals(gctx, kpi(pf, pt))
		out.Previous = totalsOf(r, profit)
		return err
	})
	g.Go(func() error {
		rows, err := q.DashKpiDaily(gctx, gen.DashKpiDailyParams{CompanyID: p.CompanyID, FromDate: f.From, ToDate: f.To, AllScope: sc.All, ScopePaths: sc.Paths, UnderPath: f.UnderPath, StoreID: f.StoreID})
		if err != nil {
			return err
		}
		byDay := map[string]gen.DashKpiDailyRow{}
		for _, r := range rows {
			byDay[r.BusinessDate.Format("2006-01-02")] = r
		}
		for d := f.From; !d.After(f.To); d = d.AddDate(0, 0, 1) { // dense series: days without sales show as zero
			k := d.Format("2006-01-02")
			pt := DayPoint{Date: k}
			if r, ok := byDay[k]; ok {
				pt.Transactions, pt.NetSales = r.Transactions, r.NetSales
				if profit {
					pt.GrossProfit = r.GrossProfit
				}
			}
			out.Trend = append(out.Trend, pt)
		}
		return nil
	})
	g.Go(func() error {
		rows, err := q.DashHourly(gctx, gen.DashHourlyParams{CompanyID: p.CompanyID, FromDate: f.From, ToDate: f.To, AllScope: sc.All, ScopePaths: sc.Paths, UnderPath: f.UnderPath, StoreID: f.StoreID})
		if err != nil {
			return err
		}
		byHour := map[int16]gen.DashHourlyRow{}
		for _, r := range rows {
			byHour[r.Hour] = r
		}
		for h := int16(0); h < 24; h++ {
			hp := HourPoint{Hour: h}
			if r, ok := byHour[h]; ok {
				hp.Transactions, hp.NetSales = r.Transactions, r.NetSales
			}
			out.Hourly = append(out.Hourly, hp)
		}
		return nil
	})
	g.Go(func() error {
		rows, err := q.DashTopProducts(gctx, gen.DashTopProductsParams{CompanyID: p.CompanyID, FromDate: f.From, ToDate: f.To, AllScope: sc.All, ScopePaths: sc.Paths,
			UnderPath: f.UnderPath, StoreID: f.StoreID, OrderBy: "sales", RowLimit: 10})
		if err != nil {
			return err
		}
		for _, r := range rows {
			name := r.ProductName
			if r.VariantName != "" {
				name += " " + r.VariantName
			}
			pr := ProductRow{VariantID: r.VariantID, SKU: r.Sku, Name: name, Category: r.CategoryName, Qty: r.Qty, NetSales: r.NetSales}
			if profit {
				pr.Profit = r.NetSales.Sub(r.Cogs)
			}
			out.TopProducts = append(out.TopProducts, pr)
		}
		return nil
	})
	g.Go(func() error {
		rows, err := q.DashByCategory(gctx, gen.DashByCategoryParams{CompanyID: p.CompanyID, FromDate: f.From, ToDate: f.To, AllScope: sc.All, ScopePaths: sc.Paths, UnderPath: f.UnderPath, StoreID: f.StoreID})
		if err != nil {
			return err
		}
		total := decimal.Zero
		for _, r := range rows {
			total = total.Add(r.NetSales)
		}
		for i, r := range rows {
			if i >= 8 { // keep the chart readable: the tail is not shown on the dashboard (the full list is a report)
				break
			}
			out.Categories = append(out.Categories, CategoryRow{Name: r.CategoryName, Qty: r.Qty, NetSales: r.NetSales, SharePct: pct(r.NetSales, total)})
		}
		return nil
	})
	g.Go(func() error {
		rows, err := q.DashPayments(gctx, gen.DashPaymentsParams{CompanyID: p.CompanyID, FromDate: f.From, ToDate: f.To, AllScope: sc.All, ScopePaths: sc.Paths, UnderPath: f.UnderPath, StoreID: f.StoreID})
		if err != nil {
			return err
		}
		total := decimal.Zero
		for _, r := range rows {
			total = total.Add(r.Amount)
		}
		for _, r := range rows {
			out.Payments = append(out.Payments, PaymentRow{Method: r.Method, Payments: r.Payments, Amount: r.Amount, SharePct: pct(r.Amount, total)})
		}
		return nil
	})
	g.Go(func() error { return s.breakdown(gctx, q, p, sc, f, profit, out) })
	if isc, ok := scopeOf(p, "inventory.view"); ok {
		g.Go(func() error {
			r, err := q.DashStock(gctx, gen.DashStockParams{CompanyID: p.CompanyID, AllScope: isc.All, ScopePaths: isc.Paths, UnderPath: f.UnderPath, StoreID: f.StoreID})
			if err != nil {
				return err
			}
			out.Stock = &StockInfo{StockValue: r.StockValue, LowStock: r.LowStock, OutOfStock: r.OutOfStock}
			return nil
		})
	}
	g.Go(func() error {
		n, err := q.DashOpenShifts(gctx, gen.DashOpenShiftsParams{CompanyID: p.CompanyID, AllScope: sc.All, ScopePaths: sc.Paths, UnderPath: f.UnderPath, StoreID: f.StoreID})
		out.OpenShifts = &n
		return err
	})
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return out, nil
}

// breakdown splits the selected node into its direct children (company → regions, region → stores).
// A single store has no children, so the dashboard shows only its trend.
func (s *Service) breakdown(ctx context.Context, q *gen.Queries, p *access.Principal, sc scopeArgs, f Filter, profit bool, out *Overview) error {
	if f.StoreID != nil {
		return nil
	}
	var node uuid.UUID
	if f.UnderPath == "" {
		root, err := q.GetCompanyRootOrgUnit(ctx, p.CompanyID)
		if err != nil {
			return err
		}
		node = root.ID
	} else {
		u, err := q.GetOrgUnitByPath(ctx, gen.GetOrgUnitByPathParams{CompanyID: p.CompanyID, Path: f.UnderPath})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if u.Type == "store" {
			return nil
		}
		node = u.ID
	}
	rows, err := q.DashKpiByChild(ctx, gen.DashKpiByChildParams{CompanyID: p.CompanyID, FromDate: f.From, ToDate: f.To, NodeID: node, AllScope: sc.All, ScopePaths: sc.Paths})
	if err != nil {
		return err
	}
	total := decimal.Zero
	for _, r := range rows {
		total = total.Add(r.NetSales)
	}
	for _, r := range rows {
		b := BreakdownRow{UnitID: r.UnitID, Code: r.UnitCode, Name: r.UnitName, Type: r.UnitType, Path: r.UnitPath, Stores: r.Stores, Transactions: r.Transactions,
			NetSales: r.NetSales, SharePct: pct(r.NetSales, total)}
		if profit {
			b.GrossProfit = r.GrossProfit
		}
		out.Breakdown = append(out.Breakdown, b)
		out.BreakdownLevel = r.UnitType
	}
	return nil
}
