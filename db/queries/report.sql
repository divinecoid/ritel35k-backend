-- ===== Summary maintenance (internal/summary) =====

-- name: MarkSummaryDirty :exec
INSERT INTO summary_dirty (store_id, business_date, company_id) VALUES ($1, $2, $3)
ON CONFLICT (store_id, business_date) DO UPDATE SET marked_at = now();

-- name: ClaimSummaryDirty :many
-- Takes a batch of slices (SKIP LOCKED: safe with several workers) in the same tx that rebuilds them.
DELETE FROM summary_dirty d
USING (SELECT store_id, business_date FROM summary_dirty ORDER BY marked_at LIMIT sqlc.arg(batch)::int FOR UPDATE SKIP LOCKED) c
WHERE d.store_id = c.store_id AND d.business_date = c.business_date
RETURNING d.store_id, d.business_date, d.company_id;

-- name: SaleBusinessDate :one
SELECT store_id, business_date, company_id FROM sales WHERE id = $1;

-- name: ReturnBusinessDate :one
SELECT r.store_id, r.company_id, (r.created_at AT TIME ZONE st.timezone)::date AS business_date
FROM sale_returns r JOIN stores st ON st.id = r.store_id WHERE r.id = $1;

-- name: DeleteKpiSlice :exec
DELETE FROM store_kpi_daily WHERE store_id = $1 AND business_date = $2;
-- name: DeleteHourlySlice :exec
DELETE FROM sales_hourly WHERE store_id = $1 AND business_date = $2;
-- name: DeleteProductSlice :exec
DELETE FROM sales_by_product_daily WHERE store_id = $1 AND business_date = $2;
-- name: DeletePaymentSlice :exec
DELETE FROM payment_method_daily WHERE store_id = $1 AND business_date = $2;

-- name: InsertKpiSlice :exec
WITH st AS (SELECT id, company_id, timezone FROM stores WHERE id = sqlc.arg(store_id)::uuid),
w AS (SELECT (sqlc.arg(day)::date)::timestamp AT TIME ZONE st.timezone AS t0,
             ((sqlc.arg(day)::date) + 1)::timestamp AT TIME ZONE st.timezone AS t1 FROM st),
s AS (
    SELECT count(*) FILTER (WHERE status = 'completed')::int AS tx,
           count(*) FILTER (WHERE status = 'voided')::int AS voided,
           COALESCE(sum(subtotal) FILTER (WHERE status = 'completed'), 0)::numeric AS gross,
           COALESCE(sum(discount_total) FILTER (WHERE status = 'completed'), 0)::numeric AS disc,
           COALESCE(sum(tax_total) FILTER (WHERE status = 'completed'), 0)::numeric AS tax,
           COALESCE(sum(grand_total) FILTER (WHERE status = 'completed'), 0)::numeric AS grand
    FROM sales WHERE store_id = sqlc.arg(store_id)::uuid AND business_date = sqlc.arg(day)::date
),
i AS (
    SELECT COALESCE(sum(si.qty), 0)::numeric AS qty, COALESCE(sum(si.qty * si.unit_cost), 0)::numeric AS cogs
    FROM sale_items si JOIN sales sa ON sa.id = si.sale_id
    WHERE sa.store_id = sqlc.arg(store_id)::uuid AND sa.business_date = sqlc.arg(day)::date AND sa.status = 'completed'
),
r AS (
    SELECT count(DISTINCT sr.id)::int AS n,
           COALESCE(sum(sri.qty), 0)::numeric AS qty,
           COALESCE(sum(sri.amount), 0)::numeric AS refund,
           COALESCE(sum(sri.amount * (1 - COALESCE(si.tax_amount / NULLIF(si.line_total, 0), 0))), 0)::numeric AS refund_net,
           COALESCE(sum(CASE WHEN sri.restock THEN sri.qty * si.unit_cost ELSE 0 END), 0)::numeric AS restock_cost
    FROM sale_returns sr
    JOIN sale_return_items sri ON sri.return_id = sr.id
    JOIN sale_items si ON si.id = sri.sale_item_id
    CROSS JOIN w
    WHERE sr.store_id = sqlc.arg(store_id)::uuid AND sr.created_at >= w.t0 AND sr.created_at < w.t1
)
INSERT INTO store_kpi_daily (store_id, business_date, company_id, transactions, voided, returns_count, items_sold, gross_sales, discount, tax,
                             grand_total, refund, net_sales, cogs, gross_profit)
