package integration

import (
	"context"
	"fmt"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestDashboardAtScale checks the plan's acceptance criterion: the "All stores" dashboard reads 50 stores x 1 year of
// summaries in under a second. It seeds synthetic summary rows directly (the rebuild path is covered elsewhere), so it
// is opt-in:  BENCH_SUMMARY=1 go test ./tests/integration -run TestDashboardAtScale -v
func TestDashboardAtScale(t *testing.T) {
	if os.Getenv("BENCH_SUMMARY") == "" {
		t.Skip("set BENCH_SUMMARY=1 to run")
	}
	ctx := context.Background()
	tn := newTenant(t)
	region := tn.unit(t, "region", "BR-"+uniq("r"), tn.RootUnit)
	for i := 0; i < 50; i++ {
		tn.store(t, fmt.Sprintf("BS%02d-%s", i, uniq("s")), region)
	}
	items := make([]item, 30)
	for i := range items {
		items[i] = tn.product(t, uniq("BP"), "25000")
	}
	cid := uuid.MustParse(tn.CompanyID)
	end := time.Now().UTC().Truncate(24 * time.Hour)
	start := end.AddDate(0, 0, -364)

	mustExec := func(sql string, args ...any) {
		t.Helper()
		if _, err := testPool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	mustExec(`INSERT INTO store_kpi_daily (store_id, business_date, company_id, transactions, items_sold, gross_sales, discount, tax, grand_total, refund, net_sales, cogs, gross_profit)
		SELECT s.id, d::date, s.company_id, 400 + (random()*200)::int, 900, 20000000, 500000, 1900000, 19500000, 200000, 17000000, 11000000, 6000000
		FROM stores s CROSS JOIN generate_series($2::date, $3::date, '1 day') d WHERE s.company_id = $1`, cid, start, end)
	mustExec(`INSERT INTO sales_hourly (store_id, business_date, hour, company_id, transactions, net_sales)
		SELECT s.id, d::date, h, s.company_id, 30, 1200000 FROM stores s CROSS JOIN generate_series($2::date, $3::date, '1 day') d CROSS JOIN generate_series(8, 21) h WHERE s.company_id = $1`, cid, start, end)
	mustExec(`INSERT INTO payment_method_daily (store_id, business_date, method, company_id, payments, amount)
		SELECT s.id, d::date, m, s.company_id, 100, 4000000 FROM stores s CROSS JOIN generate_series($2::date, $3::date, '1 day') d CROSS JOIN unnest(ARRAY['CASH','QRIS','DEBIT','EWALLET']) m WHERE s.company_id = $1`, cid, start, end)
	mustExec(`INSERT INTO sales_by_product_daily (store_id, business_date, variant_id, company_id, category_id, brand_id, qty, net_sales, discount, cogs)
		SELECT s.id, d::date, v.id, s.company_id, NULL, NULL, 12, 300000, 0, 180000
		FROM stores s CROSS JOIN generate_series($2::date, $3::date, '1 day') d CROSS JOIN product_variants v WHERE s.company_id = $1 AND v.company_id = $1`, cid, start, end)
	mustExec(`ANALYZE store_kpi_daily; ANALYZE sales_hourly; ANALYZE payment_method_daily; ANALYZE sales_by_product_daily`)

	var rows int
	_ = testPool.QueryRow(ctx, `select count(*) from sales_by_product_daily where company_id=$1`, cid).Scan(&rows)
	t.Logf("seeded: 50 stores x 365 days; product-daily rows = %d", rows)

	url := "/dashboard/overview?from=" + start.Format("2006-01-02") + "&to=" + end.Format("2006-01-02")
	var d []time.Duration
	for i := 0; i < 7; i++ {
		t0 := time.Now()
		var o overviewOut
		tn.Owner.mustOK("GET", url, nil, &o)
		d = append(d, time.Since(t0))
		if i == 0 && (o.Current.Transactions < 50*365*400 || len(o.Breakdown) != 1 || o.Breakdown[0].Stores != 50) {
			t.Fatalf("unexpected totals: %+v", o.Current)
		}
	}
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	t.Logf("overview latency over 365 days: min=%v median=%v max=%v", d[0], d[len(d)/2], d[len(d)-1])
	if d[len(d)/2] > time.Second {
		t.Fatalf("median %v exceeds the 1s target", d[len(d)/2])
	}
}
