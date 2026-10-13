-- name: InsertSecurityEvent :one
INSERT INTO security_events (id, company_id, store_id, reader_id, type, severity, title, epc, variant_id, sale_id, user_id, detail, dedupe_key, occurred_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
ON CONFLICT (company_id, dedupe_key) WHERE dedupe_key <> '' DO NOTHING
RETURNING id;

-- name: GetSecurityEvent :one
SELECT e.*, s.name AS store_name, o.path::text AS path, COALESCE(r.code, '') AS reader_code, COALESCE(u.name, '') AS user_name,
       COALESCE(v.sku, '') AS sku, COALESCE(p.name, '') AS product_name,
       COALESCE(ab.name, '') AS acknowledged_by_name, COALESCE(rb.name, '') AS resolved_by_name
FROM security_events e
JOIN stores s ON s.id = e.store_id JOIN org_units o ON o.id = s.org_unit_id
LEFT JOIN rfid_readers r ON r.id = e.reader_id
LEFT JOIN users u ON u.id = e.user_id
LEFT JOIN product_variants v ON v.id = e.variant_id LEFT JOIN products p ON p.id = v.product_id
LEFT JOIN users ab ON ab.id = e.acknowledged_by LEFT JOIN users rb ON rb.id = e.resolved_by
WHERE e.id = $1 AND e.company_id = $2;

-- name: ListSecurityEvents :many
SELECT e.id, e.store_id, e.type, e.severity, e.title, e.epc, e.sale_id, e.status, e.occurred_at, e.resolved_at,
       s.name AS store_name, COALESCE(r.code, '') AS reader_code, COALESCE(u.name, '') AS user_name
FROM security_events e
JOIN stores s ON s.id = e.store_id JOIN org_units o ON o.id = s.org_unit_id
LEFT JOIN rfid_readers r ON r.id = e.reader_id LEFT JOIN users u ON u.id = e.user_id
WHERE e.company_id = sqlc.arg(company_id)
  AND (sqlc.arg(all_scope)::bool OR EXISTS (SELECT 1 FROM unnest(sqlc.arg(scope_paths)::text[]) sp WHERE o.path <@ sp::ltree))
  AND (sqlc.narg(store_id)::uuid IS NULL OR e.store_id = sqlc.narg(store_id)::uuid)
  AND (sqlc.arg(under_path)::text = '' OR o.path <@ sqlc.arg(under_path)::text::ltree)
  AND (sqlc.arg(status)::text = '' OR (sqlc.arg(status)::text = 'active' AND e.status IN ('open','acknowledged')) OR e.status = sqlc.arg(status)::text)
  AND (sqlc.arg(type)::text = '' OR e.type = sqlc.arg(type)::text)
  AND (sqlc.arg(severity)::text = '' OR e.severity = sqlc.arg(severity)::text)
  AND e.occurred_at >= sqlc.arg(from_time)::timestamptz AND e.occurred_at < sqlc.arg(to_time)::timestamptz
ORDER BY e.occurred_at DESC
LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);

-- name: CountSecurityEvents :one
SELECT count(*) FROM security_events e
JOIN stores s ON s.id = e.store_id JOIN org_units o ON o.id = s.org_unit_id
WHERE e.company_id = sqlc.arg(company_id)
  AND (sqlc.arg(all_scope)::bool OR EXISTS (SELECT 1 FROM unnest(sqlc.arg(scope_paths)::text[]) sp WHERE o.path <@ sp::ltree))
  AND (sqlc.narg(store_id)::uuid IS NULL OR e.store_id = sqlc.narg(store_id)::uuid)
  AND (sqlc.arg(under_path)::text = '' OR o.path <@ sqlc.arg(under_path)::text::ltree)
  AND (sqlc.arg(status)::text = '' OR (sqlc.arg(status)::text = 'active' AND e.status IN ('open','acknowledged')) OR e.status = sqlc.arg(status)::text)
  AND (sqlc.arg(type)::text = '' OR e.type = sqlc.arg(type)::text)
  AND (sqlc.arg(severity)::text = '' OR e.severity = sqlc.arg(severity)::text)
  AND e.occurred_at >= sqlc.arg(from_time)::timestamptz AND e.occurred_at < sqlc.arg(to_time)::timestamptz;