SELECT st.id, sqlc.arg(day)::date, st.company_id, s.tx, s.voided, r.n, i.qty - r.qty, s.gross, s.disc, s.tax, s.grand, r.refund,
       s.grand - s.tax - r.refund_net, i.cogs - r.restock_cost, (s.grand - s.tax - r.refund_net) - (i.cogs - r.restock_cost)
FROM st CROSS JOIN s CROSS JOIN i CROSS JOIN r;

-- name: InsertHourlySlice :exec
INSERT INTO sales_hourly (store_id, business_date, hour, company_id, transactions, net_sales)
SELECT sa.store_id, sa.business_date, extract(hour FROM sa.created_at AT TIME ZONE st.timezone)::smallint, sa.company_id,
       count(*)::int, sum(sa.grand_total - sa.tax_total)
FROM sales sa JOIN stores st ON st.id = sa.store_id
WHERE sa.store_id = sqlc.arg(store_id)::uuid AND sa.business_date = sqlc.arg(day)::date AND sa.status = 'completed'
GROUP BY sa.store_id, sa.business_date, 3, sa.company_id;

-- name: InsertProductSlice :exec
WITH st AS (SELECT id, company_id, timezone FROM stores WHERE id = sqlc.arg(store_id)::uuid),
w AS (SELECT (sqlc.arg(day)::date)::timestamp AT TIME ZONE st.timezone AS t0,
             ((sqlc.arg(day)::date) + 1)::timestamp AT TIME ZONE st.timezone AS t1 FROM st),
x AS (
    SELECT si.variant_id, si.qty AS qty, (si.line_total - si.tax_amount) AS net, si.discount_amount AS disc, (si.qty * si.unit_cost) AS cogs
    FROM sale_items si JOIN sales sa ON sa.id = si.sale_id
    WHERE sa.store_id = sqlc.arg(store_id)::uuid AND sa.business_date = sqlc.arg(day)::date AND sa.status = 'completed'
    UNION ALL
    SELECT si.variant_id, -sri.qty, -(sri.amount * (1 - COALESCE(si.tax_amount / NULLIF(si.line_total, 0), 0))), 0::numeric,
           -(CASE WHEN sri.restock THEN sri.qty * si.unit_cost ELSE 0 END)
    FROM sale_returns sr
    JOIN sale_return_items sri ON sri.return_id = sr.id
    JOIN sale_items si ON si.id = sri.sale_item_id
    CROSS JOIN w
    WHERE sr.store_id = sqlc.arg(store_id)::uuid AND sr.created_at >= w.t0 AND sr.created_at < w.t1
)
INSERT INTO sales_by_product_daily (store_id, business_date, variant_id, company_id, category_id, brand_id, qty, net_sales, discount, cogs)
SELECT sqlc.arg(store_id)::uuid, sqlc.arg(day)::date, x.variant_id, (SELECT company_id FROM st), p.category_id, p.brand_id,
       sum(x.qty), sum(x.net), sum(x.disc), sum(x.cogs)
FROM x JOIN product_variants pv ON pv.id = x.variant_id JOIN products p ON p.id = pv.product_id
GROUP BY x.variant_id, p.category_id, p.brand_id;

-- name: InsertPaymentSlice :exec
INSERT INTO payment_method_daily (store_id, business_date, method, company_id, payments, amount)
SELECT sa.store_id, sa.business_date, pm.method, sa.company_id, count(*)::int, sum(pm.amount)
FROM payments pm JOIN sales sa ON sa.id = pm.sale_id
WHERE sa.store_id = sqlc.arg(store_id)::uuid AND sa.business_date = sqlc.arg(day)::date AND sa.status = 'completed'
GROUP BY sa.store_id, sa.business_date, pm.method, sa.company_id;

