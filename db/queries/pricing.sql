-- name: ClosePrices :exec
UPDATE prices SET effective_to = sqlc.arg(close_at)
WHERE org_unit_id = sqlc.arg(org_unit_id) AND variant_id = sqlc.arg(variant_id) AND company_id = sqlc.arg(company_id)
  AND price_list_id = sqlc.arg(price_list_id) AND min_qty = sqlc.arg(min_qty)
  AND effective_to IS NULL AND effective_from < sqlc.arg(close_at);

-- name: InsertPrice :one
INSERT INTO prices (id, company_id, org_unit_id, variant_id, price_list_id, price, min_qty, effective_from, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING *;

-- name: ResolvePrice :one
-- Nearest ancestor-or-self wins; within a unit the highest qualifying volume tier (min_qty <= qty) wins.
SELECT p.price, p.min_qty, p.org_unit_id AS source_org_unit_id, ou.type AS source_type, ou.name AS source_name
FROM prices p
JOIN org_units ou ON ou.id = p.org_unit_id
JOIN org_units t ON t.id = sqlc.arg(store_org_unit_id)
WHERE p.company_id = sqlc.arg(company_id) AND p.variant_id = sqlc.arg(variant_id) AND p.price_list_id = sqlc.arg(price_list_id)
  AND t.path <@ ou.path AND p.min_qty <= sqlc.arg(qty)::numeric
  AND p.effective_from <= sqlc.arg(at)::timestamptz AND (p.effective_to IS NULL OR p.effective_to > sqlc.arg(at)::timestamptz)
ORDER BY nlevel(ou.path) DESC, p.min_qty DESC, p.effective_from DESC
LIMIT 1;

-- name: ListVariantPrices :many
SELECT p.id, p.org_unit_id, ou.name AS org_unit_name, ou.type AS org_unit_type, ou.path::text AS org_unit_path,
       p.price_list_id, pl.name AS price_list_name, p.min_qty, p.price, p.effective_from, p.effective_to, p.created_at
FROM prices p JOIN org_units ou ON ou.id = p.org_unit_id JOIN price_lists pl ON pl.id = p.price_list_id
WHERE p.company_id = $1 AND p.variant_id = $2
ORDER BY pl.is_default DESC, pl.name, ou.path, p.min_qty, p.effective_from DESC;

-- name: GetDefaultPriceList :one
SELECT * FROM price_lists WHERE company_id = $1 AND is_default;

-- name: GetPriceList :one
SELECT * FROM price_lists WHERE id = $1 AND company_id = $2;

-- name: ListPriceLists :many
SELECT pl.*, ml.name AS level_name FROM price_lists pl LEFT JOIN membership_levels ml ON ml.id = pl.membership_level_id
WHERE pl.company_id = $1 ORDER BY pl.is_default DESC, pl.name;

-- name: ListApplicablePriceLists :many
-- The default list plus any list bound to the customer's membership level.
SELECT id FROM price_lists
WHERE company_id = sqlc.arg(company_id) AND status = 'active'
  AND (is_default OR (sqlc.narg(level_id)::uuid IS NOT NULL AND membership_level_id = sqlc.narg(level_id)::uuid));

-- name: CreatePriceList :one
INSERT INTO price_lists (id, company_id, code, name, membership_level_id) VALUES ($1, $2, $3, $4, $5) RETURNING *;

-- name: UpdatePriceList :one
UPDATE price_lists SET name = COALESCE(sqlc.narg(name), name), status = COALESCE(sqlc.narg(status), status)
WHERE id = sqlc.arg(id) AND company_id = sqlc.arg(company_id) RETURNING *;

-- name: CreateDefaultPriceList :one
INSERT INTO price_lists (id, company_id, code, name, is_default) VALUES ($1, $2, 'STANDARD', 'Harga Normal', true) RETURNING *;
