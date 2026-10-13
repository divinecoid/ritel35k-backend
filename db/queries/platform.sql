-- name: InsertOutbox :exec
INSERT INTO outbox (company_id, event_type, aggregate_type, aggregate_id, payload)
VALUES ($1, $2, $3, $4, $5);

-- name: ClaimOutbox :many
SELECT * FROM outbox WHERE published_at IS NULL AND attempts < sqlc.arg(max_attempts)
ORDER BY id LIMIT sqlc.arg(batch) FOR UPDATE SKIP LOCKED;

-- name: MarkOutboxPublished :exec
UPDATE outbox SET published_at = now() WHERE id = $1;

-- name: MarkOutboxFailed :exec
UPDATE outbox SET attempts = attempts + 1, last_error = $2 WHERE id = $1;

-- name: MarkEventProcessed :execrows
INSERT INTO processed_events (consumer, event_id) VALUES ($1, $2) ON CONFLICT DO NOTHING;

-- name: UnmarkEventProcessed :exec
DELETE FROM processed_events WHERE consumer = $1 AND event_id = $2;

-- name: NextDocumentNo :one
INSERT INTO document_sequences (company_id, store_id, doc_type, period, last_no)
VALUES ($1, $2, $3, $4, 1)
ON CONFLICT (store_id, doc_type, period) DO UPDATE SET last_no = document_sequences.last_no + 1
RETURNING last_no;

-- name: InsertAuditLog :exec
INSERT INTO audit_logs (id, company_id, store_id, user_id, action, entity, entity_id, old_value, new_value, ip_address, user_agent, device_id, request_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13);

-- name: ListAuditLogs :many
SELECT a.* FROM audit_logs a
LEFT JOIN stores s ON s.id = a.store_id
LEFT JOIN org_units o ON o.id = s.org_unit_id
WHERE a.company_id = sqlc.arg(company_id)
  AND (
    sqlc.arg(all_scope)::bool
    OR (a.store_id IS NOT NULL AND EXISTS (
          SELECT 1 FROM unnest(sqlc.arg(scope_paths)::text[]) p WHERE o.path <@ p::ltree))
  )
  AND (sqlc.arg(action)::text = '' OR a.action = sqlc.arg(action)::text)
  AND (sqlc.arg(entity)::text = '' OR a.entity = sqlc.arg(entity)::text)
  AND (sqlc.narg(user_id)::uuid IS NULL OR a.user_id = sqlc.narg(user_id)::uuid)
  AND (sqlc.narg(store_id)::uuid IS NULL OR a.store_id = sqlc.narg(store_id)::uuid)
  AND (sqlc.narg(from_time)::timestamptz IS NULL OR a.created_at >= sqlc.narg(from_time)::timestamptz)
  AND (sqlc.narg(to_time)::timestamptz IS NULL OR a.created_at < sqlc.narg(to_time)::timestamptz)
ORDER BY a.created_at DESC
LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);
