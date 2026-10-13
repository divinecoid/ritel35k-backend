-- +goose Up

-- ------------------------------------------------------------ membership & customers
CREATE TABLE membership_levels (
    id               uuid PRIMARY KEY,
    company_id       uuid NOT NULL REFERENCES companies(id),
    code             text NOT NULL,
    name             text NOT NULL,
    rank             integer NOT NULL,                       -- 0 = lowest
    min_spend        numeric(19,4) NOT NULL DEFAULT 0,       -- lifetime spend that qualifies for this level
    earn_multiplier  numeric(6,2) NOT NULL DEFAULT 1 CHECK (earn_multiplier >= 0),
    is_default       boolean NOT NULL DEFAULT false,
    created_at       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (company_id, code),
    UNIQUE (company_id, rank)
);
CREATE UNIQUE INDEX membership_levels_one_default ON membership_levels (company_id) WHERE is_default;

CREATE TABLE customers (
    id                uuid PRIMARY KEY,
    company_id        uuid NOT NULL REFERENCES companies(id),
    member_no         text NOT NULL,
    name              text NOT NULL,
    phone             text NOT NULL,
    email             text NOT NULL DEFAULT '',
    birthday          date,
    gender            text NOT NULL DEFAULT '' CHECK (gender IN ('','M','F')),
    address           text NOT NULL DEFAULT '',
    notes             text NOT NULL DEFAULT '',
    level_id          uuid NOT NULL REFERENCES membership_levels(id),
    points_balance    bigint NOT NULL DEFAULT 0,
    lifetime_spend    numeric(19,4) NOT NULL DEFAULT 0,
    last_purchase_at  timestamptz,
    home_store_id     uuid REFERENCES stores(id),
    status            text NOT NULL DEFAULT 'active' CHECK (status IN ('active','inactive')),
    created_by        uuid,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    UNIQUE (company_id, member_no),
    UNIQUE (company_id, phone)
);
CREATE INDEX customers_name_idx ON customers (company_id, lower(name));

