-- +goose Up

ALTER TABLE inventory_movements DROP CONSTRAINT inventory_movements_movement_type_check;
ALTER TABLE inventory_movements ADD CONSTRAINT inventory_movements_movement_type_check CHECK (movement_type IN
    ('OPENING','PURCHASE','PURCHASE_RETURN','SALE','SALE_VOID','SALE_RETURN','TRANSFER_OUT','TRANSFER_IN','ADJUSTMENT','DAMAGE','OPNAME'));

-- ---------------------------------------------------------------- purchasing
CREATE TABLE purchase_orders (
    id               uuid PRIMARY KEY,
    company_id       uuid NOT NULL REFERENCES companies(id),
    store_id         uuid REFERENCES stores(id),               -- NULL = central warehouse
    warehouse_id     uuid NOT NULL REFERENCES warehouses(id),
    supplier_id      uuid NOT NULL REFERENCES suppliers(id),
    document_number  text NOT NULL,
    status           text NOT NULL DEFAULT 'draft' CHECK (status IN ('draft','submitted','approved','partially_received','received','cancelled')),
    expected_date    date,
    note             text NOT NULL DEFAULT '',
    total            numeric(19,4) NOT NULL DEFAULT 0,
    version          integer NOT NULL DEFAULT 1,
    created_by       uuid NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    submitted_at     timestamptz,
    approved_by      uuid,
    approved_at      timestamptz,
    cancel_reason    text NOT NULL DEFAULT '',
    UNIQUE (company_id, document_number)
);
CREATE INDEX purchase_orders_store_idx ON purchase_orders (store_id, created_at DESC);
CREATE INDEX purchase_orders_supplier_idx ON purchase_orders (supplier_id, created_at DESC);

CREATE TABLE purchase_order_items (
    id            uuid PRIMARY KEY,
    po_id         uuid NOT NULL REFERENCES purchase_orders(id) ON DELETE CASCADE,
    line_no       integer NOT NULL,
    variant_id    uuid NOT NULL REFERENCES product_variants(id),
    qty_ordered   numeric(19,4) NOT NULL CHECK (qty_ordered > 0),
    unit_price    numeric(19,4) NOT NULL CHECK (unit_price >= 0),
    qty_received  numeric(19,4) NOT NULL DEFAULT 0,
    UNIQUE (po_id, line_no),
    UNIQUE (po_id, variant_id)
);

CREATE TABLE goods_receipts (
    id               uuid PRIMARY KEY,
    company_id       uuid NOT NULL REFERENCES companies(id),
    store_id         uuid REFERENCES stores(id),
    warehouse_id     uuid NOT NULL REFERENCES warehouses(id),
    supplier_id      uuid NOT NULL REFERENCES suppliers(id),
    po_id            uuid REFERENCES purchase_orders(id),
    document_number  text NOT NULL,
    supplier_ref     text NOT NULL DEFAULT '',   -- supplier delivery note / invoice number
    received_date    date NOT NULL,
    note             text NOT NULL DEFAULT '',
    total_cost       numeric(19,4) NOT NULL DEFAULT 0,
    created_by       uuid NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (company_id, document_number)
);
CREATE INDEX goods_receipts_po_idx ON goods_receipts (po_id);
CREATE INDEX goods_receipts_store_idx ON goods_receipts (store_id, created_at DESC);

CREATE TABLE goods_receipt_items (
    id           uuid PRIMARY KEY,
    grn_id       uuid NOT NULL REFERENCES goods_receipts(id) ON DELETE CASCADE,
    line_no      integer NOT NULL,
    po_item_id   uuid REFERENCES purchase_order_items(id),
    variant_id   uuid NOT NULL REFERENCES product_variants(id),
    qty          numeric(19,4) NOT NULL CHECK (qty > 0),
    unit_cost    numeric(19,4) NOT NULL CHECK (unit_cost >= 0),
    UNIQUE (grn_id, line_no)
);

CREATE TABLE purchase_returns (
    id               uuid PRIMARY KEY,
    company_id       uuid NOT NULL REFERENCES companies(id),
    store_id         uuid REFERENCES stores(id),
    warehouse_id     uuid NOT NULL REFERENCES warehouses(id),
    supplier_id      uuid NOT NULL REFERENCES suppliers(id),
    grn_id           uuid REFERENCES goods_receipts(id),
    document_number  text NOT NULL,
    reason           text NOT NULL,
    total_cost       numeric(19,4) NOT NULL DEFAULT 0,
    created_by       uuid NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (company_id, document_number)
);

