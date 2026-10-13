-- name: ApplyBalanceDelta :one
-- Atomic stock change. Fails (no row) if it would go below zero unless allow_negative.
-- Moving-average cost is recomputed on inbound movements that carry a cost.
UPDATE inventory_balances SET
    on_hand = on_hand + sqlc.arg(delta)::numeric,
    avg_cost = CASE
        WHEN sqlc.arg(delta)::numeric > 0 AND sqlc.arg(unit_cost)::numeric > 0 THEN
            CASE WHEN on_hand <= 0 THEN sqlc.arg(unit_cost)::numeric
                 ELSE ((on_hand * avg_cost) + (sqlc.arg(delta)::numeric * sqlc.arg(unit_cost)::numeric)) / (on_hand + sqlc.arg(delta)::numeric)
            END
        ELSE avg_cost END,
    version = version + 1,
    updated_at = now()
WHERE warehouse_id = sqlc.arg(warehouse_id) AND variant_id = sqlc.arg(variant_id)
  AND (on_hand + sqlc.arg(delta)::numeric >= 0 OR sqlc.arg(allow_negative)::bool)
RETURNING *;

-- name: InsertBalance :one
INSERT INTO inventory_balances (company_id, store_id, warehouse_id, variant_id, on_hand, avg_cost)
VALUES (sqlc.arg(company_id), sqlc.narg(store_id), sqlc.arg(warehouse_id), sqlc.arg(variant_id), sqlc.arg(delta)::numeric,
        CASE WHEN sqlc.arg(delta)::numeric > 0 THEN sqlc.arg(unit_cost)::numeric ELSE 0 END)
ON CONFLICT (warehouse_id, variant_id) DO NOTHING
RETURNING *;

-- name: InsertMovement :exec
INSERT INTO inventory_movements (id, company_id, store_id, warehouse_id, variant_id, movement_type, qty_delta, unit_cost, balance_after,
                                 ref_type, ref_id, document_number, note, created_by, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, COALESCE(sqlc.narg(at)::timestamptz, now()));

-- name: GetDefaultWarehouse :one
SELECT * FROM warehouses WHERE store_id = $1 AND is_default AND status = 'active';

-- name: ListBalances :many
SELECT b.company_id, b.store_id, b.warehouse_id, b.variant_id, b.on_hand, b.reserved, b.in_transit, b.damaged, b.quarantine,
       b.avg_cost, b.updated_at,
       v.sku, v.name AS variant_name, v.reorder_point, v.min_stock,
       p.id AS product_id, p.name AS product_name,
       w.code AS warehouse_code, w.name AS warehouse_name,
       s.name AS store_name, s.code AS store_code
FROM inventory_balances b
JOIN product_variants v ON v.id = b.variant_id
JOIN products p ON p.id = v.product_id
JOIN warehouses w ON w.id = b.warehouse_id
LEFT JOIN stores s ON s.id = b.store_id
LEFT JOIN org_units o ON o.id = s.org_unit_id
WHERE b.company_id = sqlc.arg(company_id)
  AND (sqlc.arg(all_scope)::bool OR (b.store_id IS NOT NULL AND EXISTS (
         SELECT 1 FROM unnest(sqlc.arg(scope_paths)::text[]) sp WHERE o.path <@ sp::ltree)))
  AND (sqlc.narg(store_id)::uuid IS NULL OR b.store_id = sqlc.narg(store_id)::uuid)
  AND (sqlc.narg(warehouse_id)::uuid IS NULL OR b.warehouse_id = sqlc.narg(warehouse_id)::uuid)
  AND (sqlc.narg(variant_id)::uuid IS NULL OR b.variant_id = sqlc.narg(variant_id)::uuid)
  AND (sqlc.arg(under_path)::text = '' OR o.path <@ sqlc.arg(under_path)::text::ltree)
  AND (sqlc.arg(q)::text = '' OR v.sku ILIKE '%' || sqlc.arg(q)::text || '%' OR p.name ILIKE '%' || sqlc.arg(q)::text || '%'
       OR EXISTS (SELECT 1 FROM barcodes bc WHERE bc.variant_id = v.id AND bc.barcode = sqlc.arg(q)::text))
  AND (NOT sqlc.arg(low_only)::bool OR b.on_hand <= v.reorder_point)
ORDER BY p.name, v.sku, s.name
LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);

-- name: CountBalances :one
SELECT count(*)
FROM inventory_balances b
JOIN product_variants v ON v.id = b.variant_id
JOIN products p ON p.id = v.product_id
LEFT JOIN stores s ON s.id = b.store_id
LEFT JOIN org_units o ON o.id = s.org_unit_id
WHERE b.company_id = sqlc.arg(company_id)
  AND (sqlc.arg(all_scope)::bool OR (b.store_id IS NOT NULL AND EXISTS (
         SELECT 1 FROM unnest(sqlc.arg(scope_paths)::text[]) sp WHERE o.path <@ sp::ltree)))
  AND (sqlc.narg(store_id)::uuid IS NULL OR b.store_id = sqlc.narg(store_id)::uuid)
  AND (sqlc.narg(warehouse_id)::uuid IS NULL OR b.warehouse_id = sqlc.narg(warehouse_id)::uuid)
  AND (sqlc.narg(variant_id)::uuid IS NULL OR b.variant_id = sqlc.narg(variant_id)::uuid)
  AND (sqlc.arg(under_path)::text = '' OR o.path <@ sqlc.arg(under_path)::text::ltree)
  AND (sqlc.arg(q)::text = '' OR v.sku ILIKE '%' || sqlc.arg(q)::text || '%' OR p.name ILIKE '%' || sqlc.arg(q)::text || '%'
       OR EXISTS (SELECT 1 FROM barcodes bc WHERE bc.variant_id = v.id AND bc.barcode = sqlc.arg(q)::text))
  AND (NOT sqlc.arg(low_only)::bool OR b.on_hand <= v.reorder_point);

-- name: ListMovements :many
SELECT m.*, v.sku, p.name AS product_name, w.code AS warehouse_code
FROM inventory_movements m
JOIN product_variants v ON v.id = m.variant_id
JOIN products p ON p.id = v.product_id
JOIN warehouses w ON w.id = m.warehouse_id
LEFT JOIN stores s ON s.id = m.store_id
LEFT JOIN org_units o ON o.id = s.org_unit_id
WHERE m.company_id = sqlc.arg(company_id)
  AND (sqlc.arg(all_scope)::bool OR (m.store_id IS NOT NULL AND EXISTS (
         SELECT 1 FROM unnest(sqlc.arg(scope_paths)::text[]) sp WHERE o.path <@ sp::ltree)))
  AND (sqlc.narg(store_id)::uuid IS NULL OR m.store_id = sqlc.narg(store_id)::uuid)
  AND (sqlc.narg(variant_id)::uuid IS NULL OR m.variant_id = sqlc.narg(variant_id)::uuid)
  AND (sqlc.arg(movement_type)::text = '' OR m.movement_type = sqlc.arg(movement_type)::text)
  AND (sqlc.narg(from_time)::timestamptz IS NULL OR m.created_at >= sqlc.narg(from_time)::timestamptz)
  AND (sqlc.narg(to_time)::timestamptz IS NULL OR m.created_at < sqlc.narg(to_time)::timestamptz)
ORDER BY m.created_at DESC, m.id DESC
LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);

-- name: ReconcileBalance :one
-- Ledger sum vs cached balance for one item (used by tests and the nightly reconcile job).
SELECT COALESCE(SUM(qty_delta), 0)::numeric AS ledger_sum FROM inventory_movements WHERE warehouse_id = $1 AND variant_id = $2;