-- name: SecuritySummary :many
-- Counts by type/severity/status over the window, for the loss-prevention overview.
SELECT e.type, e.severity, e.status, count(*)::bigint AS n
FROM security_events e
JOIN stores s ON s.id = e.store_id JOIN org_units o ON o.id = s.org_unit_id
WHERE e.company_id = sqlc.arg(company_id)
  AND (sqlc.arg(all_scope)::bool OR EXISTS (SELECT 1 FROM unnest(sqlc.arg(scope_paths)::text[]) sp WHERE o.path <@ sp::ltree))
  AND (sqlc.narg(store_id)::uuid IS NULL OR e.store_id = sqlc.narg(store_id)::uuid)
  AND (sqlc.arg(under_path)::text = '' OR o.path <@ sqlc.arg(under_path)::text::ltree)
  AND e.occurred_at >= sqlc.arg(from_time)::timestamptz AND e.occurred_at < sqlc.arg(to_time)::timestamptz
GROUP BY e.type, e.severity, e.status;

-- name: SecurityByDay :many
SELECT (e.occurred_at AT TIME ZONE st.timezone)::date AS day, count(*)::bigint AS n, count(*) FILTER (WHERE e.severity = 'critical')::bigint AS critical
FROM security_events e
JOIN stores st ON st.id = e.store_id JOIN org_units o ON o.id = st.org_unit_id
WHERE e.company_id = sqlc.arg(company_id)
  AND (sqlc.arg(all_scope)::bool OR EXISTS (SELECT 1 FROM unnest(sqlc.arg(scope_paths)::text[]) sp WHERE o.path <@ sp::ltree))
  AND (sqlc.narg(store_id)::uuid IS NULL OR e.store_id = sqlc.narg(store_id)::uuid)
  AND (sqlc.arg(under_path)::text = '' OR o.path <@ sqlc.arg(under_path)::text::ltree)
  AND e.occurred_at >= sqlc.arg(from_time)::timestamptz AND e.occurred_at < sqlc.arg(to_time)::timestamptz
GROUP BY 1 ORDER BY 1;

-- name: SecurityByStore :many
SELECT st.id AS store_id, st.name AS store_name, count(*)::bigint AS n, count(*) FILTER (WHERE e.status IN ('open','acknowledged'))::bigint AS active,
       count(*) FILTER (WHERE e.severity = 'critical')::bigint AS critical
FROM security_events e
JOIN stores st ON st.id = e.store_id JOIN org_units o ON o.id = st.org_unit_id
WHERE e.company_id = sqlc.arg(company_id)
  AND (sqlc.arg(all_scope)::bool OR EXISTS (SELECT 1 FROM unnest(sqlc.arg(scope_paths)::text[]) sp WHERE o.path <@ sp::ltree))
  AND (sqlc.narg(store_id)::uuid IS NULL OR e.store_id = sqlc.narg(store_id)::uuid)
  AND (sqlc.arg(under_path)::text = '' OR o.path <@ sqlc.arg(under_path)::text::ltree)
  AND e.occurred_at >= sqlc.arg(from_time)::timestamptz AND e.occurred_at < sqlc.arg(to_time)::timestamptz
GROUP BY st.id, st.name ORDER BY n DESC LIMIT 10;

-- name: UpdateSecurityEventStatus :one
UPDATE security_events SET status = sqlc.arg(status),
    acknowledged_by = CASE WHEN sqlc.arg(status)::text = 'acknowledged' THEN sqlc.arg(by)::uuid ELSE acknowledged_by END,
    acknowledged_at = CASE WHEN sqlc.arg(status)::text = 'acknowledged' THEN now() ELSE acknowledged_at END,
    resolved_by = CASE WHEN sqlc.arg(status)::text IN ('resolved','false_alarm') THEN sqlc.arg(by)::uuid ELSE resolved_by END,
    resolved_at = CASE WHEN sqlc.arg(status)::text IN ('resolved','false_alarm') THEN now() ELSE resolved_at END,
    resolution_note = CASE WHEN sqlc.arg(note)::text <> '' THEN sqlc.arg(note)::text ELSE resolution_note END
WHERE id = sqlc.arg(id) AND company_id = sqlc.arg(company_id) AND status IN ('open','acknowledged')
RETURNING id;

-- name: SaleRFIDLines :many
SELECT si.id AS sale_item_id, si.variant_id, array_agg(t.epc ORDER BY t.epc)::text[] AS epcs
FROM sale_item_tags t JOIN sale_items si ON si.id = t.sale_item_id WHERE si.sale_id = $1 GROUP BY si.id, si.variant_id;

-- name: SaleDiscountFacts :one
SELECT store_id, cashier_id, document_number, subtotal, discount_total, promo_discount, voucher_discount, grand_total, status FROM sales WHERE id = $1;