-- name: SummaryDrift :many
-- Reconcile: slices in the window whose summary disagrees with the source (or is missing / stale).
WITH src AS (
    SELECT store_id, business_date, count(*) FILTER (WHERE status = 'completed')::int AS tx,
           COALESCE(sum(grand_total) FILTER (WHERE status = 'completed'), 0)::numeric AS grand,
           count(*) FILTER (WHERE status = 'voided')::int AS voided
    FROM sales WHERE business_date >= sqlc.arg(since)::date GROUP BY store_id, business_date
)
SELECT src.store_id, src.business_date
FROM src LEFT JOIN store_kpi_daily k ON k.store_id = src.store_id AND k.business_date = src.business_date
WHERE k.store_id IS NULL OR k.transactions <> src.tx OR k.grand_total <> src.grand OR k.voided <> src.voided
UNION
SELECT k.store_id, k.business_date
FROM store_kpi_daily k
WHERE k.business_date >= sqlc.arg(since)::date AND (k.transactions > 0 OR k.voided > 0)
  AND NOT EXISTS (SELECT 1 FROM sales s WHERE s.store_id = k.store_id AND s.business_date = k.business_date);

-- name: SummaryReturnDrift :many
-- Refund totals per store/local day: source (sale_return_items) vs the summary column.
WITH src AS (
    SELECT sr.store_id, (sr.created_at AT TIME ZONE st.timezone)::date AS business_date, COALESCE(sum(sri.amount), 0)::numeric AS refund
    FROM sale_returns sr JOIN sale_return_items sri ON sri.return_id = sr.id JOIN stores st ON st.id = sr.store_id
    WHERE sr.created_at >= sqlc.arg(since)::date::timestamptz - interval '1 day'
    GROUP BY sr.store_id, 2
)
SELECT src.store_id, src.business_date
FROM src LEFT JOIN store_kpi_daily k ON k.store_id = src.store_id AND k.business_date = src.business_date
WHERE src.business_date >= sqlc.arg(since)::date AND (k.store_id IS NULL OR k.refund <> src.refund);

-- ===== Dashboard / report reads =====
-- Every read joins store -> org_unit and applies the caller's scope + the optional under_path / store filter.

-- name: GetOrgUnitByPath :one
SELECT id, name, type, path::text AS path FROM org_units WHERE company_id = $1 AND path = sqlc.arg(path)::text::ltree;

-- name: DashKpiTotals :one
SELECT COALESCE(sum(k.transactions), 0)::bigint AS transactions, COALESCE(sum(k.voided), 0)::bigint AS voided,
       COALESCE(sum(k.returns_count), 0)::bigint AS returns_count, COALESCE(sum(k.items_sold), 0)::numeric AS items_sold,
       COALESCE(sum(k.gross_sales), 0)::numeric AS gross_sales, COALESCE(sum(k.discount), 0)::numeric AS discount,
       COALESCE(sum(k.tax), 0)::numeric AS tax, COALESCE(sum(k.grand_total), 0)::numeric AS grand_total,
       COALESCE(sum(k.refund), 0)::numeric AS refund, COALESCE(sum(k.net_sales), 0)::numeric AS net_sales,
       COALESCE(sum(k.cogs), 0)::numeric AS cogs, COALESCE(sum(k.gross_profit), 0)::numeric AS gross_profit
FROM store_kpi_daily k
JOIN stores s ON s.id = k.store_id JOIN org_units o ON o.id = s.org_unit_id
WHERE k.company_id = sqlc.arg(company_id) AND k.business_date BETWEEN sqlc.arg(from_date)::date AND sqlc.arg(to_date)::date
  AND (sqlc.arg(all_scope)::bool OR EXISTS (SELECT 1 FROM unnest(sqlc.arg(scope_paths)::text[]) sp WHERE o.path <@ sp::ltree))
  AND (sqlc.arg(under_path)::text = '' OR o.path <@ sqlc.arg(under_path)::text::ltree)
  AND (sqlc.narg(store_id)::uuid IS NULL OR k.store_id = sqlc.narg(store_id)::uuid);

