-- ============================================================ purchase orders
-- name: InsertPO :one
INSERT INTO purchase_orders (id, company_id, store_id, warehouse_id, supplier_id, document_number, expected_date, note, total, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING *;

-- name: GetPO :one
SELECT po.*, sp.name AS supplier_name, w.code AS warehouse_code, w.name AS warehouse_name, s.name AS store_name, COALESCE(o.path::text, '')::text AS path
FROM purchase_orders po
JOIN suppliers sp ON sp.id = po.supplier_id
JOIN warehouses w ON w.id = po.warehouse_id
LEFT JOIN stores s ON s.id = po.store_id LEFT JOIN org_units o ON o.id = s.org_unit_id
WHERE po.id = $1 AND po.company_id = $2;

-- name: GetPOForUpdate :one
SELECT * FROM purchase_orders WHERE id = $1 AND company_id = $2 FOR UPDATE;

-- name: ListPOs :many
SELECT po.id, po.document_number, po.status, po.total, po.expected_date, po.created_at, po.store_id, po.warehouse_id,
       sp.name AS supplier_name, w.code AS warehouse_code, s.name AS store_name
FROM purchase_orders po
JOIN suppliers sp ON sp.id = po.supplier_id
JOIN warehouses w ON w.id = po.warehouse_id
LEFT JOIN stores s ON s.id = po.store_id LEFT JOIN org_units o ON o.id = s.org_unit_id
WHERE po.company_id = sqlc.arg(company_id)
  AND (sqlc.arg(all_scope)::bool OR (po.store_id IS NOT NULL AND EXISTS (SELECT 1 FROM unnest(sqlc.arg(scope_paths)::text[]) sp2 WHERE o.path <@ sp2::ltree)))
  AND (sqlc.narg(store_id)::uuid IS NULL OR po.store_id = sqlc.narg(store_id)::uuid)
  AND (sqlc.narg(supplier_id)::uuid IS NULL OR po.supplier_id = sqlc.narg(supplier_id)::uuid)
  AND (sqlc.arg(status)::text = '' OR po.status = sqlc.arg(status)::text)
  AND (sqlc.arg(q)::text = '' OR po.document_number ILIKE '%' || sqlc.arg(q)::text || '%')
ORDER BY po.created_at DESC
LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);

-- name: CountPOs :one
SELECT count(*)
FROM purchase_orders po
LEFT JOIN stores s ON s.id = po.store_id LEFT JOIN org_units o ON o.id = s.org_unit_id
WHERE po.company_id = sqlc.arg(company_id)
  AND (sqlc.arg(all_scope)::bool OR (po.store_id IS NOT NULL AND EXISTS (SELECT 1 FROM unnest(sqlc.arg(scope_paths)::text[]) sp2 WHERE o.path <@ sp2::ltree)))
  AND (sqlc.narg(store_id)::uuid IS NULL OR po.store_id = sqlc.narg(store_id)::uuid)
  AND (sqlc.narg(supplier_id)::uuid IS NULL OR po.supplier_id = sqlc.narg(supplier_id)::uuid)
  AND (sqlc.arg(status)::text = '' OR po.status = sqlc.arg(status)::text)
  AND (sqlc.arg(q)::text = '' OR po.document_number ILIKE '%' || sqlc.arg(q)::text || '%');

-- name: UpdatePODraft :exec
UPDATE purchase_orders SET supplier_id = $2, warehouse_id = $3, store_id = $4, expected_date = $5, note = $6, total = $7, version = version + 1, updated_at = now()
WHERE id = $1;

-- name: SetPOStatus :exec
UPDATE purchase_orders SET status = $2, updated_at = now(),
    submitted_at = COALESCE(sqlc.narg(submitted_at), submitted_at),
    approved_by = COALESCE(sqlc.narg(approved_by), approved_by),
    approved_at = COALESCE(sqlc.narg(approved_at), approved_at),
    cancel_reason = COALESCE(sqlc.narg(cancel_reason), cancel_reason)
WHERE id = $1;

-- name: InsertPOItem :one
INSERT INTO purchase_order_items (id, po_id, line_no, variant_id, qty_ordered, unit_price) VALUES ($1, $2, $3, $4, $5, $6) RETURNING *;

-- name: DeletePOItems :exec
DELETE FROM purchase_order_items WHERE po_id = $1;

-- name: ListPOItems :many
SELECT i.*, v.sku, p.name AS product_name FROM purchase_order_items i
JOIN product_variants v ON v.id = i.variant_id JOIN products p ON p.id = v.product_id
WHERE i.po_id = $1 ORDER BY i.line_no;

