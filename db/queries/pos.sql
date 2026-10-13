-- name: CreateTerminal :one
INSERT INTO pos_terminals (id, company_id, store_id, code, name, created_by) VALUES ($1, $2, $3, $4, $5, $6) RETURNING *;

-- name: GetTerminal :one
SELECT t.*, o.path::text AS path FROM pos_terminals t
JOIN stores s ON s.id = t.store_id JOIN org_units o ON o.id = s.org_unit_id
WHERE t.id = $1 AND t.company_id = $2;

-- name: ListTerminals :many
SELECT t.*, s.name AS store_name, o.path::text AS path FROM pos_terminals t
JOIN stores s ON s.id = t.store_id JOIN org_units o ON o.id = s.org_unit_id
WHERE t.company_id = sqlc.arg(company_id)
  AND (sqlc.arg(all_scope)::bool OR EXISTS (SELECT 1 FROM unnest(sqlc.arg(scope_paths)::text[]) sp WHERE o.path <@ sp::ltree))
  AND (sqlc.narg(store_id)::uuid IS NULL OR t.store_id = sqlc.narg(store_id)::uuid)
ORDER BY s.name, t.code;

-- name: UpdateTerminal :one
UPDATE pos_terminals SET name = COALESCE(sqlc.narg(name), name), status = COALESCE(sqlc.narg(status), status), updated_at = now()
WHERE id = sqlc.arg(id) AND company_id = sqlc.arg(company_id) RETURNING *;

-- name: CreateShift :one
INSERT INTO cashier_shifts (id, company_id, store_id, terminal_id, cashier_id, opening_cash) VALUES ($1, $2, $3, $4, $5, $6) RETURNING *;

-- name: GetShift :one
SELECT sh.*, o.path::text AS path FROM cashier_shifts sh
JOIN stores s ON s.id = sh.store_id JOIN org_units o ON o.id = s.org_unit_id
WHERE sh.id = $1 AND sh.company_id = $2;

-- name: GetShiftForUpdate :one
SELECT * FROM cashier_shifts WHERE id = $1 AND company_id = $2 FOR UPDATE;

-- name: GetOpenShiftByCashier :one
SELECT * FROM cashier_shifts WHERE cashier_id = $1 AND status = 'open';

-- name: GetOpenShiftByTerminal :one
SELECT * FROM cashier_shifts WHERE terminal_id = $1 AND status = 'open';

-- name: ListShifts :many
SELECT sh.*, t.code AS terminal_code, u.name AS cashier_name, s.name AS store_name
FROM cashier_shifts sh
JOIN pos_terminals t ON t.id = sh.terminal_id
JOIN users u ON u.id = sh.cashier_id
JOIN stores s ON s.id = sh.store_id JOIN org_units o ON o.id = s.org_unit_id
WHERE sh.company_id = sqlc.arg(company_id)
  AND (sqlc.arg(all_scope)::bool OR EXISTS (SELECT 1 FROM unnest(sqlc.arg(scope_paths)::text[]) sp WHERE o.path <@ sp::ltree))
  AND (sqlc.narg(store_id)::uuid IS NULL OR sh.store_id = sqlc.narg(store_id)::uuid)
  AND (sqlc.arg(status)::text = '' OR sh.status = sqlc.arg(status)::text)
ORDER BY sh.opened_at DESC
LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);

-- name: CloseShift :one
UPDATE cashier_shifts SET status = 'closed', closed_at = now(), closed_by = $2, expected_cash = $3, counted_cash = $4, variance = $5, note = $6
WHERE id = $1 AND status = 'open' RETURNING *;

