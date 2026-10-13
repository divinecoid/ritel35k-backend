package report

import (
	"context"
	"fmt"
	"time"

	"github.com/shopspring/decimal"

	"github.com/ritel35k/backend/db/gen"
	"github.com/ritel35k/backend/internal/access"
	"github.com/ritel35k/backend/pkg/apperr"
)

const (
	PermSales     = "report.sales.view"
	PermInventory = "report.inventory.view"

	DefaultLimit = 500
	MaxLimit     = 5000
)

// Column types drive both the UI formatter and CSV escaping.
const (
	TText     = "text"
	TDate     = "date"
	TDateTime = "datetime"
	TMoney    = "money"
	TInt      = "int"
	TQty      = "qty"
	TPercent  = "percent"
)

type Column struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Type  string `json:"type"`
}

// Table is the uniform report result: the screen, the CSV export and any future PDF all render the same shape.
type Table struct {
	Report      string    `json:"report"`
	Title       string    `json:"title"`
	From        string    `json:"from,omitempty"`
	To          string    `json:"to,omitempty"`
	Columns     []Column  `json:"columns"`
	Rows        [][]any   `json:"rows"`
	Totals      []any     `json:"totals,omitempty"`
	Truncated   bool      `json:"truncated"`
	ShowProfit  bool      `json:"show_profit"`
	GeneratedAt time.Time `json:"generated_at"`
}

type Options struct {
	Limit        int
	Order        string // sales | qty (product reports)
	OnlyVariance bool   // shift report
}

