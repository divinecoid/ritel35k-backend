-- +goose Up

CREATE TABLE pos_terminals (
    id          uuid PRIMARY KEY,
    company_id  uuid NOT NULL REFERENCES companies(id),
    store_id    uuid NOT NULL REFERENCES stores(id),
    code        text NOT NULL,
    name        text NOT NULL,
    status      text NOT NULL DEFAULT 'active' CHECK (status IN ('active','inactive')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    created_by  uuid,
    UNIQUE (store_id, code)
);

-- Every store that already exists gets the same default terminal new stores get at provisioning.
INSERT INTO pos_terminals (id, company_id, store_id, code, name)
SELECT gen_random_uuid(), company_id, id, 'POS-01', 'Kasir 1' FROM stores;

CREATE TABLE cashier_shifts (
    id            uuid PRIMARY KEY,
    company_id    uuid NOT NULL REFERENCES companies(id),
    store_id      uuid NOT NULL REFERENCES stores(id),
    terminal_id   uuid NOT NULL REFERENCES pos_terminals(id),
    cashier_id    uuid NOT NULL REFERENCES users(id),
    status        text NOT NULL DEFAULT 'open' CHECK (status IN ('open','closed')),
    opening_cash  numeric(19,4) NOT NULL CHECK (opening_cash >= 0),
    opened_at     timestamptz NOT NULL DEFAULT now(),
    closed_at     timestamptz,
    closed_by     uuid,
    expected_cash numeric(19,4),
    counted_cash  numeric(19,4),
    variance      numeric(19,4),
    note          text NOT NULL DEFAULT ''
);
-- one open shift per terminal and per cashier
CREATE UNIQUE INDEX cashier_shifts_one_open_terminal ON cashier_shifts (terminal_id) WHERE status = 'open';
CREATE UNIQUE INDEX cashier_shifts_one_open_cashier ON cashier_shifts (cashier_id) WHERE status = 'open';
CREATE INDEX cashier_shifts_store_idx ON cashier_shifts (store_id, opened_at DESC);

CREATE TABLE cash_movements (
    id          uuid PRIMARY KEY,
    company_id  uuid NOT NULL,
    store_id    uuid NOT NULL,
    shift_id    uuid NOT NULL REFERENCES cashier_shifts(id),
    type        text NOT NULL CHECK (type IN ('CASH_IN','CASH_OUT','REFUND')),
    amount      numeric(19,4) NOT NULL CHECK (amount > 0),
    reason      text NOT NULL DEFAULT '',
    ref_type    text NOT NULL DEFAULT '',
    ref_id      uuid,
    created_by  uuid NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX cash_movements_shift_idx ON cash_movements (shift_id);

CREATE TABLE sales (
    id               uuid PRIMARY KEY,
    company_id       uuid NOT NULL REFERENCES companies(id),
    store_id         uuid NOT NULL REFERENCES stores(id),
    terminal_id      uuid NOT NULL REFERENCES pos_terminals(id),
    shift_id         uuid NOT NULL REFERENCES cashier_shifts(id),
    cashier_id       uuid NOT NULL REFERENCES users(id),
    document_number  text NOT NULL,
    client_txn_id    uuid NOT NULL,
    status           text NOT NULL DEFAULT 'completed' CHECK (status IN ('completed','voided')),
    business_date    date NOT NULL,
    subtotal         numeric(19,4) NOT NULL,   -- gross before discounts
    discount_total   numeric(19,4) NOT NULL,
    tax_total        numeric(19,4) NOT NULL,
    grand_total      numeric(19,4) NOT NULL,
    paid_total       numeric(19,4) NOT NULL,   -- applied to the bill
    change_amount    numeric(19,4) NOT NULL DEFAULT 0,
    refunded_total   numeric(19,4) NOT NULL DEFAULT 0,
    approved_by      uuid,
    void_reason      text NOT NULL DEFAULT '',
    voided_by        uuid,
    voided_at        timestamptz,
    created_at       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (company_id, document_number),
    UNIQUE (terminal_id, client_txn_id)
);
CREATE INDEX sales_store_date_idx ON sales (store_id, business_date DESC, created_at DESC);
CREATE INDEX sales_shift_idx ON sales (shift_id);
CREATE INDEX sales_cashier_idx ON sales (cashier_id, created_at DESC);

CREATE TABLE sale_items (
    id               uuid PRIMARY KEY,
    sale_id          uuid NOT NULL REFERENCES sales(id) ON DELETE CASCADE,
    line_no          integer NOT NULL,
    variant_id       uuid NOT NULL REFERENCES product_variants(id),
    sku              text NOT NULL,
    product_name     text NOT NULL,
    barcode          text NOT NULL DEFAULT '',
    qty              numeric(19,4) NOT NULL CHECK (qty > 0),
    unit_price       numeric(19,4) NOT NULL,
    discount_percent numeric(7,4) NOT NULL DEFAULT 0,
    discount_amount  numeric(19,4) NOT NULL DEFAULT 0,
    tax_rate         numeric(7,4) NOT NULL DEFAULT 0,
    tax_amount       numeric(19,4) NOT NULL DEFAULT 0,
    line_total       numeric(19,4) NOT NULL,
    unit_cost        numeric(19,4) NOT NULL DEFAULT 0,   -- COGS snapshot (moving average at sale time)
    warehouse_id     uuid NOT NULL REFERENCES warehouses(id),
    UNIQUE (sale_id, line_no)
);
CREATE INDEX sale_items_variant_idx ON sale_items (variant_id);

CREATE TABLE payments (
    id         uuid PRIMARY KEY,
    sale_id    uuid NOT NULL REFERENCES sales(id) ON DELETE CASCADE,
    method     text NOT NULL,
    amount     numeric(19,4) NOT NULL CHECK (amount > 0),  -- applied to the bill
    tendered   numeric(19,4) NOT NULL,                     -- cash handed over (== amount for non-cash)
    reference  text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX payments_sale_idx ON payments (sale_id);

CREATE TABLE sale_returns (
    id               uuid PRIMARY KEY,
    company_id       uuid NOT NULL,
    store_id         uuid NOT NULL REFERENCES stores(id),
    sale_id          uuid NOT NULL REFERENCES sales(id),
    shift_id         uuid REFERENCES cashier_shifts(id),
    document_number  text NOT NULL,
    reason           text NOT NULL,
    refund_method    text NOT NULL,
    refund_amount    numeric(19,4) NOT NULL,
    approved_by      uuid,
    created_by       uuid NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (company_id, document_number)
);
CREATE INDEX sale_returns_sale_idx ON sale_returns (sale_id);

CREATE TABLE sale_return_items (
    id              uuid PRIMARY KEY,
    return_id       uuid NOT NULL REFERENCES sale_returns(id) ON DELETE CASCADE,
    sale_item_id    uuid NOT NULL REFERENCES sale_items(id),
    qty             numeric(19,4) NOT NULL CHECK (qty > 0),
    amount          numeric(19,4) NOT NULL,
    restock         boolean NOT NULL DEFAULT true,
    reason          text NOT NULL DEFAULT ''
);
CREATE INDEX sale_return_items_item_idx ON sale_return_items (sale_item_id);

-- +goose Down
DROP TABLE sale_return_items;
DROP TABLE sale_returns;
DROP TABLE payments;
DROP TABLE sale_items;
DROP TABLE sales;
DROP TABLE cash_movements;
DROP TABLE cashier_shifts;
DROP TABLE pos_terminals;
