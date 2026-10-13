-- +goose Up
CREATE TABLE users (
    id             uuid PRIMARY KEY,
    company_id     uuid NOT NULL REFERENCES companies(id),
    email          citext NOT NULL UNIQUE,
    name           text NOT NULL,
    password_hash  text NOT NULL,
    pin_hash       text,
    status         text NOT NULL DEFAULT 'active' CHECK (status IN ('active','inactive','locked')),
    last_login_at  timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    created_by     uuid
);
CREATE INDEX users_company_idx ON users (company_id);

CREATE TABLE permissions (
    code         text PRIMARY KEY,
    description  text NOT NULL DEFAULT ''
);

CREATE TABLE roles (
    id          uuid PRIMARY KEY,
    company_id  uuid NOT NULL REFERENCES companies(id),
    code        text NOT NULL,
    name        text NOT NULL,
    is_system   boolean NOT NULL DEFAULT false,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (company_id, code)
);

CREATE TABLE role_permissions (
    role_id          uuid NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
    permission_code  text NOT NULL REFERENCES permissions(code) ON DELETE CASCADE,
    PRIMARY KEY (role_id, permission_code)
);

-- RBAC + organizational scope.
-- org_unit_id NULL  => whole company; otherwise the org_unit and everything below it.
CREATE TABLE user_role_scopes (
    id           uuid PRIMARY KEY,
    user_id      uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role_id      uuid NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
    org_unit_id  uuid REFERENCES org_units(id) ON DELETE CASCADE,
    created_at   timestamptz NOT NULL DEFAULT now(),
    created_by   uuid
);
CREATE UNIQUE INDEX user_role_scopes_uniq ON user_role_scopes (user_id, role_id, COALESCE(org_unit_id, '00000000-0000-0000-0000-000000000000'::uuid));
CREATE INDEX user_role_scopes_user_idx ON user_role_scopes (user_id);

CREATE TABLE refresh_tokens (
    id           uuid PRIMARY KEY,
    user_id      uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    family_id    uuid NOT NULL,
    token_hash   bytea NOT NULL UNIQUE,
    expires_at   timestamptz NOT NULL,
    revoked_at   timestamptz,
    replaced_by  uuid,
    user_agent   text NOT NULL DEFAULT '',
    ip           text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX refresh_tokens_user_idx ON refresh_tokens (user_id);
CREATE INDEX refresh_tokens_family_idx ON refresh_tokens (family_id);

-- +goose Down
DROP TABLE refresh_tokens;
DROP TABLE user_role_scopes;
DROP TABLE role_permissions;
DROP TABLE roles;
DROP TABLE permissions;
DROP TABLE users;
