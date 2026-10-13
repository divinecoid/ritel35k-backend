-- +goose Up
CREATE EXTENSION IF NOT EXISTS ltree;
CREATE EXTENSION IF NOT EXISTS citext;

CREATE TABLE companies (
    id          uuid PRIMARY KEY,
    code        text NOT NULL UNIQUE,
    name        text NOT NULL,
    status      text NOT NULL DEFAULT 'active' CHECK (status IN ('active','inactive')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- Generic org tree: company -> business_unit -> region -> store. Levels are optional.
-- `path` is an ltree built from sanitized codes; codes are immutable and unique per company.
CREATE TABLE org_units (
    id          uuid PRIMARY KEY,
    company_id  uuid NOT NULL REFERENCES companies(id),
    parent_id   uuid REFERENCES org_units(id),
    type        text NOT NULL CHECK (type IN ('company','business_unit','region','store')),
    code        text NOT NULL,
    name        text NOT NULL,
    path        ltree NOT NULL,
    status      text NOT NULL DEFAULT 'active' CHECK (status IN ('active','inactive')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (company_id, code),
    UNIQUE (company_id, path)
);
CREATE INDEX org_units_path_gist ON org_units USING gist (path);
CREATE INDEX org_units_parent_idx ON org_units (parent_id);

CREATE TABLE stores (
    id               uuid PRIMARY KEY,
    company_id       uuid NOT NULL REFERENCES companies(id),
    org_unit_id      uuid NOT NULL UNIQUE REFERENCES org_units(id),
    code             text NOT NULL,
    name             text NOT NULL,
    address          jsonb NOT NULL DEFAULT '{}'::jsonb,
    timezone         text NOT NULL DEFAULT 'Asia/Jakarta',
    operating_hours  jsonb NOT NULL DEFAULT '{}'::jsonb,
    status           text NOT NULL DEFAULT 'active' CHECK (status IN ('active','inactive','closed')),
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    created_by       uuid,
    UNIQUE (company_id, code)
);
CREATE INDEX stores_company_idx ON stores (company_id);

-- store_id NULL = central warehouse (not tied to one store).
CREATE TABLE warehouses (
    id          uuid PRIMARY KEY,
    company_id  uuid NOT NULL REFERENCES companies(id),
    store_id    uuid REFERENCES stores(id),
    code        text NOT NULL,
    name        text NOT NULL,
    type        text NOT NULL DEFAULT 'store' CHECK (type IN ('store','main','transit','damaged','return')),
    is_default  boolean NOT NULL DEFAULT false,
    status      text NOT NULL DEFAULT 'active' CHECK (status IN ('active','inactive')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    created_by  uuid,
    UNIQUE (company_id, code)
);
CREATE INDEX warehouses_store_idx ON warehouses (store_id);
-- at most one default warehouse per store
CREATE UNIQUE INDEX warehouses_one_default_per_store ON warehouses (store_id) WHERE is_default AND store_id IS NOT NULL;

-- Config inheritance: company root org_unit -> region -> store. Most specific wins.
CREATE TABLE settings (
    id           uuid PRIMARY KEY,
    company_id   uuid NOT NULL REFERENCES companies(id),
    org_unit_id  uuid NOT NULL REFERENCES org_units(id),
    key          text NOT NULL,
    value        jsonb NOT NULL,
    version      integer NOT NULL DEFAULT 1,
    updated_by   uuid,
    updated_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (org_unit_id, key)
);
CREATE INDEX settings_company_key_idx ON settings (company_id, key);

-- +goose Down
DROP TABLE settings;
DROP TABLE warehouses;
DROP TABLE stores;
DROP TABLE org_units;
DROP TABLE companies;