-- name: DashKpiDaily :many
SELECT k.business_date, sum(k.transactions)::bigint AS transactions, sum(k.items_sold)::numeric AS items_sold,
       sum(k.gross_sales)::numeric AS gross_sales, sum(k.discount)::numeric AS discount, sum(k.refund)::numeric AS refund,
       sum(k.net_sales)::numeric AS net_sales, sum(k.cogs)::numeric AS cogs, sum(k.gross_profit)::numeric AS gross_profit
FROM store_kpi_daily k
JOIN stores s ON s.id = k.store_id JOIN org_units o ON o.id = s.org_unit_id
WHERE k.company_id = sqlc.arg(company_id) AND k.business_date BETWEEN sqlc.arg(from_date)::date AND sqlc.arg(to_date)::date
  AND (sqlc.arg(all_scope)::bool OR EXISTS (SELECT 1 FROM unnest(sqlc.arg(scope_paths)::text[]) sp WHERE o.path <@ sp::ltree))
  AND (sqlc.arg(under_path)::text = '' OR o.path <@ sqlc.arg(under_path)::text::ltree)
  AND (sqlc.narg(store_id)::uuid IS NULL OR k.store_id = sqlc.narg(store_id)::uuid)
GROUP BY k.business_date ORDER BY k.business_date;

-- name: DashKpiByMonth :many
SELECT date_trunc('month', k.business_date)::date AS month, sum(k.transactions)::bigint AS transactions, sum(k.items_sold)::numeric AS items_sold,
       sum(k.gross_sales)::numeric AS gross_sales, sum(k.discount)::numeric AS discount, sum(k.refund)::numeric AS refund,
       sum(k.net_sales)::numeric AS net_sales, sum(k.cogs)::numeric AS cogs, sum(k.gross_profit)::numeric AS gross_profit
FROM store_kpi_daily k
JOIN stores s ON s.id = k.store_id JOIN org_units o ON o.id = s.org_unit_id
WHERE k.company_id = sqlc.arg(company_id) AND k.business_date BETWEEN sqlc.arg(from_date)::date AND sqlc.arg(to_date)::date
  AND (sqlc.arg(all_scope)::bool OR EXISTS (SELECT 1 FROM unnest(sqlc.arg(scope_paths)::text[]) sp WHERE o.path <@ sp::ltree))
  AND (sqlc.arg(under_path)::text = '' OR o.path <@ sqlc.arg(under_path)::text::ltree)
  AND (sqlc.narg(store_id)::uuid IS NULL OR k.store_id = sqlc.narg(store_id)::uuid)
GROUP BY 1 ORDER BY 1;

-- name: DashKpiByStore :many
SELECT s.id AS store_id, s.code AS store_code, s.name AS store_name, COALESCE(sum(k.transactions), 0)::bigint AS transactions,
       COALESCE(sum(k.items_sold), 0)::numeric AS items_sold, COALESCE(sum(k.gross_sales), 0)::numeric AS gross_sales,
       COALESCE(sum(k.discount), 0)::numeric AS discount, COALESCE(sum(k.refund), 0)::numeric AS refund,
       COALESCE(sum(k.net_sales), 0)::numeric AS net_sales, COALESCE(sum(k.cogs), 0)::numeric AS cogs,
       COALESCE(sum(k.gross_profit), 0)::numeric AS gross_profit
FROM stores s JOIN org_units o ON o.id = s.org_unit_id
LEFT JOIN store_kpi_daily k ON k.store_id = s.id AND k.business_date BETWEEN sqlc.arg(from_date)::date AND sqlc.arg(to_date)::date
WHERE s.company_id = sqlc.arg(company_id) AND s.status <> 'closed'
  AND (sqlc.arg(all_scope)::bool OR EXISTS (SELECT 1 FROM unnest(sqlc.arg(scope_paths)::text[]) sp WHERE o.path <@ sp::ltree))
  AND (sqlc.arg(under_path)::text = '' OR o.path <@ sqlc.arg(under_path)::text::ltree)
  AND (sqlc.narg(store_id)::uuid IS NULL OR s.id = sqlc.narg(store_id)::uuid)
GROUP BY s.id, s.code, s.name ORDER BY net_sales DESC, s.name;