-- name: InsertCashMovement :one
INSERT INTO cash_movements (id, company_id, store_id, shift_id, type, amount, reason, ref_type, ref_id, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING *;

-- name: ListCashMovements :many
SELECT * FROM cash_movements WHERE shift_id = $1 ORDER BY created_at;

-- name: ShiftTotals :one
SELECT
  COALESCE((SELECT SUM(p.amount) FROM payments p JOIN sales s ON s.id = p.sale_id
            WHERE s.shift_id = $1 AND s.status = 'completed' AND p.method = 'CASH'), 0)::numeric AS cash_sales,
  COALESCE((SELECT SUM(p.amount) FROM payments p JOIN sales s ON s.id = p.sale_id
            WHERE s.shift_id = $1 AND s.status = 'completed' AND p.method <> 'CASH'), 0)::numeric AS noncash_sales,
  COALESCE((SELECT SUM(s.grand_total) FROM sales s WHERE s.shift_id = $1 AND s.status = 'completed'), 0)::numeric AS gross_sales,
  (SELECT count(*) FROM sales s WHERE s.shift_id = $1 AND s.status = 'completed') AS sale_count,
  (SELECT count(*) FROM sales s WHERE s.shift_id = $1 AND s.status = 'voided') AS void_count,
  COALESCE((SELECT SUM(amount) FROM cash_movements WHERE shift_id = $1 AND type = 'CASH_IN'), 0)::numeric AS cash_in,
  COALESCE((SELECT SUM(amount) FROM cash_movements WHERE shift_id = $1 AND type = 'CASH_OUT'), 0)::numeric AS cash_out,
  COALESCE((SELECT SUM(amount) FROM cash_movements WHERE shift_id = $1 AND type = 'REFUND'), 0)::numeric AS cash_refunds;

-- name: InsertSale :one
INSERT INTO sales (id, company_id, store_id, terminal_id, shift_id, cashier_id, document_number, client_txn_id, business_date,
                   subtotal, discount_total, tax_total, grand_total, paid_total, change_amount, approved_by,
                   customer_id, voucher_id, voucher_discount, promo_discount, points_earned, points_redeemed)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22) RETURNING *;

-- name: InsertSaleItem :one
INSERT INTO sale_items (id, sale_id, line_no, variant_id, sku, product_name, barcode, qty, unit_price, discount_percent, discount_amount,
                        tax_rate, tax_amount, line_total, unit_cost, warehouse_id, promo_discount)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17) RETURNING *;

-- name: InsertPayment :one
INSERT INTO payments (id, sale_id, method, amount, tendered, reference) VALUES ($1, $2, $3, $4, $5, $6) RETURNING *;

-- name: GetSale :one
SELECT s.*, o.path::text AS path, st.code AS store_code, st.name AS store_name, st.timezone AS store_timezone,
       t.code AS terminal_code, u.name AS cashier_name, cu.name AS customer_name, cu.member_no AS customer_member_no, cu.points_balance AS customer_points
FROM sales s
LEFT JOIN customers cu ON cu.id = s.customer_id
JOIN stores st ON st.id = s.store_id JOIN org_units o ON o.id = st.org_unit_id
JOIN pos_terminals t ON t.id = s.terminal_id JOIN users u ON u.id = s.cashier_id
WHERE s.id = $1 AND s.company_id = $2;

-- name: GetSaleByClientTxn :one
SELECT id FROM sales WHERE terminal_id = $1 AND client_txn_id = $2;

-- name: GetSaleForUpdate :one
SELECT * FROM sales WHERE id = $1 AND company_id = $2 FOR UPDATE;

-- name: ListSaleItems :many
SELECT * FROM sale_items WHERE sale_id = $1 ORDER BY line_no;

-- name: ListPayments :many
SELECT * FROM payments WHERE sale_id = $1 ORDER BY created_at, id;

-- name: ListSales :many
SELECT s.id, s.document_number, s.store_id, st.name AS store_name, s.terminal_id, t.code AS terminal_code, s.cashier_id, u.name AS cashier_name,
       s.status, s.business_date, s.grand_total, s.discount_total, s.refunded_total, s.created_at,
       COALESCE((SELECT string_agg(DISTINCT p.method, ',') FROM payments p WHERE p.sale_id = s.id), '')::text AS methods
