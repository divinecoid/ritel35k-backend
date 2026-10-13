-- name: CreateCompany :one
INSERT INTO companies (id, code, name) VALUES ($1, $2, $3) RETURNING *;

-- name: GetCompany :one
SELECT * FROM companies WHERE id = $1;

-- name: GetCompanyByCode :one
SELECT * FROM companies WHERE code = $1;

-- name: CreateOrgUnit :one
INSERT INTO org_units (id, company_id, parent_id, type, code, name, path)
VALUES ($1, $2, $3, $4, $5, $6, sqlc.arg(path)::ltree)
RETURNING id, company_id, parent_id, type, code, name, path::text AS path, status, created_at, updated_at;

-- name: GetOrgUnit :one
SELECT id, company_id, parent_id, type, code, name, path::text AS path, status, created_at, updated_at
FROM org_units WHERE id = $1 AND company_id = $2;

-- name: GetCompanyRootOrgUnit :one
SELECT id, company_id, parent_id, type, code, name, path::text AS path, status, created_at, updated_at
FROM org_units WHERE company_id = $1 AND type = 'company';

-- name: ListOrgUnitsVisible :many
-- Units inside the caller's scope plus their ancestors (so the tree can be rendered).
SELECT o.id, o.company_id, o.parent_id, o.type, o.code, o.name, o.path::text AS path, o.status,
       (sqlc.arg(all_scope)::bool OR EXISTS (
           SELECT 1 FROM unnest(sqlc.arg(scope_paths)::text[]) p WHERE o.path <@ p::ltree
       ))::bool AS accessible,
       st.id AS store_id, st.status AS store_status
FROM org_units o
LEFT JOIN stores st ON st.org_unit_id = o.id
WHERE o.company_id = sqlc.arg(company_id)
  AND (sqlc.arg(all_scope)::bool OR EXISTS (
        SELECT 1 FROM unnest(sqlc.arg(scope_paths)::text[]) p
        WHERE o.path <@ p::ltree OR p::ltree <@ o.path
  ))
ORDER BY o.path;

-- name: CreateStore :one
INSERT INTO stores (id, company_id, org_unit_id, code, name, address, timezone, operating_hours, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: GetStore :one
SELECT s.*, o.path::text AS path, o.parent_id AS parent_org_unit_id
FROM stores s JOIN org_units o ON o.id = s.org_unit_id
WHERE s.id = $1 AND s.company_id = $2;

-- name: ListStores :many
SELECT s.*, o.path::text AS path, o.parent_id AS parent_org_unit_id
FROM stores s JOIN org_units o ON o.id = s.org_unit_id
WHERE s.company_id = sqlc.arg(company_id)
  AND (sqlc.arg(all_scope)::bool OR EXISTS (
        SELECT 1 FROM unnest(sqlc.arg(scope_paths)::text[]) p WHERE o.path <@ p::ltree))
  AND (sqlc.arg(status)::text = '' OR s.status = sqlc.arg(status)::text)
  AND (sqlc.arg(under_path)::text = '' OR o.path <@ sqlc.arg(under_path)::text::ltree)
ORDER BY s.name;

-- name: UpdateStore :one
UPDATE stores SET
    name = COALESCE(sqlc.narg(name), name),
    address = COALESCE(sqlc.narg(address), address),
    timezone = COALESCE(sqlc.narg(timezone), timezone),
    operating_hours = COALESCE(sqlc.narg(operating_hours), operating_hours),
    status = COALESCE(sqlc.narg(status), status),
    updated_at = now()
WHERE id = sqlc.arg(id) AND company_id = sqlc.arg(company_id)
RETURNING *;

-- name: CreateWarehouse :one
INSERT INTO warehouses (id, company_id, store_id, code, name, type, is_default, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: ListWarehouses :many
SELECT w.*
FROM warehouses w
LEFT JOIN stores s ON s.id = w.store_id
LEFT JOIN org_units o ON o.id = s.org_unit_id
WHERE w.company_id = sqlc.arg(company_id)
  AND (
    sqlc.arg(all_scope)::bool
    OR (w.store_id IS NOT NULL AND EXISTS (
          SELECT 1 FROM unnest(sqlc.arg(scope_paths)::text[]) p WHERE o.path <@ p::ltree))
  )
  AND (sqlc.narg(store_id)::uuid IS NULL OR w.store_id = sqlc.narg(store_id)::uuid)
ORDER BY w.code;

-- name: GetWarehouse :one
SELECT * FROM warehouses WHERE id = $1 AND company_id = $2;

-- name: UpdateWarehouse :one
UPDATE warehouses SET
    name = COALESCE(sqlc.narg(name), name),
    status = COALESCE(sqlc.narg(status), status),
    updated_at = now()
WHERE id = sqlc.arg(id) AND company_id = sqlc.arg(company_id)
RETURNING *;

-- name: UpsertSetting :one
INSERT INTO settings (id, company_id, org_unit_id, key, value, updated_by)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (org_unit_id, key) DO UPDATE
    SET value = EXCLUDED.value, version = settings.version + 1, updated_by = EXCLUDED.updated_by, updated_at = now()
RETURNING *;

-- name: DeleteSetting :execrows
DELETE FROM settings WHERE org_unit_id = $1 AND key = $2 AND company_id = $3;

-- name: ResolveSetting :one
-- Most specific ancestor-or-self override wins.
SELECT s.value
FROM settings s
JOIN org_units so ON so.id = s.org_unit_id
JOIN org_units t ON t.id = sqlc.arg(org_unit_id)
WHERE s.company_id = sqlc.arg(company_id) AND s.key = sqlc.arg(key) AND t.path <@ so.path
ORDER BY nlevel(so.path) DESC
LIMIT 1;

-- name: ResolveAllSettings :many
SELECT DISTINCT ON (s.key)
    s.key, s.value, s.org_unit_id AS source_org_unit_id, so.type AS source_type, so.name AS source_name
FROM settings s
JOIN org_units so ON so.id = s.org_unit_id
JOIN org_units t ON t.id = sqlc.arg(org_unit_id)
WHERE s.company_id = sqlc.arg(company_id) AND t.path <@ so.path
ORDER BY s.key, nlevel(so.path) DESC;

-- name: ListWarehouseDirectory :many
-- Minimal company-wide list (no scope filter) so a store can pick a transfer destination it cannot otherwise see.
SELECT w.id, w.code, w.name, w.type, w.store_id, s.name AS store_name
FROM warehouses w LEFT JOIN stores s ON s.id = w.store_id
WHERE w.company_id = $1 AND w.status = 'active'
ORDER BY s.name NULLS FIRST, w.code;
