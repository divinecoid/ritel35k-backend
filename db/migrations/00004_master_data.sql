-- +goose Up
-- Global (company-level) master data: available in every store automatically.

CREATE TABLE categories (
    id          uuid PRIMARY KEY,
    company_id  uuid NOT NULL REFERENCES companies(id),
    parent_id   uuid REFERENCES categories(id),
    code        text NOT NULL,
    name        text NOT NULL,
    status      text NOT NULL DEFAULT 'active' CHECK (status IN ('active','inactive')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    created_by  uuid,
    UNIQUE (company_id, code)
);

CREATE TABLE brands (
    id          uuid PRIMARY KEY,
    company_id  uuid NOT NULL REFERENCES companies(id),
    code        text NOT NULL,
    name        text NOT NULL,
    status      text NOT NULL DEFAULT 'active' CHECK (status IN ('active','inactive')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    created_by  uuid,
    UNIQUE (company_id, code)
);

CREATE TABLE uoms (
    id          uuid PRIMARY KEY,
    company_id  uuid NOT NULL REFERENCES companies(id),
    code        text NOT NULL,
    name        text NOT NULL,
    allows_decimal boolean NOT NULL DEFAULT false,
    created_at  timestamptz NOT NULL DEFAULT now(),
    created_by  uuid,
    UNIQUE (company_id, code)
);

CREATE TABLE products (
    id               uuid PRIMARY KEY,
    company_id       uuid NOT NULL REFERENCES companies(id),
    code             text NOT NULL,
    name             text NOT NULL,
    short_name       text NOT NULL DEFAULT '',
    category_id      uuid REFERENCES categories(id),
    brand_id         uuid REFERENCES brands(id),
    uom_id           uuid NOT NULL REFERENCES uoms(id),
    tax_code         text NOT NULL DEFAULT 'PPN',
    image_key        text NOT NULL DEFAULT '',
    is_rfid_tracked  boolean NOT NULL DEFAULT false,
    status           text NOT NULL DEFAULT 'active' CHECK (status IN ('active','inactive','discontinued')),
    version          integer NOT NULL DEFAULT 1,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    created_by       uuid,
    UNIQUE (company_id, code)
);
CREATE INDEX products_category_idx ON products (category_id);
CREATE INDEX products_brand_idx ON products (brand_id);
CREATE INDEX products_name_idx ON products (company_id, lower(name));

-- SKU level. Selling price is NOT here (Phase 4 price lists); purchase_price/cost are defaults only.
CREATE TABLE product_variants (
    id              uuid PRIMARY KEY,
    company_id      uuid NOT NULL REFERENCES companies(id),
    product_id      uuid NOT NULL REFERENCES products(id) ON DELETE CASCADE,
    sku             text NOT NULL,
    name            text NOT NULL DEFAULT '',
    attributes      jsonb NOT NULL DEFAULT '{}'::jsonb,
    purchase_price  numeric(19,4) NOT NULL DEFAULT 0,
    cost            numeric(19,4) NOT NULL DEFAULT 0,
    min_stock       numeric(19,4) NOT NULL DEFAULT 0,
    max_stock       numeric(19,4) NOT NULL DEFAULT 0,
    reorder_point   numeric(19,4) NOT NULL DEFAULT 0,
    status          text NOT NULL DEFAULT 'active' CHECK (status IN ('active','inactive')),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    created_by      uuid,
    UNIQUE (company_id, sku)
);
CREATE INDEX product_variants_product_idx ON product_variants (product_id);

-- Multiple barcodes per variant; unique per company so a scan resolves to exactly one variant.
CREATE TABLE barcodes (
    id          uuid PRIMARY KEY,
    company_id  uuid NOT NULL REFERENCES companies(id),
    variant_id  uuid NOT NULL REFERENCES product_variants(id) ON DELETE CASCADE,
    barcode     text NOT NULL,
    kind        text NOT NULL DEFAULT 'EAN13',
    is_primary  boolean NOT NULL DEFAULT false,
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (company_id, barcode)
);
CREATE INDEX barcodes_variant_idx ON barcodes (variant_id);

CREATE TABLE suppliers (
    id                  uuid PRIMARY KEY,
    company_id          uuid NOT NULL REFERENCES companies(id),
    code                text NOT NULL,
    name                text NOT NULL,
    pic                 text NOT NULL DEFAULT '',
    phone               text NOT NULL DEFAULT '',
    email               text NOT NULL DEFAULT '',
    address             text NOT NULL DEFAULT '',
    npwp                text NOT NULL DEFAULT '',
    payment_term_days   integer NOT NULL DEFAULT 0,
    bank_account        jsonb NOT NULL DEFAULT '{}'::jsonb,
    lead_time_days      integer NOT NULL DEFAULT 0,
    currency            text NOT NULL DEFAULT 'IDR',
    status              text NOT NULL DEFAULT 'active' CHECK (status IN ('active','inactive')),
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    created_by          uuid,
    UNIQUE (company_id, code)
);

CREATE TABLE supplier_products (
    supplier_id     uuid NOT NULL REFERENCES suppliers(id) ON DELETE CASCADE,
    variant_id      uuid NOT NULL REFERENCES product_variants(id) ON DELETE CASCADE,
    company_id      uuid NOT NULL REFERENCES companies(id),
    supplier_sku    text NOT NULL DEFAULT '',
    purchase_price  numeric(19,4) NOT NULL DEFAULT 0,
    moq             numeric(19,4) NOT NULL DEFAULT 1,
    lead_time_days  integer NOT NULL DEFAULT 0,
    PRIMARY KEY (supplier_id, variant_id)
);

-- +goose Down
DROP TABLE supplier_products;
DROP TABLE suppliers;
DROP TABLE barcodes;
DROP TABLE product_variants;
DROP TABLE products;
DROP TABLE uoms;
DROP TABLE brands;
DROP TABLE categories;