-- name: DashKpiByChild :many
-- One row per direct child of the selected node (region, business unit or store), stores without sales included.
SELECT c.id AS unit_id, c.code AS unit_code, c.name AS unit_name, c.type AS unit_type, c.path::text AS unit_path,
       count(DISTINCT s.id)::bigint AS stores, COALESCE(sum(k.transactions), 0)::bigint AS transactions,
       COALESCE(sum(k.net_sales), 0)::numeric AS net_sales, COALESCE(sum(k.gross_profit), 0)::numeric AS gross_profit,
       COALESCE(sum(k.refund), 0)::numeric AS refund
FROM org_units c
JOIN org_units o ON o.path <@ c.path
JOIN stores s ON s.org_unit_id = o.id AND s.status <> 'closed'
LEFT JOIN store_kpi_daily k ON k.store_id = s.id AND k.business_date BETWEEN sqlc.arg(from_date)::date AND sqlc.arg(to_date)::date
WHERE c.company_id = sqlc.arg(company_id) AND c.parent_id = sqlc.arg(node_id)::uuid
  AND (sqlc.arg(all_scope)::bool OR EXISTS (SELECT 1 FROM unnest(sqlc.arg(scope_paths)::text[]) sp WHERE o.path <@ sp::ltree))
GROUP BY c.id, c.code, c.name, c.type, c.path ORDER BY net_sales DESC, c.name;

-- name: DashHourly :many
SELECT h.hour, sum(h.transactions)::bigint AS transactions, sum(h.net_sales)::numeric AS net_sales
FROM sales_hourly h
JOIN stores s ON s.id = h.store_id JOIN org_units o ON o.id = s.org_unit_id
WHERE h.company_id = sqlc.arg(company_id) AND h.business_date BETWEEN sqlc.arg(from_date)::date AND sqlc.arg(to_date)::date
  AND (sqlc.arg(all_scope)::bool OR EXISTS (SELECT 1 FROM unnest(sqlc.arg(scope_paths)::text[]) sp WHERE o.path <@ sp::ltree))
  AND (sqlc.arg(under_path)::text = '' OR o.path <@ sqlc.arg(under_path)::text::ltree)
  AND (sqlc.narg(store_id)::uuid IS NULL OR h.store_id = sqlc.narg(store_id)::uuid)
GROUP BY h.hour ORDER BY h.hour;

-- name: DashPayments :many
SELECT pm.method, sum(pm.payments)::bigint AS payments, sum(pm.amount)::numeric AS amount
FROM payment_method_daily pm
JOIN stores s ON s.id = pm.store_id JOIN org_units o ON o.id = s.org_unit_id
WHERE pm.company_id = sqlc.arg(company_id) AND pm.business_date BETWEEN sqlc.arg(from_date)::date AND sqlc.arg(to_date)::date
  AND (sqlc.arg(all_scope)::bool OR EXISTS (SELECT 1 FROM unnest(sqlc.arg(scope_paths)::text[]) sp WHERE o.path <@ sp::ltree))
  AND (sqlc.arg(under_path)::text = '' OR o.path <@ sqlc.arg(under_path)::text::ltree)
  AND (sqlc.narg(store_id)::uuid IS NULL OR pm.store_id = sqlc.narg(store_id)::uuid)
GROUP BY pm.method ORDER BY amount DESC;

-- name: DashTopProducts :many
SELECT pv.id AS variant_id, pv.sku, p.name AS product_name, pv.name AS variant_name, COALESCE(c.name, '') AS category_name,
       sum(d.qty)::numeric AS qty, sum(d.net_sales)::numeric AS net_sales, sum(d.cogs)::numeric AS cogs, sum(d.discount)::numeric AS discount
FROM sales_by_product_daily d
JOIN stores s ON s.id = d.store_id JOIN org_units o ON o.id = s.org_unit_id
JOIN product_variants pv ON pv.id = d.variant_id JOIN products p ON p.id = pv.product_id
LEFT JOIN categories c ON c.id = d.category_id
WHERE d.company_id = sqlc.arg(company_id) AND d.business_date BETWEEN sqlc.arg(from_date)::date AND sqlc.arg(to_date)::date
  AND (sqlc.arg(all_scope)::bool OR EXISTS (SELECT 1 FROM unnest(sqlc.arg(scope_paths)::text[]) sp WHERE o.path <@ sp::ltree))
  AND (sqlc.arg(under_path)::text = '' OR o.path <@ sqlc.arg(under_path)::text::ltree)
  AND (sqlc.narg(store_id)::uuid IS NULL OR d.store_id = sqlc.narg(store_id)::uuid)
