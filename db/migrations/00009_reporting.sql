-- +goose Up

-- Summary layer. Dashboards and reports read these tables, never sales/sale_items.
-- Each (store, business_date) slice is REBUILT from the source tables whenever it is marked dirty, so
-- every event is idempotent, bursts of sales coalesce into one rebuild, and a rebuild is also the repair.
-- business_date is the store's local date (stores.timezone).
CREATE TABLE summary_dirty (
    store_id       uuid NOT NULL REFERENCES stores(id),
    business_date  date NOT NULL,
    company_id     uuid NOT NULL,
    marked_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (store_id, business_date)
);
CREATE INDEX summary_dirty_marked_idx ON summary_dirty (marked_at);

CREATE TABLE store_kpi_daily (
    store_id       uuid NOT NULL REFERENCES stores(id),
    business_date  date NOT NULL,
    company_id     uuid NOT NULL,
    transactions   integer NOT NULL DEFAULT 0,
    voided         integer NOT NULL DEFAULT 0,
    returns_count  integer NOT NULL DEFAULT 0,
    items_sold     numeric(19,4) NOT NULL DEFAULT 0,
    gross_sales    numeric(19,4) NOT NULL DEFAULT 0,   -- before discounts
    discount       numeric(19,4) NOT NULL DEFAULT 0,
    tax            numeric(19,4) NOT NULL DEFAULT 0,
    grand_total    numeric(19,4) NOT NULL DEFAULT 0,   -- collected, tax included
    refund         numeric(19,4) NOT NULL DEFAULT 0,   -- refunds made on this date, tax included
    net_sales      numeric(19,4) NOT NULL DEFAULT 0,   -- (grand_total - tax) - refunds without tax
    cogs           numeric(19,4) NOT NULL DEFAULT 0,   -- cost of goods sold minus restocked returns
    gross_profit   numeric(19,4) NOT NULL DEFAULT 0,   -- net_sales - cogs
    rebuilt_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (store_id, business_date)
);
CREATE INDEX store_kpi_daily_company_date_idx ON store_kpi_daily (company_id, business_date);

CREATE TABLE sales_hourly (
    store_id       uuid NOT NULL REFERENCES stores(id),
    business_date  date NOT NULL,
    hour           smallint NOT NULL CHECK (hour BETWEEN 0 AND 23),
    company_id     uuid NOT NULL,
    transactions   integer NOT NULL DEFAULT 0,
    net_sales      numeric(19,4) NOT NULL DEFAULT 0,
    PRIMARY KEY (store_id, business_date, hour)
);
CREATE INDEX sales_hourly_company_date_idx ON sales_hourly (company_id, business_date);

-- category_id / brand_id are denormalized so category and brand reports need no join to the catalog tree.
CREATE TABLE sales_by_product_daily (
    store_id       uuid NOT NULL REFERENCES stores(id),
    business_date  date NOT NULL,
    variant_id     uuid NOT NULL REFERENCES product_variants(id),
    company_id     uuid NOT NULL,
    category_id    uuid,
    brand_id       uuid,
    qty            numeric(19,4) NOT NULL DEFAULT 0,   -- net of returns
    net_sales      numeric(19,4) NOT NULL DEFAULT 0,   -- tax excluded, net of returns
    discount       numeric(19,4) NOT NULL DEFAULT 0,
    cogs           numeric(19,4) NOT NULL DEFAULT 0,
    PRIMARY KEY (store_id, business_date, variant_id)
);
CREATE INDEX sales_by_product_daily_company_date_idx ON sales_by_product_daily (company_id, business_date);
CREATE INDEX sales_by_product_daily_variant_idx ON sales_by_product_daily (variant_id, business_date);

CREATE TABLE payment_method_daily (
    store_id       uuid NOT NULL REFERENCES stores(id),
    business_date  date NOT NULL,
    method         text NOT NULL,
    company_id     uuid NOT NULL,
    payments       integer NOT NULL DEFAULT 0,
    amount         numeric(19,4) NOT NULL DEFAULT 0,
    PRIMARY KEY (store_id, business_date, method)
);
CREATE INDEX payment_method_daily_company_date_idx ON payment_method_daily (company_id, business_date);

-- Returns are located by the store's local date, so rebuilding a day needs a sargable range on created_at.
CREATE INDEX sale_returns_store_time_idx ON sale_returns (store_id, created_at);

-- Anything sold before this migration gets summarised by the worker.
INSERT INTO summary_dirty (store_id, business_date, company_id)
SELECT DISTINCT store_id, business_date, company_id FROM sales
ON CONFLICT DO NOTHING;
INSERT INTO summary_dirty (store_id, business_date, company_id)
SELECT DISTINCT r.store_id, (r.created_at AT TIME ZONE st.timezone)::date, r.company_id
FROM sale_returns r JOIN stores st ON st.id = r.store_id
ON CONFLICT DO NOTHING;

-- In-app notifications, fanned out to each recipient when raised (recipients are chosen by permission + scope).
CREATE TABLE notifications (
    id           uuid PRIMARY KEY,
    company_id   uuid NOT NULL,
    user_id      uuid NOT NULL REFERENCES users(id),
    store_id     uuid REFERENCES stores(id),
    type         text NOT NULL,
    severity     text NOT NULL DEFAULT 'info' CHECK (severity IN ('info','warning','critical')),
    title        text NOT NULL,
    body         text NOT NULL DEFAULT '',
    entity_type  text NOT NULL DEFAULT '',
    entity_id    text NOT NULL DEFAULT '',
    dedupe_key   text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now(),
    read_at      timestamptz
);
CREATE INDEX notifications_user_idx ON notifications (user_id, created_at DESC);
CREATE INDEX notifications_unread_idx ON notifications (user_id) WHERE read_at IS NULL;
CREATE UNIQUE INDEX notifications_dedupe_idx ON notifications (user_id, dedupe_key) WHERE dedupe_key <> '';

-- +goose Down
DROP TABLE notifications;
DROP INDEX sale_returns_store_time_idx;
DROP TABLE payment_method_daily;
DROP TABLE sales_by_product_daily;
DROP TABLE sales_hourly;
DROP TABLE store_kpi_daily;
DROP TABLE summary_dirty;
