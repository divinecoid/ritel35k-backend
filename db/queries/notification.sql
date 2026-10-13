-- name: InsertNotification :exec
INSERT INTO notifications (id, company_id, user_id, store_id, type, severity, title, body, entity_type, entity_id, dedupe_key)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT (user_id, dedupe_key) WHERE dedupe_key <> '' DO NOTHING;

-- name: NotificationRecipients :many
-- Active users that hold `perm` with a role assignment covering the store (a company-wide assignment covers every store).
SELECT DISTINCT u.id
FROM users u
JOIN user_role_scopes urs ON urs.user_id = u.id
JOIN role_permissions rp ON rp.role_id = urs.role_id AND rp.permission_code = sqlc.arg(perm)::text
LEFT JOIN org_units ou ON ou.id = urs.org_unit_id
WHERE u.company_id = sqlc.arg(company_id) AND u.status = 'active'
  AND (urs.org_unit_id IS NULL
       OR ou.path @> (SELECT o.path FROM stores s JOIN org_units o ON o.id = s.org_unit_id WHERE s.id = sqlc.narg(store_id)::uuid));

-- name: ListNotifications :many
SELECT n.id, n.store_id, n.type, n.severity, n.title, n.body, n.entity_type, n.entity_id, n.created_at, n.read_at, COALESCE(s.name, '') AS store_name
FROM notifications n LEFT JOIN stores s ON s.id = n.store_id
WHERE n.user_id = sqlc.arg(user_id) AND (NOT sqlc.arg(unread_only)::bool OR n.read_at IS NULL)
ORDER BY n.created_at DESC
LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);

-- name: CountNotifications :one
SELECT count(*) FROM notifications n WHERE n.user_id = sqlc.arg(user_id) AND (NOT sqlc.arg(unread_only)::bool OR n.read_at IS NULL);

-- name: MarkNotificationRead :exec
UPDATE notifications SET read_at = now() WHERE id = $1 AND user_id = $2 AND read_at IS NULL;

-- name: MarkAllNotificationsRead :exec
UPDATE notifications SET read_at = now() WHERE user_id = $1 AND read_at IS NULL;

-- name: PurgeOldNotifications :execrows
DELETE FROM notifications WHERE created_at < $1;

-- name: LowStockByStore :many
SELECT s.company_id, s.id AS store_id, s.name AS store_name, count(*)::bigint AS skus
FROM inventory_balances b
JOIN product_variants v ON v.id = b.variant_id AND v.status = 'active'
JOIN stores s ON s.id = b.store_id AND s.status = 'active'
WHERE v.reorder_point > 0 AND b.on_hand <= v.reorder_point
GROUP BY s.company_id, s.id, s.name;

-- ===== Approval inbox: documents waiting for a decision, scoped by the approver's grant for that document type =====

-- name: PendingPurchaseOrders :many
SELECT po.id, po.document_number, po.total, po.submitted_at, po.created_by, u.name AS requested_by,
       COALESCE(s.name, '') AS store_name, w.name AS warehouse_name, sup.name AS supplier_name
FROM purchase_orders po
JOIN warehouses w ON w.id = po.warehouse_id
JOIN suppliers sup ON sup.id = po.supplier_id
JOIN users u ON u.id = po.created_by
LEFT JOIN stores s ON s.id = po.store_id
LEFT JOIN org_units o ON o.id = s.org_unit_id
WHERE po.company_id = sqlc.arg(company_id) AND po.status = 'submitted'
  AND (sqlc.arg(all_scope)::bool OR (po.store_id IS NOT NULL AND EXISTS (SELECT 1 FROM unnest(sqlc.arg(scope_paths)::text[]) sp WHERE o.path <@ sp::ltree)))
ORDER BY po.submitted_at NULLS LAST
LIMIT 100;

-- name: PendingTransfers :many
SELECT t.id, t.document_number, t.submitted_at, t.created_by, u.name AS requested_by,
       COALESCE(fs.name, fw.name) AS from_name, COALESCE(ts.name, tw.name) AS to_name,
       (SELECT count(*) FROM stock_transfer_items i WHERE i.transfer_id = t.id) AS line_count
FROM stock_transfers t
JOIN warehouses fw ON fw.id = t.from_warehouse_id JOIN warehouses tw ON tw.id = t.to_warehouse_id
JOIN users u ON u.id = t.created_by
LEFT JOIN stores fs ON fs.id = t.from_store_id LEFT JOIN org_units fo ON fo.id = fs.org_unit_id
LEFT JOIN stores ts ON ts.id = t.to_store_id LEFT JOIN org_units tpo ON tpo.id = ts.org_unit_id
WHERE t.company_id = sqlc.arg(company_id) AND t.status = 'submitted'
  AND (sqlc.arg(all_scope)::bool
       OR (t.from_store_id IS NOT NULL AND EXISTS (SELECT 1 FROM unnest(sqlc.arg(scope_paths)::text[]) sp WHERE fo.path <@ sp::ltree)))
ORDER BY t.submitted_at NULLS LAST
LIMIT 100;

-- name: PendingStockCounts :many
SELECT c.id, c.document_number, c.kind, c.submitted_at, c.submitted_by, c.created_by, u.name AS requested_by,
       COALESCE(s.name, '') AS store_name, w.name AS warehouse_name,
       (SELECT COALESCE(sum(abs(i.variance) * i.unit_cost), 0) FROM stock_count_items i WHERE i.count_id = c.id)::numeric AS variance_value
FROM stock_counts c
JOIN warehouses w ON w.id = c.warehouse_id
JOIN users u ON u.id = COALESCE(c.submitted_by, c.created_by)
LEFT JOIN stores s ON s.id = c.store_id
LEFT JOIN org_units o ON o.id = s.org_unit_id
WHERE c.company_id = sqlc.arg(company_id) AND c.status = 'submitted'
  AND (sqlc.arg(all_scope)::bool OR (c.store_id IS NOT NULL AND EXISTS (SELECT 1 FROM unnest(sqlc.arg(scope_paths)::text[]) sp WHERE o.path <@ sp::ltree)))
ORDER BY c.submitted_at NULLS LAST
LIMIT 100;

-- name: CountCashierVoids :one
SELECT count(*) FROM sales WHERE cashier_id = $1 AND business_date = $2 AND status = 'voided';

-- name: GetTransferStores :one
SELECT from_store_id, to_store_id FROM stock_transfers WHERE id = $1;