GROUP BY pv.id, pv.sku, p.name, pv.name, c.name
ORDER BY CASE WHEN sqlc.arg(order_by)::text = 'qty' THEN sum(d.qty) ELSE sum(d.net_sales) END DESC, pv.sku
LIMIT sqlc.arg(row_limit);

-- name: DashByCategory :many
SELECT COALESCE(c.id::text, '') AS category_id, COALESCE(c.name, 'Tanpa kategori') AS category_name,
       sum(d.qty)::numeric AS qty, sum(d.net_sales)::numeric AS net_sales, sum(d.cogs)::numeric AS cogs
FROM sales_by_product_daily d
JOIN stores s ON s.id = d.store_id JOIN org_units o ON o.id = s.org_unit_id
LEFT JOIN categories c ON c.id = d.category_id
WHERE d.company_id = sqlc.arg(company_id) AND d.business_date BETWEEN sqlc.arg(from_date)::date AND sqlc.arg(to_date)::date
  AND (sqlc.arg(all_scope)::bool OR EXISTS (SELECT 1 FROM unnest(sqlc.arg(scope_paths)::text[]) sp WHERE o.path <@ sp::ltree))
  AND (sqlc.arg(under_path)::text = '' OR o.path <@ sqlc.arg(under_path)::text::ltree)
  AND (sqlc.narg(store_id)::uuid IS NULL OR d.store_id = sqlc.narg(store_id)::uuid)
GROUP BY c.id, c.name ORDER BY net_sales DESC;

-- name: DashStock :one
-- Current stock value and low-stock SKU count straight from the balance cache (one row per warehouse/variant, not the ledger).
SELECT COALESCE(sum(b.on_hand * b.avg_cost) FILTER (WHERE b.on_hand > 0), 0)::numeric AS stock_value,
       count(*) FILTER (WHERE v.reorder_point > 0 AND b.on_hand <= v.reorder_point)::bigint AS low_stock,
       count(*) FILTER (WHERE b.on_hand <= 0)::bigint AS out_of_stock
FROM inventory_balances b
JOIN product_variants v ON v.id = b.variant_id AND v.status = 'active'
JOIN stores s ON s.id = b.store_id JOIN org_units o ON o.id = s.org_unit_id
WHERE b.company_id = sqlc.arg(company_id)
  AND (sqlc.arg(all_scope)::bool OR EXISTS (SELECT 1 FROM unnest(sqlc.arg(scope_paths)::text[]) sp WHERE o.path <@ sp::ltree))
  AND (sqlc.arg(under_path)::text = '' OR o.path <@ sqlc.arg(under_path)::text::ltree)
  AND (sqlc.narg(store_id)::uuid IS NULL OR b.store_id = sqlc.narg(store_id)::uuid);

-- name: DashOpenShifts :one
SELECT count(*)::bigint FROM cashier_shifts sh
JOIN stores s ON s.id = sh.store_id JOIN org_units o ON o.id = s.org_unit_id
WHERE sh.company_id = sqlc.arg(company_id) AND sh.status = 'open'
  AND (sqlc.arg(all_scope)::bool OR EXISTS (SELECT 1 FROM unnest(sqlc.arg(scope_paths)::text[]) sp WHERE o.path <@ sp::ltree))
  AND (sqlc.arg(under_path)::text = '' OR o.path <@ sqlc.arg(under_path)::text::ltree)
  AND (sqlc.narg(store_id)::uuid IS NULL OR sh.store_id = sqlc.narg(store_id)::uuid);