CREATE TABLE purchase_return_items (
    id          uuid PRIMARY KEY,
    return_id   uuid NOT NULL REFERENCES purchase_returns(id) ON DELETE CASCADE,
    line_no     integer NOT NULL,
    variant_id  uuid NOT NULL REFERENCES product_variants(id),
    qty         numeric(19,4) NOT NULL CHECK (qty > 0),
    unit_cost   numeric(19,4) NOT NULL,
    UNIQUE (return_id, line_no)
);

-- ---------------------------------------------------------------- transfers
CREATE TABLE stock_transfers (
    id                 uuid PRIMARY KEY,
    company_id         uuid NOT NULL REFERENCES companies(id),
    from_warehouse_id  uuid NOT NULL REFERENCES warehouses(id),
    to_warehouse_id    uuid NOT NULL REFERENCES warehouses(id),
    from_store_id      uuid REFERENCES stores(id),
    to_store_id        uuid REFERENCES stores(id),
    document_number    text NOT NULL,
    status             text NOT NULL DEFAULT 'draft' CHECK (status IN ('draft','submitted','approved','shipped','received','cancelled')),
    note               text NOT NULL DEFAULT '',
    created_by         uuid NOT NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    submitted_at       timestamptz,
    approved_by        uuid,
    approved_at        timestamptz,
    shipped_by         uuid,
    shipped_at         timestamptz,
    received_by        uuid,
    received_at        timestamptz,
    cancel_reason      text NOT NULL DEFAULT '',
    UNIQUE (company_id, document_number),
    CHECK (from_warehouse_id <> to_warehouse_id)
);
CREATE INDEX stock_transfers_from_idx ON stock_transfers (from_store_id, created_at DESC);
CREATE INDEX stock_transfers_to_idx ON stock_transfers (to_store_id, created_at DESC);

CREATE TABLE stock_transfer_items (
    id             uuid PRIMARY KEY,
    transfer_id    uuid NOT NULL REFERENCES stock_transfers(id) ON DELETE CASCADE,
    line_no        integer NOT NULL,
    variant_id     uuid NOT NULL REFERENCES product_variants(id),
    qty_requested  numeric(19,4) NOT NULL CHECK (qty_requested > 0),
    qty_shipped    numeric(19,4) NOT NULL DEFAULT 0,
    qty_received   numeric(19,4) NOT NULL DEFAULT 0,
    unit_cost      numeric(19,4) NOT NULL DEFAULT 0,    -- origin moving average at shipment
    UNIQUE (transfer_id, line_no),
    UNIQUE (transfer_id, variant_id)
);

-- ---------------------------------------------------------------- stock count (opname)
CREATE TABLE stock_counts (
    id               uuid PRIMARY KEY,
    company_id       uuid NOT NULL REFERENCES companies(id),
    store_id         uuid REFERENCES stores(id),
    warehouse_id     uuid NOT NULL REFERENCES warehouses(id),
    document_number  text NOT NULL,
    kind             text NOT NULL CHECK (kind IN ('full','partial','category','cycle')),
    status           text NOT NULL DEFAULT 'counting' CHECK (status IN ('counting','submitted','approved','cancelled')),
    note             text NOT NULL DEFAULT '',
    snapshot_at      timestamptz NOT NULL DEFAULT now(),
    created_by       uuid NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    submitted_by     uuid,
    submitted_at     timestamptz,
    approved_by      uuid,
    approved_at      timestamptz,
    UNIQUE (company_id, document_number)
);
CREATE INDEX stock_counts_store_idx ON stock_counts (store_id, created_at DESC);

CREATE TABLE stock_count_items (
    id           uuid PRIMARY KEY,
    count_id     uuid NOT NULL REFERENCES stock_counts(id) ON DELETE CASCADE,
    variant_id   uuid NOT NULL REFERENCES product_variants(id),
    system_qty   numeric(19,4) NOT NULL,                 -- frozen at snapshot_at
    counted_qty  numeric(19,4),
    counted_at   timestamptz,
    expected_qty numeric(19,4),                          -- system_qty + movements between snapshot and counted_at (set on submit)
    variance     numeric(19,4),                          -- counted - expected
    unit_cost    numeric(19,4) NOT NULL DEFAULT 0,
    UNIQUE (count_id, variant_id)
);

-- +goose Down
DROP TABLE stock_count_items;
DROP TABLE stock_counts;
DROP TABLE stock_transfer_items;
DROP TABLE stock_transfers;
DROP TABLE purchase_return_items;
DROP TABLE purchase_returns;
DROP TABLE goods_receipt_items;
DROP TABLE goods_receipts;
DROP TABLE purchase_order_items;
DROP TABLE purchase_orders;