-- Append-only points ledger; customers.points_balance is the cached sum.
CREATE TABLE loyalty_transactions (
    id             uuid PRIMARY KEY,
    company_id     uuid NOT NULL,
    customer_id    uuid NOT NULL REFERENCES customers(id),
    kind           text NOT NULL CHECK (kind IN ('earn','redeem','adjust','reverse_earn','restore_redeem','expire')),
    points         bigint NOT NULL,                 -- signed
    balance_after  bigint NOT NULL,
    sale_id        uuid,
    expires_at     timestamptz,                     -- earn rows only
    note           text NOT NULL DEFAULT '',
    created_by     uuid,
    created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX loyalty_customer_idx ON loyalty_transactions (customer_id, created_at DESC);
CREATE INDEX loyalty_sale_idx ON loyalty_transactions (sale_id);

-- ------------------------------------------------------------ price lists
CREATE TABLE price_lists (
    id                   uuid PRIMARY KEY,
    company_id           uuid NOT NULL REFERENCES companies(id),
    code                 text NOT NULL,
    name                 text NOT NULL,
    membership_level_id  uuid REFERENCES membership_levels(id),   -- set => only for customers of this level
    is_default           boolean NOT NULL DEFAULT false,
    status               text NOT NULL DEFAULT 'active' CHECK (status IN ('active','inactive')),
    created_at           timestamptz NOT NULL DEFAULT now(),
    UNIQUE (company_id, code)
);
CREATE UNIQUE INDEX price_lists_one_default ON price_lists (company_id) WHERE is_default;

INSERT INTO price_lists (id, company_id, code, name, is_default)
SELECT gen_random_uuid(), id, 'STANDARD', 'Harga Normal', true FROM companies;

ALTER TABLE prices ADD COLUMN price_list_id uuid REFERENCES price_lists(id);
UPDATE prices p SET price_list_id = pl.id FROM price_lists pl WHERE pl.company_id = p.company_id AND pl.is_default;
ALTER TABLE prices ALTER COLUMN price_list_id SET NOT NULL;
DROP INDEX prices_lookup_idx;
CREATE INDEX prices_lookup_idx ON prices (variant_id, price_list_id, org_unit_id, min_qty, effective_from DESC);

-- Default membership levels for companies that already exist (new companies get them at bootstrap).
INSERT INTO membership_levels (id, company_id, code, name, rank, min_spend, earn_multiplier, is_default)
SELECT gen_random_uuid(), c.id, v.code, v.name, v.rank, v.min_spend, v.mult, v.rank = 0
FROM companies c CROSS JOIN (VALUES
    ('REGULAR',  'Regular',  0, 0::numeric,        1.00::numeric),
    ('SILVER',   'Silver',   1, 5000000::numeric,  1.25::numeric),
    ('GOLD',     'Gold',     2, 20000000::numeric, 1.50::numeric),
    ('PLATINUM', 'Platinum', 3, 50000000::numeric, 2.00::numeric)
) AS v(code, name, rank, min_spend, mult);

-- ------------------------------------------------------------ promotions
CREATE TABLE promotions (
    id            uuid PRIMARY KEY,
    company_id    uuid NOT NULL REFERENCES companies(id),
    code          text NOT NULL,
    name          text NOT NULL,
    description   text NOT NULL DEFAULT '',
    type          text NOT NULL CHECK (type IN ('percent','fixed','bxgy','bundle','threshold')),
    config        jsonb NOT NULL,
    priority      integer NOT NULL DEFAULT 100,           -- lower runs first
    stackable     boolean NOT NULL DEFAULT true,
    exclusive     boolean NOT NULL DEFAULT false,
    starts_at     timestamptz,
    ends_at       timestamptz,
    days_of_week  smallint[],                             -- 0=Sunday..6 ; NULL = every day
    time_from     text CHECK (time_from IS NULL OR time_from ~ '^([01][0-9]|2[0-3]):[0-5][0-9]$'),   -- store-local HH:MM
    time_to       text CHECK (time_to IS NULL OR time_to ~ '^([01][0-9]|2[0-3]):[0-5][0-9]$'),
    status        text NOT NULL DEFAULT 'active' CHECK (status IN ('active','inactive')),
    version       integer NOT NULL DEFAULT 1,
    created_by    uuid,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (company_id, code)
);

-- scope_type: org_unit (stores below it), product (variant), category, brand, level (membership)
CREATE TABLE promotion_scopes (
    promotion_id  uuid NOT NULL REFERENCES promotions(id) ON DELETE CASCADE,
    scope_type    text NOT NULL CHECK (scope_type IN ('org_unit','product','category','brand','level')),
    scope_id      uuid NOT NULL,
    PRIMARY KEY (promotion_id, scope_type, scope_id)
);

-- ------------------------------------------------------------ vouchers
CREATE TABLE vouchers (
    id              uuid PRIMARY KEY,
    company_id      uuid NOT NULL REFERENCES companies(id),
    code            citext NOT NULL,
    kind            text NOT NULL CHECK (kind IN ('fixed','percent')),
    value           numeric(19,4) NOT NULL CHECK (value > 0),
    min_spend       numeric(19,4) NOT NULL DEFAULT 0,
    max_discount    numeric(19,4),                          -- cap for percent vouchers
    max_uses        integer,                                -- NULL = unlimited
    used_count      integer NOT NULL DEFAULT 0,
    customer_id     uuid REFERENCES customers(id),          -- personal voucher
    valid_from      timestamptz,
    valid_to        timestamptz,
    status          text NOT NULL DEFAULT 'active' CHECK (status IN ('active','inactive')),
    batch           text NOT NULL DEFAULT '',
    note            text NOT NULL DEFAULT '',
    created_by      uuid,
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (company_id, code),
    CHECK (max_uses IS NULL OR used_count <= max_uses)
);

CREATE TABLE voucher_redemptions (
    id           uuid PRIMARY KEY,
    voucher_id   uuid NOT NULL REFERENCES vouchers(id),
    sale_id      uuid NOT NULL REFERENCES sales(id),
    customer_id  uuid REFERENCES customers(id),
    amount       numeric(19,4) NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    reversed_at  timestamptz,
    UNIQUE (voucher_id, sale_id)
);

-- ------------------------------------------------------------ sales extensions
ALTER TABLE sales
    ADD COLUMN customer_id      uuid REFERENCES customers(id),
    ADD COLUMN voucher_id       uuid REFERENCES vouchers(id),
    ADD COLUMN voucher_discount numeric(19,4) NOT NULL DEFAULT 0,
    ADD COLUMN promo_discount   numeric(19,4) NOT NULL DEFAULT 0,
    ADD COLUMN points_earned    bigint NOT NULL DEFAULT 0,
    ADD COLUMN points_redeemed  bigint NOT NULL DEFAULT 0;
CREATE INDEX sales_customer_idx ON sales (customer_id, created_at DESC);

ALTER TABLE sale_items ADD COLUMN promo_discount numeric(19,4) NOT NULL DEFAULT 0;

CREATE TABLE sale_item_promotions (
    sale_item_id  uuid NOT NULL REFERENCES sale_items(id) ON DELETE CASCADE,
    promotion_id  uuid NOT NULL,
    name          text NOT NULL,
    amount        numeric(19,4) NOT NULL,
    PRIMARY KEY (sale_item_id, promotion_id)
);

-- +goose Down
DROP TABLE sale_item_promotions;
ALTER TABLE sale_items DROP COLUMN promo_discount;
ALTER TABLE sales DROP COLUMN customer_id, DROP COLUMN voucher_id, DROP COLUMN voucher_discount, DROP COLUMN promo_discount, DROP COLUMN points_earned, DROP COLUMN points_redeemed;
DROP TABLE voucher_redemptions;
DROP TABLE vouchers;
DROP TABLE promotion_scopes;
DROP TABLE promotions;
DROP INDEX prices_lookup_idx;
ALTER TABLE prices DROP COLUMN price_list_id;
CREATE INDEX prices_lookup_idx ON prices (variant_id, org_unit_id, effective_from DESC);
DROP TABLE price_lists;
DROP TABLE loyalty_transactions;
DROP TABLE customers;
DROP TABLE membership_levels;