FROM sales s
JOIN stores st ON st.id = s.store_id JOIN org_units o ON o.id = st.org_unit_id
JOIN pos_terminals t ON t.id = s.terminal_id JOIN users u ON u.id = s.cashier_id
WHERE s.company_id = sqlc.arg(company_id)
  AND (sqlc.arg(all_scope)::bool OR EXISTS (SELECT 1 FROM unnest(sqlc.arg(scope_paths)::text[]) sp WHERE o.path <@ sp::ltree))
  AND (sqlc.narg(store_id)::uuid IS NULL OR s.store_id = sqlc.narg(store_id)::uuid)
  AND (sqlc.arg(under_path)::text = '' OR o.path <@ sqlc.arg(under_path)::text::ltree)
  AND (sqlc.arg(status)::text = '' OR s.status = sqlc.arg(status)::text)
  AND (sqlc.arg(q)::text = '' OR s.document_number ILIKE '%' || sqlc.arg(q)::text || '%')
  AND (sqlc.narg(shift_id)::uuid IS NULL OR s.shift_id = sqlc.narg(shift_id)::uuid)
  AND (sqlc.narg(date_from)::date IS NULL OR s.business_date >= sqlc.narg(date_from)::date)
  AND (sqlc.narg(date_to)::date IS NULL OR s.business_date <= sqlc.narg(date_to)::date)
ORDER BY s.created_at DESC
LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);

-- name: CountSales :one
SELECT count(*)
FROM sales s
JOIN stores st ON st.id = s.store_id JOIN org_units o ON o.id = st.org_unit_id
WHERE s.company_id = sqlc.arg(company_id)
  AND (sqlc.arg(all_scope)::bool OR EXISTS (SELECT 1 FROM unnest(sqlc.arg(scope_paths)::text[]) sp WHERE o.path <@ sp::ltree))
  AND (sqlc.narg(store_id)::uuid IS NULL OR s.store_id = sqlc.narg(store_id)::uuid)
  AND (sqlc.arg(under_path)::text = '' OR o.path <@ sqlc.arg(under_path)::text::ltree)
  AND (sqlc.arg(status)::text = '' OR s.status = sqlc.arg(status)::text)
  AND (sqlc.arg(q)::text = '' OR s.document_number ILIKE '%' || sqlc.arg(q)::text || '%')
  AND (sqlc.narg(shift_id)::uuid IS NULL OR s.shift_id = sqlc.narg(shift_id)::uuid)
  AND (sqlc.narg(date_from)::date IS NULL OR s.business_date >= sqlc.narg(date_from)::date)
  AND (sqlc.narg(date_to)::date IS NULL OR s.business_date <= sqlc.narg(date_to)::date);

-- name: VoidSale :one
UPDATE sales SET status = 'voided', void_reason = $2, voided_by = $3, voided_at = now()
WHERE id = $1 AND status = 'completed' RETURNING *;

-- name: AddSaleRefunded :exec
UPDATE sales SET refunded_total = refunded_total + $2 WHERE id = $1;

-- name: InsertReturn :one
INSERT INTO sale_returns (id, company_id, store_id, sale_id, shift_id, document_number, reason, refund_method, refund_amount, approved_by, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11) RETURNING *;

-- name: InsertReturnItem :one
INSERT INTO sale_return_items (id, return_id, sale_item_id, qty, amount, restock, reason) VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING *;

-- name: ListReturnsBySale :many
SELECT * FROM sale_returns WHERE sale_id = $1 ORDER BY created_at;

-- name: ListReturnItemsBySale :many
SELECT ri.* FROM sale_return_items ri JOIN sale_returns r ON r.id = ri.return_id WHERE r.sale_id = $1;

-- name: ReturnedQtyBySale :many
SELECT ri.sale_item_id, SUM(ri.qty)::numeric AS qty FROM sale_return_items ri JOIN sale_returns r ON r.id = ri.return_id
WHERE r.sale_id = $1 GROUP BY ri.sale_item_id;

-- name: LookupVariantForSale :one
SELECT v.id AS variant_id, v.sku, v.name AS variant_name, v.status AS variant_status, v.cost,
       p.id AS product_id, p.name AS product_name, p.tax_code, p.status AS product_status, p.category_id, p.brand_id,
       u.allows_decimal, p.is_rfid_tracked
FROM product_variants v JOIN products p ON p.id = v.product_id JOIN uoms u ON u.id = p.uom_id
WHERE v.id = $1 AND v.company_id = $2;

-- name: PrimaryBarcode :one
SELECT barcode FROM barcodes WHERE variant_id = $1 ORDER BY is_primary DESC, barcode LIMIT 1;

-- name: GetStoreAddress :one
SELECT address FROM stores WHERE id = $1;