-- name: ListPOItemsForUpdate :many
SELECT * FROM purchase_order_items WHERE po_id = $1 ORDER BY line_no FOR UPDATE;

-- name: AddPOItemReceived :exec
UPDATE purchase_order_items SET qty_received = qty_received + $2 WHERE id = $1;

-- name: SupplierPrice :one
SELECT purchase_price FROM supplier_products WHERE supplier_id = $1 AND variant_id = $2;

-- ============================================================ goods receipts
-- name: InsertGRN :one
INSERT INTO goods_receipts (id, company_id, store_id, warehouse_id, supplier_id, po_id, document_number, supplier_ref, received_date, note, total_cost, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12) RETURNING *;

-- name: InsertGRNItem :one
INSERT INTO goods_receipt_items (id, grn_id, line_no, po_item_id, variant_id, qty, unit_cost) VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING *;

-- name: GetGRN :one
SELECT g.*, sp.name AS supplier_name, w.code AS warehouse_code, s.name AS store_name, po.document_number AS po_number, COALESCE(o.path::text, '')::text AS path
FROM goods_receipts g
JOIN suppliers sp ON sp.id = g.supplier_id JOIN warehouses w ON w.id = g.warehouse_id
LEFT JOIN purchase_orders po ON po.id = g.po_id
LEFT JOIN stores s ON s.id = g.store_id LEFT JOIN org_units o ON o.id = s.org_unit_id
WHERE g.id = $1 AND g.company_id = $2;

-- name: ListGRNItems :many
SELECT i.*, v.sku, p.name AS product_name FROM goods_receipt_items i
JOIN product_variants v ON v.id = i.variant_id JOIN products p ON p.id = v.product_id
WHERE i.grn_id = $1 ORDER BY i.line_no;

-- name: ListGRNs :many
SELECT g.id, g.document_number, g.supplier_ref, g.received_date, g.total_cost, g.created_at, g.store_id, g.po_id,
       sp.name AS supplier_name, w.code AS warehouse_code, s.name AS store_name, po.document_number AS po_number
FROM goods_receipts g
JOIN suppliers sp ON sp.id = g.supplier_id JOIN warehouses w ON w.id = g.warehouse_id
LEFT JOIN purchase_orders po ON po.id = g.po_id
LEFT JOIN stores s ON s.id = g.store_id LEFT JOIN org_units o ON o.id = s.org_unit_id
WHERE g.company_id = sqlc.arg(company_id)
  AND (sqlc.arg(all_scope)::bool OR (g.store_id IS NOT NULL AND EXISTS (SELECT 1 FROM unnest(sqlc.arg(scope_paths)::text[]) sp2 WHERE o.path <@ sp2::ltree)))
  AND (sqlc.narg(store_id)::uuid IS NULL OR g.store_id = sqlc.narg(store_id)::uuid)
  AND (sqlc.narg(po_id)::uuid IS NULL OR g.po_id = sqlc.narg(po_id)::uuid)
ORDER BY g.created_at DESC
LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);

-- name: GRNReturnedQty :many
SELECT ri.variant_id, SUM(ri.qty)::numeric AS qty FROM purchase_return_items ri
JOIN purchase_returns r ON r.id = ri.return_id WHERE r.grn_id = $1 GROUP BY ri.variant_id;

-- name: InsertPurchaseReturn :one
INSERT INTO purchase_returns (id, company_id, store_id, warehouse_id, supplier_id, grn_id, document_number, reason, total_cost, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING *;

-- name: InsertPurchaseReturnItem :one
INSERT INTO purchase_return_items (id, return_id, line_no, variant_id, qty, unit_cost) VALUES ($1, $2, $3, $4, $5, $6) RETURNING *;

-- name: ListPurchaseReturns :many
SELECT r.id, r.document_number, r.reason, r.total_cost, r.created_at, r.store_id, sp.name AS supplier_name, w.code AS warehouse_code, s.name AS store_name
FROM purchase_returns r
JOIN suppliers sp ON sp.id = r.supplier_id JOIN warehouses w ON w.id = r.warehouse_id
LEFT JOIN stores s ON s.id = r.store_id LEFT JOIN org_units o ON o.id = s.org_unit_id
WHERE r.company_id = sqlc.arg(company_id)
  AND (sqlc.arg(all_scope)::bool OR (r.store_id IS NOT NULL AND EXISTS (SELECT 1 FROM unnest(sqlc.arg(scope_paths)::text[]) sp2 WHERE o.path <@ sp2::ltree)))
ORDER BY r.created_at DESC
LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);

