-- ===== terminal device credential =====

-- name: UpsertTerminalSyncToken :exec
INSERT INTO terminal_sync_tokens (terminal_id, company_id, token_hash, token_hint) VALUES ($1, $2, $3, $4)
ON CONFLICT (terminal_id) DO UPDATE SET token_hash = EXCLUDED.token_hash, token_hint = EXCLUDED.token_hint, created_at = now(),
    last_sync_at = NULL, snapshot_version = '', pending = 0;

-- name: GetTerminalBySyncToken :one
SELECT t.id, t.company_id, t.store_id, t.code, t.name, t.status, s.name AS store_name, s.status AS store_status
FROM terminal_sync_tokens k JOIN pos_terminals t ON t.id = k.terminal_id JOIN stores s ON s.id = t.store_id
WHERE k.token_hash = $1;

-- name: TouchTerminalSync :exec
UPDATE terminal_sync_tokens SET last_sync_at = now(), last_ip = sqlc.arg(ip), pending = sqlc.arg(pending),
    agent_version = CASE WHEN sqlc.arg(version)::text = '' THEN agent_version ELSE sqlc.arg(version)::text END,
    snapshot_version = CASE WHEN sqlc.arg(snapshot_version)::text = '' THEN snapshot_version ELSE sqlc.arg(snapshot_version)::text END
WHERE terminal_id = sqlc.arg(terminal_id);

-- name: ListTerminalSyncStatus :many
SELECT k.terminal_id, k.token_hint, k.created_at, k.last_sync_at, k.last_ip, k.agent_version, k.pending, k.snapshot_version
FROM terminal_sync_tokens k JOIN pos_terminals t ON t.id = k.terminal_id
WHERE k.company_id = $1;

-- ===== catalogue snapshot =====

-- name: SnapshotItems :many
SELECT v.id AS variant_id, v.sku, p.name AS product_name, v.name AS variant_name, p.tax_code, u.allows_decimal, p.category_id, p.brand_id, p.is_rfid_tracked,
       COALESCE((SELECT array_agg(b.barcode ORDER BY b.is_primary DESC, b.barcode) FROM barcodes b WHERE b.variant_id = v.id), '{}')::text[] AS barcodes
FROM product_variants v JOIN products p ON p.id = v.product_id JOIN uoms u ON u.id = p.uom_id
WHERE v.company_id = $1 AND v.status = 'active' AND p.status = 'active'
ORDER BY v.id;

-- name: SnapshotPriceLists :many
SELECT id, is_default, membership_level_id FROM price_lists WHERE company_id = $1 AND status = 'active' ORDER BY id;

-- name: SnapshotPrices :many
-- Only prices set at the store's org unit or one of its ancestors (the only ones that can ever apply to this store),
-- still open or ending in the future, on active lists. depth = nlevel(path) so the edge can pick "nearest wins".
SELECT p.variant_id, p.price_list_id, p.min_qty, p.price, nlevel(ou.path)::int AS depth, ou.name AS source_name, p.effective_from, p.effective_to
FROM prices p
JOIN org_units ou ON ou.id = p.org_unit_id
JOIN org_units t ON t.id = sqlc.arg(store_org_unit_id)
JOIN price_lists pl ON pl.id = p.price_list_id AND pl.status = 'active'
WHERE p.company_id = sqlc.arg(company_id) AND t.path <@ ou.path
  AND (p.effective_to IS NULL OR p.effective_to > sqlc.arg(since)::timestamptz)
ORDER BY p.variant_id, p.price_list_id, p.min_qty, p.effective_from;

-- name: SnapshotPromotions :many
-- Everything that has not ended and applies to this store (future starts included: the edge may sell after they begin).
SELECT p.* FROM promotions p
WHERE p.company_id = sqlc.arg(company_id) AND p.status = 'active' AND (p.ends_at IS NULL OR p.ends_at > sqlc.arg(at)::timestamptz)
  AND (NOT EXISTS (SELECT 1 FROM promotion_scopes s WHERE s.promotion_id = p.id AND s.scope_type = 'org_unit')
       OR EXISTS (SELECT 1 FROM promotion_scopes s JOIN org_units so ON so.id = s.scope_id JOIN org_units t ON t.id = sqlc.arg(store_org_unit_id)
                  WHERE s.promotion_id = p.id AND s.scope_type = 'org_unit' AND t.path <@ so.path))
ORDER BY p.id;

-- name: SnapshotMembers :many
-- Members who have shopped in this store recently (the people who will come back), capped so the snapshot stays small.
SELECT c.id, c.member_no, c.name, c.phone, c.level_id, l.name AS level_name, l.earn_multiplier, c.points_balance
FROM customers c JOIN membership_levels l ON l.id = c.level_id
WHERE c.company_id = sqlc.arg(company_id) AND c.status = 'active'
  AND (c.home_store_id = sqlc.arg(store_id) OR EXISTS (SELECT 1 FROM sales s WHERE s.customer_id = c.id AND s.store_id = sqlc.arg(store_id) AND s.created_at > sqlc.arg(since)::timestamptz))
