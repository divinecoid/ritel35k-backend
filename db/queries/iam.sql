-- name: CreateUser :one
INSERT INTO users (id, company_id, email, name, password_hash, pin_hash, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetUser :one
SELECT * FROM users WHERE id = $1;

-- name: GetUserByEmail :one
SELECT * FROM users WHERE email = $1;

-- name: ListUsers :many
SELECT u.* FROM users u
WHERE u.company_id = sqlc.arg(company_id)
  AND (sqlc.arg(q)::text = '' OR u.name ILIKE '%' || sqlc.arg(q)::text || '%' OR u.email ILIKE '%' || sqlc.arg(q)::text || '%')
  AND (sqlc.arg(all_scope)::bool OR EXISTS (
        SELECT 1 FROM user_role_scopes urs JOIN org_units ou ON ou.id = urs.org_unit_id
        WHERE urs.user_id = u.id AND EXISTS (SELECT 1 FROM unnest(sqlc.arg(scope_paths)::text[]) p WHERE ou.path <@ p::ltree)))
ORDER BY u.name
LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);

-- name: CountUsers :one
SELECT count(*) FROM users u
WHERE u.company_id = sqlc.arg(company_id)
  AND (sqlc.arg(q)::text = '' OR u.name ILIKE '%' || sqlc.arg(q)::text || '%' OR u.email ILIKE '%' || sqlc.arg(q)::text || '%')
  AND (sqlc.arg(all_scope)::bool OR EXISTS (
        SELECT 1 FROM user_role_scopes urs JOIN org_units ou ON ou.id = urs.org_unit_id
        WHERE urs.user_id = u.id AND EXISTS (SELECT 1 FROM unnest(sqlc.arg(scope_paths)::text[]) p WHERE ou.path <@ p::ltree)));

-- name: ListUserScopePaths :many
-- Scope paths of a user's role assignments; '' = company-wide.
SELECT COALESCE(ou.path::text, '')::text AS scope_path
FROM user_role_scopes urs LEFT JOIN org_units ou ON ou.id = urs.org_unit_id
WHERE urs.user_id = $1;

-- name: ListCompanyIDs :many
SELECT id FROM companies;

-- name: GrantAllPermissionsToRole :exec
INSERT INTO role_permissions (role_id, permission_code)
SELECT $1, code FROM permissions ON CONFLICT DO NOTHING;

-- name: UpdateUser :one
UPDATE users SET
    name = COALESCE(sqlc.narg(name), name),
    status = COALESCE(sqlc.narg(status), status),
    updated_at = now()
WHERE id = sqlc.arg(id) AND company_id = sqlc.arg(company_id)
RETURNING *;

-- name: UpdateUserPassword :exec
UPDATE users SET password_hash = $2, updated_at = now() WHERE id = $1;

-- name: UpdateUserPIN :exec
UPDATE users SET pin_hash = $2, updated_at = now() WHERE id = $1;

-- name: TouchUserLogin :exec
UPDATE users SET last_login_at = now() WHERE id = $1;

-- name: UpsertPermission :exec
INSERT INTO permissions (code, description) VALUES ($1, $2)
ON CONFLICT (code) DO UPDATE SET description = EXCLUDED.description;

-- name: ListPermissions :many
SELECT * FROM permissions ORDER BY code;

-- name: CreateRole :one
INSERT INTO roles (id, company_id, code, name, is_system) VALUES ($1, $2, $3, $4, $5) RETURNING *;

-- name: GetRole :one
SELECT * FROM roles WHERE id = $1 AND company_id = $2;

-- name: GetRoleByCode :one
SELECT * FROM roles WHERE company_id = $1 AND code = $2;

-- name: ListRoles :many
SELECT * FROM roles WHERE company_id = $1 ORDER BY name;

-- name: UpdateRole :one
UPDATE roles SET name = $3, updated_at = now() WHERE id = $1 AND company_id = $2 RETURNING *;

-- name: ListRolePermissions :many
SELECT role_id, permission_code FROM role_permissions WHERE role_id = ANY(sqlc.arg(role_ids)::uuid[]) ORDER BY permission_code;

-- name: AddRolePermission :exec
INSERT INTO role_permissions (role_id, permission_code) VALUES ($1, $2) ON CONFLICT DO NOTHING;

-- name: ClearRolePermissions :exec
DELETE FROM role_permissions WHERE role_id = $1;

-- name: AddUserRoleScope :exec
INSERT INTO user_role_scopes (id, user_id, role_id, org_unit_id, created_by)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT DO NOTHING;

-- name: ClearUserRoleScopes :exec
DELETE FROM user_role_scopes WHERE user_id = $1;

-- name: ListUserRoleScopes :many
SELECT urs.id, urs.user_id, urs.role_id, r.code AS role_code, r.name AS role_name,
       urs.org_unit_id, ou.name AS org_unit_name, ou.type AS org_unit_type
FROM user_role_scopes urs
JOIN roles r ON r.id = urs.role_id
LEFT JOIN org_units ou ON ou.id = urs.org_unit_id
WHERE urs.user_id = ANY(sqlc.arg(user_ids)::uuid[])
ORDER BY r.name, ou.path;

-- name: ListUserGrants :many
-- scope_path '' = whole company.
SELECT rp.permission_code, COALESCE(ou.path::text, '')::text AS scope_path
FROM user_role_scopes urs
JOIN role_permissions rp ON rp.role_id = urs.role_id
LEFT JOIN org_units ou ON ou.id = urs.org_unit_id
WHERE urs.user_id = $1;

-- name: CreateRefreshToken :exec
INSERT INTO refresh_tokens (id, user_id, family_id, token_hash, expires_at, user_agent, ip)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: GetRefreshTokenForUpdate :one
SELECT * FROM refresh_tokens WHERE token_hash = $1 FOR UPDATE;

-- name: RotateRefreshToken :exec
UPDATE refresh_tokens SET revoked_at = now(), replaced_by = $2 WHERE id = $1;

-- name: RevokeRefreshFamily :exec
UPDATE refresh_tokens SET revoked_at = now() WHERE family_id = $1 AND revoked_at IS NULL;

-- name: RevokeUserRefreshTokens :exec
UPDATE refresh_tokens SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL;