-- ============================================================ transfers
-- name: InsertTransfer :one
INSERT INTO stock_transfers (id, company_id, from_warehouse_id, to_warehouse_id, from_store_id, to_store_id, document_number, note, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING *;

-- name: GetTransfer :one
SELECT t.*, fw.code AS from_code, fw.name AS from_name, tw.code AS to_code, tw.name AS to_name,
       COALESCE(fo.path::text, '')::text AS from_path, COALESCE(tpo.path::text, '')::text AS to_path
FROM stock_transfers t
JOIN warehouses fw ON fw.id = t.from_warehouse_id JOIN warehouses tw ON tw.id = t.to_warehouse_id
LEFT JOIN stores fs ON fs.id = t.from_store_id LEFT JOIN org_units fo ON fo.id = fs.org_unit_id
LEFT JOIN stores ts ON ts.id = t.to_store_id LEFT JOIN org_units tpo ON tpo.id = ts.org_unit_id
WHERE t.id = $1 AND t.company_id = $2;

-- name: GetTransferForUpdate :one
SELECT * FROM stock_transfers WHERE id = $1 AND company_id = $2 FOR UPDATE;

-- name: ListTransfers :many
SELECT t.id, t.document_number, t.status, t.created_at, t.from_store_id, t.to_store_id, fw.code AS from_code, tw.code AS to_code,
       fs.name AS from_store_name, ts.name AS to_store_name,
       (SELECT count(*) FROM stock_transfer_items i WHERE i.transfer_id = t.id) AS line_count
FROM stock_transfers t
JOIN warehouses fw ON fw.id = t.from_warehouse_id JOIN warehouses tw ON tw.id = t.to_warehouse_id
LEFT JOIN stores fs ON fs.id = t.from_store_id LEFT JOIN org_units fo ON fo.id = fs.org_unit_id
LEFT JOIN stores ts ON ts.id = t.to_store_id LEFT JOIN org_units tpo ON tpo.id = ts.org_unit_id
WHERE t.company_id = sqlc.arg(company_id)
  AND (sqlc.arg(all_scope)::bool
       OR (t.from_store_id IS NOT NULL AND EXISTS (SELECT 1 FROM unnest(sqlc.arg(scope_paths)::text[]) sp WHERE fo.path <@ sp::ltree))
       OR (t.to_store_id IS NOT NULL AND EXISTS (SELECT 1 FROM unnest(sqlc.arg(scope_paths)::text[]) sp WHERE tpo.path <@ sp::ltree)))
  AND (sqlc.arg(status)::text = '' OR t.status = sqlc.arg(status)::text)
ORDER BY t.created_at DESC
LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);

-- name: SetTransferStatus :exec
UPDATE stock_transfers SET status = $2, updated_at = now(),
    submitted_at = COALESCE(sqlc.narg(submitted_at), submitted_at),
    approved_by = COALESCE(sqlc.narg(approved_by), approved_by), approved_at = COALESCE(sqlc.narg(approved_at), approved_at),
    shipped_by = COALESCE(sqlc.narg(shipped_by), shipped_by), shipped_at = COALESCE(sqlc.narg(shipped_at), shipped_at),
    received_by = COALESCE(sqlc.narg(received_by), received_by), received_at = COALESCE(sqlc.narg(received_at), received_at),
    cancel_reason = COALESCE(sqlc.narg(cancel_reason), cancel_reason)
WHERE id = $1;

-- name: InsertTransferItem :one
INSERT INTO stock_transfer_items (id, transfer_id, line_no, variant_id, qty_requested) VALUES ($1, $2, $3, $4, $5) RETURNING *;

-- name: DeleteTransferItems :exec
DELETE FROM stock_transfer_items WHERE transfer_id = $1;

-- name: ListTransferItems :many
SELECT i.*, v.sku, p.name AS product_name FROM stock_transfer_items i
JOIN product_variants v ON v.id = i.variant_id JOIN products p ON p.id = v.product_id
WHERE i.transfer_id = $1 ORDER BY i.line_no;

-- name: ShipTransferItem :exec
UPDATE stock_transfer_items SET qty_shipped = $2, unit_cost = $3 WHERE id = $1;

-- name: ReceiveTransferItem :exec
UPDATE stock_transfer_items SET qty_received = $2 WHERE id = $1;

-- name: AddInTransit :exec
INSERT INTO inventory_balances (company_id, store_id, warehouse_id, variant_id, in_transit)
VALUES (sqlc.arg(company_id), sqlc.narg(store_id), sqlc.arg(warehouse_id), sqlc.arg(variant_id), sqlc.arg(delta)::numeric)
ON CONFLICT (warehouse_id, variant_id) DO UPDATE SET in_transit = inventory_balances.in_transit + EXCLUDED.in_transit, version = inventory_balances.version + 1, updated_at = now();