ORDER BY c.last_purchase_at DESC NULLS LAST, c.id
LIMIT sqlc.arg(row_limit);

-- ===== applying commands =====

-- name: GetSaleByClientTxnFull :one
SELECT id, document_number FROM sales WHERE terminal_id = $1 AND client_txn_id = $2;

-- name: InsertOfflineSale :one
INSERT INTO sales (id, company_id, store_id, terminal_id, shift_id, cashier_id, document_number, client_txn_id, business_date,
                   subtotal, discount_total, tax_total, grand_total, paid_total, change_amount, customer_id, promo_discount, points_earned,
                   offline, occurred_at, local_number, snapshot_version, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, true, $19, $20, $21, $19) RETURNING *;

-- name: CreateOfflineShift :one
INSERT INTO cashier_shifts (id, company_id, store_id, terminal_id, cashier_id, opening_cash, opened_at, client_shift_id, offline)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, true) RETURNING *;

-- name: GetShiftByClientID :one
SELECT * FROM cashier_shifts WHERE terminal_id = $1 AND client_shift_id = $2;

-- name: CloseShiftAt :one
UPDATE cashier_shifts SET status = 'closed', closed_at = $7, closed_by = $2, expected_cash = $3, counted_cash = $4, variance = $5, note = $6
WHERE id = $1 AND status = 'open' RETURNING *;

-- name: InsertOfflineException :one
INSERT INTO offline_exceptions (id, company_id, store_id, terminal_id, sale_id, shift_id, type, severity, title, detail, dedupe_key)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT (company_id, dedupe_key) WHERE dedupe_key <> '' DO NOTHING
RETURNING id;

-- name: ListOfflineExceptions :many
SELECT e.id, e.store_id, e.terminal_id, e.sale_id, e.shift_id, e.type, e.severity, e.title, e.detail, e.status, e.created_at, e.reviewed_at, e.review_note,
       s.name AS store_name, COALESCE(t.code, '') AS terminal_code, COALESCE(sa.document_number, '') AS document_number, COALESCE(sa.local_number, '') AS local_number,
       COALESCE(sa.grand_total, 0)::numeric AS grand_total, COALESCE(u.name, '') AS reviewed_by_name
FROM offline_exceptions e
JOIN stores s ON s.id = e.store_id JOIN org_units o ON o.id = s.org_unit_id
LEFT JOIN pos_terminals t ON t.id = e.terminal_id
LEFT JOIN sales sa ON sa.id = e.sale_id
LEFT JOIN users u ON u.id = e.reviewed_by
WHERE e.company_id = sqlc.arg(company_id)
  AND (sqlc.arg(all_scope)::bool OR EXISTS (SELECT 1 FROM unnest(sqlc.arg(scope_paths)::text[]) sp WHERE o.path <@ sp::ltree))
  AND (sqlc.narg(store_id)::uuid IS NULL OR e.store_id = sqlc.narg(store_id)::uuid)
  AND (sqlc.arg(status)::text = '' OR e.status = sqlc.arg(status)::text)
  AND (sqlc.arg(type)::text = '' OR e.type = sqlc.arg(type)::text)
ORDER BY e.created_at DESC
LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);

-- name: CountOfflineExceptions :one
SELECT count(*) FROM offline_exceptions e
JOIN stores s ON s.id = e.store_id JOIN org_units o ON o.id = s.org_unit_id
WHERE e.company_id = sqlc.arg(company_id)
  AND (sqlc.arg(all_scope)::bool OR EXISTS (SELECT 1 FROM unnest(sqlc.arg(scope_paths)::text[]) sp WHERE o.path <@ sp::ltree))
  AND (sqlc.narg(store_id)::uuid IS NULL OR e.store_id = sqlc.narg(store_id)::uuid)
  AND (sqlc.arg(status)::text = '' OR e.status = sqlc.arg(status)::text)
  AND (sqlc.arg(type)::text = '' OR e.type = sqlc.arg(type)::text);

-- name: GetOfflineException :one
SELECT e.*, o.path::text AS path FROM offline_exceptions e JOIN stores s ON s.id = e.store_id JOIN org_units o ON o.id = s.org_unit_id
WHERE e.id = $1 AND e.company_id = $2;

-- name: ReviewOfflineException :exec
UPDATE offline_exceptions SET status = $2, reviewed_by = $3, reviewed_at = now(), review_note = $4 WHERE id = $1 AND status = 'open';

-- name: UpdateSaleCustomerPoints :exec
UPDATE sales SET points_earned = $2 WHERE id = $1;