type Def struct {
	Key         string `json:"key"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Group       string `json:"group"`
	Perm        string `json:"perm"`
	UsesDates   bool   `json:"uses_dates"`
	run         func(ctx context.Context, s *Service, p *access.Principal, sc scopeArgs, f Filter, o Options, profit bool) (*Table, error)
}

var catalog = []Def{
	{Key: "sales_by_day", Title: "Penjualan per hari", Description: "Omzet, diskon, retur dan rata-rata transaksi per tanggal", Group: "Penjualan", Perm: PermSales, UsesDates: true, run: runSalesByDay},
	{Key: "sales_by_month", Title: "Penjualan per bulan", Description: "Ringkasan bulanan", Group: "Penjualan", Perm: PermSales, UsesDates: true, run: runSalesByMonth},
	{Key: "sales_by_store", Title: "Penjualan per toko", Description: "Bandingkan toko dalam periode yang sama", Group: "Penjualan", Perm: PermSales, UsesDates: true, run: runSalesByStore},
	{Key: "sales_by_product", Title: "Penjualan per produk", Description: "Produk terlaris menurut omzet atau jumlah", Group: "Penjualan", Perm: PermSales, UsesDates: true, run: runSalesByProduct},
	{Key: "sales_by_category", Title: "Penjualan per kategori", Description: "Kontribusi tiap kategori produk", Group: "Penjualan", Perm: PermSales, UsesDates: true, run: runSalesByCategory},
	{Key: "payments", Title: "Metode pembayaran", Description: "Jumlah dan nilai per metode bayar", Group: "Penjualan", Perm: PermSales, UsesDates: true, run: runPayments},
	{Key: "hourly", Title: "Jam ramai", Description: "Transaksi dan omzet per jam", Group: "Penjualan", Perm: PermSales, UsesDates: true, run: runHourly},
	{Key: "shift_variance", Title: "Selisih kas shift", Description: "Shift yang ditutup, kas seharusnya vs dihitung", Group: "Kasir", Perm: PermSales, UsesDates: true, run: runShiftVariance},
	{Key: "inventory_valuation", Title: "Nilai persediaan", Description: "Nilai stok saat ini per toko (rata-rata bergerak)", Group: "Stok", Perm: PermInventory, run: runValuation},
	{Key: "low_stock", Title: "Stok menipis", Description: "SKU di bawah titik pesan ulang beserta saran order", Group: "Stok", Perm: PermInventory, run: runLowStock},
}

func lookup(key string) (Def, bool) {
	for _, d := range catalog {
		if d.Key == key {
			return d, true
		}
	}
	return Def{}, false
}

// Catalog lists the reports the caller may run.
func (s *Service) Catalog(ctx context.Context) []Def {
	p := access.MustPrincipal(ctx)
	out := []Def{}
	for _, d := range catalog {
		if p.Grants.Has(d.Perm) {
			out = append(out, d)
		}
	}
	return out
}

// Run executes one report for the caller. Rows are limited to Options.Limit (Truncated tells the UI).
func (s *Service) Run(ctx context.Context, key string, f Filter, o Options) (*Table, error) {
	p := access.MustPrincipal(ctx)
	def, ok := lookup(key)
	if !ok {
		return nil, apperr.NotFound("report")
	}
	sc, has := scopeOf(p, def.Perm)
	if !has {
		return nil, apperr.Forbidden("You do not have permission to run this report")
	}
	if o.Limit <= 0 {
		o.Limit = DefaultLimit
	}
	if o.Limit > MaxLimit {
		o.Limit = MaxLimit
	}
	if err := resolveStorePath(ctx, s.tx.Queries(), p, &f); err != nil {
		return nil, err
	}
	profit := showProfit(p, def.Perm, f)
	t, err := def.run(ctx, s, p, sc, f, o, profit)
	if err != nil {
		return nil, err
	}
	t.Report, t.Title, t.ShowProfit, t.GeneratedAt = def.Key, def.Title, profit, s.now().UTC()
	if def.UsesDates {
		t.From, t.To = f.From.Format("2006-01-02"), f.To.Format("2006-01-02")
	}
	if t.Rows == nil {
		t.Rows = [][]any{}
	}
	return t, nil
}

// ---- helpers shared by the sales reports

var d100 = decimal.NewFromInt(100)

func kpiColumns(first []Column, profit bool) []Column {
	cols := append([]Column{}, first...)
	cols = append(cols,
		Column{"transactions", "Transaksi", TInt}, Column{"items", "Item terjual", TQty}, Column{"gross", "Penjualan kotor", TMoney},
		Column{"discount", "Diskon", TMoney}, Column{"refund", "Retur", TMoney}, Column{"net", "Penjualan bersih", TMoney}, Column{"avg", "Rata-rata/transaksi", TMoney})
	if profit {
		cols = append(cols, Column{"cogs", "HPP", TMoney}, Column{"profit", "Laba kotor", TMoney}, Column{"margin", "Margin %", TPercent})
	}
	return cols
}

type kpiAcc struct {
	tx                                        int64
	items, gross, disc, refund, net, cogs, gp decimal.Decimal
}

func (a *kpiAcc) add(tx int64, items, gross, disc, refund, net, cogs, gp decimal.Decimal) {
	a.tx += tx
	a.items, a.gross, a.disc, a.refund = a.items.Add(items), a.gross.Add(gross), a.disc.Add(disc), a.refund.Add(refund)
	a.net, a.cogs, a.gp = a.net.Add(net), a.cogs.Add(cogs), a.gp.Add(gp)
}

func (a kpiAcc) cells(profit bool) []any {
	avg := decimal.Zero
	if a.tx > 0 {
		avg = a.net.Div(decimal.NewFromInt(a.tx)).Round(0)
	}
	out := []any{a.tx, a.items, a.gross, a.disc, a.refund, a.net, avg}
	if profit {
		out = append(out, a.cogs, a.gp, pct(a.gp, a.net))
	}
	return out
}

func blanks(n int) []any { return make([]any, n) }

// withTotals appends a totals row whose leading label cell is "Total".
func withTotals(t *Table, lead int, a kpiAcc, profit bool) {
	row := blanks(lead)
	row[0] = "Total"
	t.Totals = append(row, a.cells(profit)...)
}

func baseArgs(p *access.Principal, sc scopeArgs, f Filter) (gen.DashKpiDailyParams, gen.DashKpiByMonthParams, gen.DashKpiByStoreParams) {
	return gen.DashKpiDailyParams{CompanyID: p.CompanyID, FromDate: f.From, ToDate: f.To, AllScope: sc.All, ScopePaths: sc.Paths, UnderPath: f.UnderPath, StoreID: f.StoreID},
		gen.DashKpiByMonthParams{CompanyID: p.CompanyID, FromDate: f.From, ToDate: f.To, AllScope: sc.All, ScopePaths: sc.Paths, UnderPath: f.UnderPath, StoreID: f.StoreID},
		gen.DashKpiByStoreParams{CompanyID: p.CompanyID, FromDate: f.From, ToDate: f.To, AllScope: sc.All, ScopePaths: sc.Paths, UnderPath: f.UnderPath, StoreID: f.StoreID}
}

func runSalesByDay(ctx context.Context, s *Service, p *access.Principal, sc scopeArgs, f Filter, _ Options, profit bool) (*Table, error) {
	a, _, _ := baseArgs(p, sc, f)
	rows, err := s.tx.Queries().DashKpiDaily(ctx, a)
	if err != nil {
		return nil, err
	}
	t := &Table{Columns: kpiColumns([]Column{{"date", "Tanggal", TDate}}, profit)}
	var tot kpiAcc
	for _, r := range rows {
		var acc kpiAcc
		acc.add(r.Transactions, r.ItemsSold, r.GrossSales, r.Discount, r.Refund, r.NetSales, r.Cogs, r.GrossProfit)
		tot.add(r.Transactions, r.ItemsSold, r.GrossSales, r.Discount, r.Refund, r.NetSales, r.Cogs, r.GrossProfit)
		t.Rows = append(t.Rows, append([]any{r.BusinessDate.Format("2006-01-02")}, acc.cells(profit)...))
	}
	withTotals(t, 1, tot, profit)
	return t, nil
}

func runSalesByMonth(ctx context.Context, s *Service, p *access.Principal, sc scopeArgs, f Filter, _ Options, profit bool) (*Table, error) {
	_, a, _ := baseArgs(p, sc, f)
	rows, err := s.tx.Queries().DashKpiByMonth(ctx, a)
	if err != nil {
		return nil, err
	}
	t := &Table{Columns: kpiColumns([]Column{{"month", "Bulan", TText}}, profit)}
	var tot kpiAcc
	for _, r := range rows {
		var acc kpiAcc
		acc.add(r.Transactions, r.ItemsSold, r.GrossSales, r.Discount, r.Refund, r.NetSales, r.Cogs, r.GrossProfit)
		tot.add(r.Transactions, r.ItemsSold, r.GrossSales, r.Discount, r.Refund, r.NetSales, r.Cogs, r.GrossProfit)
		t.Rows = append(t.Rows, append([]any{r.Month.Format("2006-01")}, acc.cells(profit)...))
	}
	withTotals(t, 1, tot, profit)
	return t, nil
}

func runSalesByStore(ctx context.Context, s *Service, p *access.Principal, sc scopeArgs, f Filter, _ Options, profit bool) (*Table, error) {
	_, _, a := baseArgs(p, sc, f)
	rows, err := s.tx.Queries().DashKpiByStore(ctx, a)
	if err != nil {
		return nil, err
	}
	t := &Table{Columns: kpiColumns([]Column{{"code", "Kode", TText}, {"store", "Toko", TText}}, profit)}
	var tot kpiAcc
	for _, r := range rows {
		var acc kpiAcc
		acc.add(r.Transactions, r.ItemsSold, r.GrossSales, r.Discount, r.Refund, r.NetSales, r.Cogs, r.GrossProfit)
		tot.add(r.Transactions, r.ItemsSold, r.GrossSales, r.Discount, r.Refund, r.NetSales, r.Cogs, r.GrossProfit)
		t.Rows = append(t.Rows, append([]any{r.StoreCode, r.StoreName}, acc.cells(profit)...))
	}
	withTotals(t, 2, tot, profit)
	return t, nil
}

func runSalesByProduct(ctx context.Context, s *Service, p *access.Principal, sc scopeArgs, f Filter, o Options, profit bool) (*Table, error) {
	order := "sales"
	if o.Order == "qty" {
		order = "qty"
	}
	rows, err := s.tx.Queries().DashTopProducts(ctx, gen.DashTopProductsParams{CompanyID: p.CompanyID, FromDate: f.From, ToDate: f.To, AllScope: sc.All, ScopePaths: sc.Paths,
		UnderPath: f.UnderPath, StoreID: f.StoreID, OrderBy: order, RowLimit: int32(o.Limit + 1)})
	if err != nil {
		return nil, err
	}
	cols := []Column{{"sku", "SKU", TText}, {"product", "Produk", TText}, {"category", "Kategori", TText}, {"qty", "Qty", TQty}, {"net", "Penjualan bersih", TMoney}, {"avg", "Harga rata-rata", TMoney}}
	if profit {
		cols = append(cols, Column{"cogs", "HPP", TMoney}, Column{"profit", "Laba kotor", TMoney}, Column{"margin", "Margin %", TPercent})
	}
	t := &Table{Columns: cols}
	if len(rows) > o.Limit {
		rows, t.Truncated = rows[:o.Limit], true
	}
	var qty, net, cogs decimal.Decimal
	for _, r := range rows {
		name := r.ProductName
		if r.VariantName != "" {
			name += " " + r.VariantName
		}
		avg := decimal.Zero
		if r.Qty.IsPositive() {
			avg = r.NetSales.Div(r.Qty).Round(0)
		}
		row := []any{r.Sku, name, r.CategoryName, r.Qty, r.NetSales, avg}
		if profit {
			row = append(row, r.Cogs, r.NetSales.Sub(r.Cogs), pct(r.NetSales.Sub(r.Cogs), r.NetSales))
		}
		t.Rows = append(t.Rows, row)
		qty, net, cogs = qty.Add(r.Qty), net.Add(r.NetSales), cogs.Add(r.Cogs)
	}
	tot := []any{"Total", "", "", qty, net, ""}
	if profit {
		tot = append(tot, cogs, net.Sub(cogs), pct(net.Sub(cogs), net))
	}
	t.Totals = tot
	return t, nil
}

func runSalesByCategory(ctx context.Context, s *Service, p *access.Principal, sc scopeArgs, f Filter, _ Options, profit bool) (*Table, error) {
	rows, err := s.tx.Queries().DashByCategory(ctx, gen.DashByCategoryParams{CompanyID: p.CompanyID, FromDate: f.From, ToDate: f.To, AllScope: sc.All, ScopePaths: sc.Paths, UnderPath: f.UnderPath, StoreID: f.StoreID})
	if err != nil {
		return nil, err
	}
	cols := []Column{{"category", "Kategori", TText}, {"qty", "Qty", TQty}, {"net", "Penjualan bersih", TMoney}, {"share", "Kontribusi %", TPercent}}
	if profit {
		cols = append(cols, Column{"cogs", "HPP", TMoney}, Column{"profit", "Laba kotor", TMoney}, Column{"margin", "Margin %", TPercent})
	}
	t := &Table{Columns: cols}
	var qty, net, cogs decimal.Decimal
	for _, r := range rows {
		net = net.Add(r.NetSales)
	}
	for _, r := range rows {
		row := []any{r.CategoryName, r.Qty, r.NetSales, pct(r.NetSales, net)}
		if profit {
			row = append(row, r.Cogs, r.NetSales.Sub(r.Cogs), pct(r.NetSales.Sub(r.Cogs), r.NetSales))
		}
		t.Rows = append(t.Rows, row)
		qty, cogs = qty.Add(r.Qty), cogs.Add(r.Cogs)
	}
	tot := []any{"Total", qty, net, d100}
	if profit {
		tot = append(tot, cogs, net.Sub(cogs), pct(net.Sub(cogs), net))
	}
	t.Totals = tot
	return t, nil
}

func runPayments(ctx context.Context, s *Service, p *access.Principal, sc scopeArgs, f Filter, _ Options, _ bool) (*Table, error) {
	rows, err := s.tx.Queries().DashPayments(ctx, gen.DashPaymentsParams{CompanyID: p.CompanyID, FromDate: f.From, ToDate: f.To, AllScope: sc.All, ScopePaths: sc.Paths, UnderPath: f.UnderPath, StoreID: f.StoreID})
	if err != nil {
		return nil, err
	}
	t := &Table{Columns: []Column{{"method", "Metode", TText}, {"payments", "Jumlah pembayaran", TInt}, {"amount", "Nilai", TMoney}, {"share", "Porsi %", TPercent}}}
	var n int64
	var total decimal.Decimal
	for _, r := range rows {
		total = total.Add(r.Amount)
	}
	for _, r := range rows {
		t.Rows = append(t.Rows, []any{r.Method, r.Payments, r.Amount, pct(r.Amount, total)})
		n += r.Payments
	}
	t.Totals = []any{"Total", n, total, d100}
	return t, nil
}

func runHourly(ctx context.Context, s *Service, p *access.Principal, sc scopeArgs, f Filter, _ Options, _ bool) (*Table, error) {
	rows, err := s.tx.Queries().DashHourly(ctx, gen.DashHourlyParams{CompanyID: p.CompanyID, FromDate: f.From, ToDate: f.To, AllScope: sc.All, ScopePaths: sc.Paths, UnderPath: f.UnderPath, StoreID: f.StoreID})
	if err != nil {
		return nil, err
	}
	t := &Table{Columns: []Column{{"hour", "Jam", TText}, {"transactions", "Transaksi", TInt}, {"net", "Penjualan bersih", TMoney}, {"avg", "Rata-rata/transaksi", TMoney}}}
	var n int64
	var net decimal.Decimal
	for _, r := range rows {
		avg := decimal.Zero
		if r.Transactions > 0 {
			avg = r.NetSales.Div(decimal.NewFromInt(r.Transactions)).Round(0)
		}
		t.Rows = append(t.Rows, []any{fmt.Sprintf("%02d:00–%02d:59", r.Hour, r.Hour), r.Transactions, r.NetSales, avg})
		n, net = n+r.Transactions, net.Add(r.NetSales)
	}
	avg := decimal.Zero
	if n > 0 {
		avg = net.Div(decimal.NewFromInt(n)).Round(0)
	}
	t.Totals = []any{"Total", n, net, avg}
	return t, nil
}

func runShiftVariance(ctx context.Context, s *Service, p *access.Principal, sc scopeArgs, f Filter, o Options, _ bool) (*Table, error) {
	rows, err := s.tx.Queries().ReportShiftVariance(ctx, gen.ReportShiftVarianceParams{CompanyID: p.CompanyID, FromTime: f.From, ToTime: f.To.AddDate(0, 0, 1),
		OnlyVariance: o.OnlyVariance, AllScope: sc.All, ScopePaths: sc.Paths, UnderPath: f.UnderPath, StoreID: f.StoreID, RowLimit: int32(o.Limit + 1)})
	if err != nil {
		return nil, err
	}
	t := &Table{Columns: []Column{{"store", "Toko", TText}, {"cashier", "Kasir", TText}, {"terminal", "Terminal", TText}, {"opened", "Dibuka", TDateTime}, {"closed", "Ditutup", TDateTime},
		{"opening", "Kas awal", TMoney}, {"expected", "Seharusnya", TMoney}, {"counted", "Dihitung", TMoney}, {"variance", "Selisih", TMoney}}}
	if len(rows) > o.Limit {
		rows, t.Truncated = rows[:o.Limit], true
	}
	var short, over decimal.Decimal // short = cash missing (negative), over = cash surplus
	for _, r := range rows {
		v := decimal.Zero
		if r.Variance != nil {
			v = *r.Variance
		}
		if v.IsNegative() {
			short = short.Add(v)
		} else {
			over = over.Add(v)
		}
		t.Rows = append(t.Rows, []any{r.StoreName, r.CashierName, r.TerminalCode, r.OpenedAt.UTC().Format(time.RFC3339), derefTime(r.ClosedAt), r.OpeningCash,
			derefDec(r.ExpectedCash), derefDec(r.CountedCash), v})
	}
	t.Totals = []any{"Total", "", "", "", "", "", "", "", short.Add(over)}
	return t, nil
}

func derefDec(d *decimal.Decimal) decimal.Decimal {
	if d == nil {
		return decimal.Zero
	}
	return *d
}

func derefTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func runValuation(ctx context.Context, s *Service, p *access.Principal, sc scopeArgs, f Filter, _ Options, _ bool) (*Table, error) {
	rows, err := s.tx.Queries().ReportInventoryValuation(ctx, gen.ReportInventoryValuationParams{CompanyID: p.CompanyID, AllScope: sc.All, ScopePaths: sc.Paths, UnderPath: f.UnderPath, StoreID: f.StoreID})
	if err != nil {
		return nil, err
	}
	t := &Table{Columns: []Column{{"code", "Kode", TText}, {"store", "Toko", TText}, {"skus", "SKU berstok", TInt}, {"units", "Unit", TQty}, {"value", "Nilai persediaan", TMoney}, {"low", "SKU menipis", TInt}}}
	var skus, low int64
	var units, value decimal.Decimal
	for _, r := range rows {
		t.Rows = append(t.Rows, []any{r.StoreCode, r.StoreName, r.Skus, r.Units, r.StockValue, r.LowStock})
		skus, low, units, value = skus+r.Skus, low+r.LowStock, units.Add(r.Units), value.Add(r.StockValue)
	}
	t.Totals = []any{"Total", "", skus, units, value, low}
	return t, nil
}

func runLowStock(ctx context.Context, s *Service, p *access.Principal, sc scopeArgs, f Filter, o Options, _ bool) (*Table, error) {
	rows, err := s.tx.Queries().ReportLowStock(ctx, gen.ReportLowStockParams{CompanyID: p.CompanyID, AllScope: sc.All, ScopePaths: sc.Paths, UnderPath: f.UnderPath, StoreID: f.StoreID, RowLimit: int32(o.Limit + 1)})
	if err != nil {
		return nil, err
	}
	t := &Table{Columns: []Column{{"store", "Toko", TText}, {"warehouse", "Gudang", TText}, {"sku", "SKU", TText}, {"product", "Produk", TText}, {"on_hand", "Stok", TQty},
		{"reorder", "Titik pesan", TQty}, {"min", "Stok minimum", TQty}, {"suggested", "Saran order", TQty}}}
	if len(rows) > o.Limit {
		rows, t.Truncated = rows[:o.Limit], true
	}
	for _, r := range rows {
		name := r.ProductName
		if r.VariantName != "" {
			name += " " + r.VariantName
		}
		t.Rows = append(t.Rows, []any{r.StoreName, r.WarehouseName, r.Sku, name, r.OnHand, r.ReorderPoint, r.MinStock, r.SuggestedOrder})
	}
	return t, nil
}