-- ============================================================ stock counts
-- name: InsertCount :one
INSERT INTO stock_counts (id, company_id, store_id, warehouse_id, document_number, kind, note, created_by) VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING *;

-- name: GetCount :one
SELECT c.*, w.code AS warehouse_code, w.name AS warehouse_name, s.name AS store_name, COALESCE(o.path::text, '')::text AS path
FROM stock_counts c JOIN warehouses w ON w.id = c.warehouse_id
LEFT JOIN stores s ON s.id = c.store_id LEFT JOIN org_units o ON o.id = s.org_unit_id
WHERE c.id = $1 AND c.company_id = $2;

-- name: GetCountForUpdate :one
SELECT * FROM stock_counts WHERE id = $1 AND company_id = $2 FOR UPDATE;

-- name: ListCounts :many
SELECT c.id, c.document_number, c.kind, c.status, c.snapshot_at, c.created_at, c.store_id, w.code AS warehouse_code, s.name AS store_name,
       (SELECT count(*) FROM stock_count_items i WHERE i.count_id = c.id) AS item_count,
       (SELECT count(*) FROM stock_count_items i WHERE i.count_id = c.id AND i.counted_qty IS NOT NULL) AS counted_count
FROM stock_counts c JOIN warehouses w ON w.id = c.warehouse_id
LEFT JOIN stores s ON s.id = c.store_id LEFT JOIN org_units o ON o.id = s.org_unit_id
WHERE c.company_id = sqlc.arg(company_id)
  AND (sqlc.arg(all_scope)::bool OR (c.store_id IS NOT NULL AND EXISTS (SELECT 1 FROM unnest(sqlc.arg(scope_paths)::text[]) sp WHERE o.path <@ sp::ltree)))
  AND (sqlc.narg(store_id)::uuid IS NULL OR c.store_id = sqlc.narg(store_id)::uuid)
  AND (sqlc.arg(status)::text = '' OR c.status = sqlc.arg(status)::text)
ORDER BY c.created_at DESC
LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);

-- name: SnapshotCountItems :execrows
-- Freezes system quantities (and cost) for the items in scope.
INSERT INTO stock_count_items (id, count_id, variant_id, system_qty, unit_cost)
SELECT gen_random_uuid(), sqlc.arg(count_id), b.variant_id, b.on_hand, b.avg_cost
FROM inventory_balances b
JOIN product_variants v ON v.id = b.variant_id
JOIN products p ON p.id = v.product_id
WHERE b.warehouse_id = sqlc.arg(warehouse_id)
  AND (sqlc.narg(category_id)::uuid IS NULL OR p.category_id = sqlc.narg(category_id)::uuid)
  AND (cardinality(sqlc.arg(variant_ids)::uuid[]) = 0 OR b.variant_id = ANY(sqlc.arg(variant_ids)::uuid[]))
ON CONFLICT DO NOTHING;

-- name: InsertCountItemIfMissing :exec
INSERT INTO stock_count_items (id, count_id, variant_id, system_qty, unit_cost) VALUES ($1, $2, $3, $4, $5) ON CONFLICT DO NOTHING;

-- name: ListCountItems :many
SELECT i.*, v.sku, p.name AS product_name,
       (SELECT barcode FROM barcodes b WHERE b.variant_id = i.variant_id ORDER BY is_primary DESC LIMIT 1) AS barcode
FROM stock_count_items i JOIN product_variants v ON v.id = i.variant_id JOIN products p ON p.id = v.product_id
WHERE i.count_id = $1 ORDER BY p.name, v.sku;

-- name: SetCountedQty :execrows
UPDATE stock_count_items SET counted_qty = $3, counted_at = now() WHERE count_id = $1 AND variant_id = $2;

-- name: AddCountedQty :one
UPDATE stock_count_items SET counted_qty = COALESCE(counted_qty, 0) + $3, counted_at = now() WHERE count_id = $1 AND variant_id = $2 RETURNING counted_qty;

-- name: SetCountVariance :exec
UPDATE stock_count_items SET expected_qty = $2, variance = $3 WHERE id = $1;

-- name: MovementsSince :one
SELECT COALESCE(SUM(qty_delta), 0)::numeric AS qty FROM inventory_movements
WHERE warehouse_id = $1 AND variant_id = $2 AND created_at > $3 AND created_at <= $4;

-- name: SetCountStatus :exec
UPDATE stock_counts SET status = $2,
    submitted_by = COALESCE(sqlc.narg(submitted_by), submitted_by), submitted_at = COALESCE(sqlc.narg(submitted_at), submitted_at),
    approved_by = COALESCE(sqlc.narg(approved_by), approved_by), approved_at = COALESCE(sqlc.narg(approved_at), approved_at)
WHERE id = $1;
