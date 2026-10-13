-- ===== readers =====

-- name: CreateReader :one
INSERT INTO rfid_readers (id, company_id, store_id, code, name, kind, token_hash, token_hint, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING *;

-- name: GetReaderByToken :one
SELECT r.*, s.name AS store_name, o.path::text AS path FROM rfid_readers r
JOIN stores s ON s.id = r.store_id JOIN org_units o ON o.id = s.org_unit_id
WHERE r.token_hash = $1;

-- name: GetReader :one
SELECT r.*, s.name AS store_name, o.path::text AS path FROM rfid_readers r
JOIN stores s ON s.id = r.store_id JOIN org_units o ON o.id = s.org_unit_id
WHERE r.id = $1 AND r.company_id = $2;

-- name: ListReaders :many
SELECT r.id, r.company_id, r.store_id, r.code, r.name, r.kind, r.status, r.token_hint, r.last_seen_at, r.last_ip, r.agent_version, r.stats, r.created_at,
       s.name AS store_name, s.code AS store_code
FROM rfid_readers r
JOIN stores s ON s.id = r.store_id JOIN org_units o ON o.id = s.org_unit_id
WHERE r.company_id = sqlc.arg(company_id)
  AND (sqlc.arg(all_scope)::bool OR EXISTS (SELECT 1 FROM unnest(sqlc.arg(scope_paths)::text[]) sp WHERE o.path <@ sp::ltree))
  AND (sqlc.narg(store_id)::uuid IS NULL OR r.store_id = sqlc.narg(store_id)::uuid)
ORDER BY s.name, r.code;

-- name: UpdateReader :one
UPDATE rfid_readers SET name = COALESCE(sqlc.narg(name), name), status = COALESCE(sqlc.narg(status), status), updated_at = now()
WHERE id = sqlc.arg(id) AND company_id = sqlc.arg(company_id) RETURNING *;

-- name: RotateReaderToken :exec
UPDATE rfid_readers SET token_hash = $2, token_hint = $3, updated_at = now() WHERE id = $1;

-- name: TouchReader :exec
UPDATE rfid_readers SET last_seen_at = now(), last_ip = sqlc.arg(ip), stats = COALESCE(sqlc.narg(stats)::jsonb, stats),
    agent_version = CASE WHEN sqlc.arg(version)::text = '' THEN agent_version ELSE sqlc.arg(version)::text END
WHERE id = sqlc.arg(id);

-- name: StaleGateReaders :many
-- Active gate readers that enrolled but have not reported for a while (or never).
SELECT r.id, r.company_id, r.store_id, r.code, r.name, r.last_seen_at, s.name AS store_name
FROM rfid_readers r JOIN stores s ON s.id = r.store_id AND s.status = 'active'
WHERE r.status = 'active' AND r.kind = 'gate' AND r.created_at < sqlc.arg(cutoff)::timestamptz
  AND (r.last_seen_at IS NULL OR r.last_seen_at < sqlc.arg(cutoff)::timestamptz);

-- ===== tags =====

-- name: InsertTag :one
INSERT INTO rfid_tags (id, company_id, epc, variant_id, status, store_id, location, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (company_id, epc) DO NOTHING
RETURNING id;

-- name: GetTagByEPC :one
SELECT t.*, v.sku, p.name AS product_name, v.name AS variant_name, COALESCE(s.name, '') AS store_name, COALESCE(o.path::text, '')::text AS path
FROM rfid_tags t
JOIN product_variants v ON v.id = t.variant_id JOIN products p ON p.id = v.product_id
LEFT JOIN stores s ON s.id = t.store_id LEFT JOIN org_units o ON o.id = s.org_unit_id
WHERE t.company_id = $1 AND t.epc = $2;

-- name: LockTagsByEPC :many
-- Locks the rows in a stable order (no deadlocks between concurrent sales/bulk changes).
SELECT t.id, t.epc, t.variant_id, t.status, t.store_id, t.sold_sale_id FROM rfid_tags t
WHERE t.company_id = sqlc.arg(company_id) AND t.epc = ANY(sqlc.arg(epcs)::text[])
ORDER BY t.epc FOR UPDATE;

-- name: ListTags :many
SELECT t.id, t.epc, t.variant_id, t.status, t.store_id, t.location, t.sold_sale_id, t.sold_at, t.last_seen_at, t.created_at, t.updated_at,
       v.sku, p.name AS product_name, v.name AS variant_name, COALESCE(s.name, '') AS store_name
FROM rfid_tags t
JOIN product_variants v ON v.id = t.variant_id JOIN products p ON p.id = v.product_id
LEFT JOIN stores s ON s.id = t.store_id LEFT JOIN org_units o ON o.id = s.org_unit_id
WHERE t.company_id = sqlc.arg(company_id)
  AND (sqlc.arg(all_scope)::bool OR (t.store_id IS NOT NULL AND EXISTS (SELECT 1 FROM unnest(sqlc.arg(scope_paths)::text[]) sp WHERE o.path <@ sp::ltree)))
  AND (sqlc.narg(store_id)::uuid IS NULL OR t.store_id = sqlc.narg(store_id)::uuid)
  AND (sqlc.arg(under_path)::text = '' OR o.path <@ sqlc.arg(under_path)::text::ltree)
  AND (sqlc.narg(variant_id)::uuid IS NULL OR t.variant_id = sqlc.narg(variant_id)::uuid)
  AND (sqlc.arg(status)::text = '' OR t.status = sqlc.arg(status)::text)
  AND (sqlc.arg(q)::text = '' OR t.epc LIKE upper(sqlc.arg(q)::text) || '%' OR v.sku ILIKE '%' || sqlc.arg(q)::text || '%' OR p.name ILIKE '%' || sqlc.arg(q)::text || '%')
ORDER BY t.updated_at DESC, t.epc
LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);

-- name: CountTags :one
SELECT count(*) FROM rfid_tags t
JOIN product_variants v ON v.id = t.variant_id JOIN products p ON p.id = v.product_id
LEFT JOIN stores s ON s.id = t.store_id LEFT JOIN org_units o ON o.id = s.org_unit_id
WHERE t.company_id = sqlc.arg(company_id)
  AND (sqlc.arg(all_scope)::bool OR (t.store_id IS NOT NULL AND EXISTS (SELECT 1 FROM unnest(sqlc.arg(scope_paths)::text[]) sp WHERE o.path <@ sp::ltree)))
  AND (sqlc.narg(store_id)::uuid IS NULL OR t.store_id = sqlc.narg(store_id)::uuid)
  AND (sqlc.arg(under_path)::text = '' OR o.path <@ sqlc.arg(under_path)::text::ltree)
  AND (sqlc.narg(variant_id)::uuid IS NULL OR t.variant_id = sqlc.narg(variant_id)::uuid)
  AND (sqlc.arg(status)::text = '' OR t.status = sqlc.arg(status)::text)
  AND (sqlc.arg(q)::text = '' OR t.epc LIKE upper(sqlc.arg(q)::text) || '%' OR v.sku ILIKE '%' || sqlc.arg(q)::text || '%' OR p.name ILIKE '%' || sqlc.arg(q)::text || '%');

-- name: SetTagsStatus :execrows
UPDATE rfid_tags SET status = sqlc.arg(status), updated_at = now(),
    sold_sale_id = CASE WHEN sqlc.arg(status)::text = 'SOLD' THEN sold_sale_id ELSE NULL END,
    sold_at = CASE WHEN sqlc.arg(status)::text = 'SOLD' THEN sold_at ELSE NULL END
WHERE company_id = sqlc.arg(company_id) AND epc = ANY(sqlc.arg(epcs)::text[]);

-- name: MoveTags :execrows
UPDATE rfid_tags SET store_id = sqlc.narg(store_id), location = sqlc.arg(location), updated_at = now()
WHERE company_id = sqlc.arg(company_id) AND epc = ANY(sqlc.arg(epcs)::text[]);

-- name: SellTags :execrows
-- Atomic sale of tags: only tags that are sellable in THIS store flip to SOLD; the caller compares the row count.
UPDATE rfid_tags SET status = 'SOLD', sold_sale_id = sqlc.arg(sale_id), sold_at = now(), updated_at = now()
WHERE company_id = sqlc.arg(company_id) AND epc = ANY(sqlc.arg(epcs)::text[]) AND store_id = sqlc.arg(store_id)
  AND status IN ('IN_STOCK','ON_SHELF','RETURNED');

-- name: RestoreTagsOfSale :execrows
-- Void: tags that left in this sale go back on the shelf.
UPDATE rfid_tags SET status = 'ON_SHELF', sold_sale_id = NULL, sold_at = NULL, updated_at = now()
WHERE company_id = sqlc.arg(company_id) AND sold_sale_id = sqlc.arg(sale_id) AND status = 'SOLD';

-- name: InsertSaleItemTag :exec
INSERT INTO sale_item_tags (sale_item_id, epc) VALUES ($1, $2);

-- name: ListUnreturnedSaleItemTags :many
SELECT epc FROM sale_item_tags WHERE sale_item_id = $1 AND returned_at IS NULL ORDER BY epc LIMIT sqlc.arg(row_limit);

-- name: MarkSaleItemTagsReturned :exec
UPDATE sale_item_tags SET returned_at = now() WHERE sale_item_id = $1 AND epc = ANY(sqlc.arg(epcs)::text[]);

-- name: ReturnTags :execrows
UPDATE rfid_tags SET status = 'RETURNED', sold_sale_id = NULL, sold_at = NULL, updated_at = now()
WHERE company_id = sqlc.arg(company_id) AND epc = ANY(sqlc.arg(epcs)::text[]) AND status = 'SOLD';

-- name: DamageTags :execrows
UPDATE rfid_tags SET status = 'DAMAGED', sold_sale_id = NULL, sold_at = NULL, updated_at = now()
WHERE company_id = sqlc.arg(company_id) AND epc = ANY(sqlc.arg(epcs)::text[]) AND status = 'SOLD';

-- name: TouchTagSeen :exec
UPDATE rfid_tags SET last_seen_at = $3, last_reader_id = $4 WHERE company_id = $1 AND epc = $2 AND (last_seen_at IS NULL OR last_seen_at < $3);

-- name: TagStatusCounts :many
SELECT t.status, count(*)::bigint AS n
FROM rfid_tags t LEFT JOIN stores s ON s.id = t.store_id LEFT JOIN org_units o ON o.id = s.org_unit_id
WHERE t.company_id = sqlc.arg(company_id)
  AND (sqlc.arg(all_scope)::bool OR (t.store_id IS NOT NULL AND EXISTS (SELECT 1 FROM unnest(sqlc.arg(scope_paths)::text[]) sp WHERE o.path <@ sp::ltree)))
  AND (sqlc.arg(under_path)::text = '' OR o.path <@ sqlc.arg(under_path)::text::ltree)
  AND (sqlc.narg(store_id)::uuid IS NULL OR t.store_id = sqlc.narg(store_id)::uuid)
GROUP BY t.status;

-- name: TagHistory :many
SELECT e.id, e.read_at, e.direction, e.decision, e.reason, e.local_decision, r.code AS reader_code, r.kind AS reader_kind, s.name AS store_name
FROM rfid_events e JOIN rfid_readers r ON r.id = e.reader_id JOIN stores s ON s.id = e.store_id
WHERE e.company_id = $1 AND e.epc = $2 ORDER BY e.read_at DESC LIMIT 50;

-- name: RFIDStockVariance :many
-- Tags physically recorded in a store per variant vs the system balance: the day-to-day RFID/stock reconciliation.
SELECT v.id AS variant_id, v.sku, p.name AS product_name, v.name AS variant_name, s.id AS store_id, s.name AS store_name,
       COALESCE(tg.n, 0)::bigint AS tag_qty, COALESCE(bal.on_hand, 0)::numeric AS system_qty
FROM (
    SELECT t.variant_id, t.store_id, count(*) AS n FROM rfid_tags t
    WHERE t.company_id = sqlc.arg(company_id) AND t.store_id IS NOT NULL AND t.status IN ('IN_STOCK','ON_SHELF','RESERVED','RETURNED')
    GROUP BY t.variant_id, t.store_id
) tg
JOIN product_variants v ON v.id = tg.variant_id JOIN products p ON p.id = v.product_id
JOIN stores s ON s.id = tg.store_id JOIN org_units o ON o.id = s.org_unit_id
LEFT JOIN LATERAL (SELECT sum(b.on_hand) AS on_hand FROM inventory_balances b WHERE b.store_id = tg.store_id AND b.variant_id = tg.variant_id) bal ON true
WHERE (sqlc.arg(all_scope)::bool OR EXISTS (SELECT 1 FROM unnest(sqlc.arg(scope_paths)::text[]) sp WHERE o.path <@ sp::ltree))
  AND (sqlc.arg(under_path)::text = '' OR o.path <@ sqlc.arg(under_path)::text::ltree)
  AND (sqlc.narg(store_id)::uuid IS NULL OR tg.store_id = sqlc.narg(store_id)::uuid)
  AND (NOT sqlc.arg(only_diff)::bool OR tg.n <> COALESCE(bal.on_hand, 0))
ORDER BY abs(tg.n - COALESCE(bal.on_hand, 0)) DESC, p.name
LIMIT sqlc.arg(row_limit);

-- ===== edge delta sync =====

-- name: TagDelta :many
SELECT t.epc, t.variant_id, t.status, t.store_id, t.sold_at, t.updated_at
FROM rfid_tags t
WHERE t.company_id = sqlc.arg(company_id) AND t.store_id = sqlc.arg(store_id)
  AND (t.updated_at > sqlc.arg(since)::timestamptz OR (t.updated_at = sqlc.arg(since)::timestamptz AND t.epc > sqlc.arg(after_epc)::text))
ORDER BY t.updated_at, t.epc
LIMIT sqlc.arg(row_limit);

-- name: GateLookupTags :many
-- Everything the gate decision needs for a batch of EPCs (tags of any store: a foreign tag is a signal too).
SELECT t.epc, t.variant_id, t.status, t.store_id, t.sold_at FROM rfid_tags t
WHERE t.company_id = sqlc.arg(company_id) AND t.epc = ANY(sqlc.arg(epcs)::text[]);

-- ===== events =====

-- name: InsertRFIDEvent :one
INSERT INTO rfid_events (id, company_id, store_id, reader_id, client_event_id, epc, direction, rssi, read_at, local_decision, decision, reason, security_event_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
ON CONFLICT (reader_id, client_event_id, read_at) DO NOTHING
RETURNING id;

-- ===== RFID stock count =====

-- name: AddCountTags :execrows
-- Only tags that belong to the counted store are accepted (the count's store, not the scanner's guess).
INSERT INTO stock_count_tags (count_id, epc, variant_id)
SELECT sqlc.arg(count_id)::uuid, t.epc, t.variant_id FROM rfid_tags t
WHERE t.company_id = sqlc.arg(company_id) AND t.epc = ANY(sqlc.arg(epcs)::text[]) AND t.store_id = sqlc.arg(store_id)
  AND t.status IN ('IN_STOCK','ON_SHELF','RESERVED','RETURNED')
ON CONFLICT DO NOTHING;

-- name: CountTagTotals :many
SELECT variant_id, count(*)::bigint AS n FROM stock_count_tags WHERE count_id = $1 AND variant_id = ANY(sqlc.arg(variants)::uuid[]) GROUP BY variant_id;