-- name: ReportInventoryValuation :many
SELECT s.id AS store_id, s.code AS store_code, s.name AS store_name, count(*) FILTER (WHERE b.on_hand > 0)::bigint AS skus,
       COALESCE(sum(b.on_hand) FILTER (WHERE b.on_hand > 0), 0)::numeric AS units,
       COALESCE(sum(b.on_hand * b.avg_cost) FILTER (WHERE b.on_hand > 0), 0)::numeric AS stock_value,
       count(*) FILTER (WHERE v.reorder_point > 0 AND b.on_hand <= v.reorder_point)::bigint AS low_stock
FROM inventory_balances b
JOIN product_variants v ON v.id = b.variant_id
JOIN stores s ON s.id = b.store_id JOIN org_units o ON o.id = s.org_unit_id
WHERE b.company_id = sqlc.arg(company_id)
  AND (sqlc.arg(all_scope)::bool OR EXISTS (SELECT 1 FROM unnest(sqlc.arg(scope_paths)::text[]) sp WHERE o.path <@ sp::ltree))
  AND (sqlc.arg(under_path)::text = '' OR o.path <@ sqlc.arg(under_path)::text::ltree)
  AND (sqlc.narg(store_id)::uuid IS NULL OR b.store_id = sqlc.narg(store_id)::uuid)
GROUP BY s.id, s.code, s.name ORDER BY stock_value DESC;

-- name: ReportLowStock :many
SELECT s.name AS store_name, w.name AS warehouse_name, v.sku, p.name AS product_name, v.name AS variant_name,
       b.on_hand, v.reorder_point, v.min_stock, GREATEST(v.max_stock - b.on_hand, 0)::numeric AS suggested_order
FROM inventory_balances b
JOIN product_variants v ON v.id = b.variant_id AND v.status = 'active'
JOIN products p ON p.id = v.product_id
JOIN warehouses w ON w.id = b.warehouse_id
JOIN stores s ON s.id = b.store_id JOIN org_units o ON o.id = s.org_unit_id
WHERE b.company_id = sqlc.arg(company_id) AND v.reorder_point > 0 AND b.on_hand <= v.reorder_point
  AND (sqlc.arg(all_scope)::bool OR EXISTS (SELECT 1 FROM unnest(sqlc.arg(scope_paths)::text[]) sp WHERE o.path <@ sp::ltree))
  AND (sqlc.arg(under_path)::text = '' OR o.path <@ sqlc.arg(under_path)::text::ltree)
  AND (sqlc.narg(store_id)::uuid IS NULL OR b.store_id = sqlc.narg(store_id)::uuid)
ORDER BY (b.on_hand - v.reorder_point), s.name, p.name
LIMIT sqlc.arg(row_limit);

-- name: ReportShiftVariance :many
SELECT sh.id, s.name AS store_name, u.name AS cashier_name, t.code AS terminal_code, sh.opened_at, sh.closed_at,
       sh.opening_cash, sh.expected_cash, sh.counted_cash, sh.variance
FROM cashier_shifts sh
JOIN stores s ON s.id = sh.store_id JOIN org_units o ON o.id = s.org_unit_id
JOIN users u ON u.id = sh.cashier_id JOIN pos_terminals t ON t.id = sh.terminal_id
WHERE sh.company_id = sqlc.arg(company_id) AND sh.status = 'closed' AND sh.variance IS NOT NULL
  AND sh.closed_at >= sqlc.arg(from_time)::timestamptz AND sh.closed_at < sqlc.arg(to_time)::timestamptz
  AND (NOT sqlc.arg(only_variance)::bool OR sh.variance <> 0)
  AND (sqlc.arg(all_scope)::bool OR EXISTS (SELECT 1 FROM unnest(sqlc.arg(scope_paths)::text[]) sp WHERE o.path <@ sp::ltree))
  AND (sqlc.arg(under_path)::text = '' OR o.path <@ sqlc.arg(under_path)::text::ltree)
  AND (sqlc.narg(store_id)::uuid IS NULL OR sh.store_id = sqlc.narg(store_id)::uuid)
ORDER BY sh.closed_at DESC
LIMIT sqlc.arg(row_limit);

-- name: PgNotify :exec
SELECT pg_notify(sqlc.arg(channel)::text, sqlc.arg(payload)::text);

-- name: GetStoreCompany :one
SELECT company_id FROM stores WHERE id = $1;
