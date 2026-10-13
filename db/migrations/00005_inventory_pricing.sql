-- +goose Up

-- Cached balance per (warehouse, variant). The ledger below is the source of truth; this table is
-- updated in the same transaction as every movement. store_id NULL = central warehouse.
CREATE TABLE inventory_balances (
    company_id    uuid NOT NULL REFERENCES companies(id),
    store_id      uuid REFERENCES stores(id),
    warehouse_id  uuid NOT NULL REFERENCES warehouses(id),
    variant_id    uuid NOT NULL REFERENCES product_variants(id),
    on_hand       numeric(19,4) NOT NULL DEFAULT 0,
    reserved      numeric(19,4) NOT NULL DEFAULT 0,
    in_transit    numeric(19,4) NOT NULL DEFAULT 0,
    damaged       numeric(19,4) NOT NULL DEFAULT 0,
    quarantine    numeric(19,4) NOT NULL DEFAULT 0,
    avg_cost      numeric(19,4) NOT NULL DEFAULT 0,
    version       bigint NOT NULL DEFAULT 0,
    updated_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (warehouse_id, variant_id)
);
CREATE INDEX inventory_balances_store_idx ON inventory_balances (store_id, variant_id);
CREATE INDEX inventory_balances_variant_idx ON inventory_balances (variant_id);

-- Append-only ledger, partitioned by month. Corrections are new rows, never updates.
CREATE TABLE inventory_movements (
    id               uuid NOT NULL,
    company_id       uuid NOT NULL,
    store_id         uuid,
    warehouse_id     uuid NOT NULL,
    variant_id       uuid NOT NULL,
    movement_type    text NOT NULL CHECK (movement_type IN
        ('OPENING','PURCHASE','SALE','SALE_VOID','SALE_RETURN','TRANSFER_OUT','TRANSFER_IN','ADJUSTMENT','DAMAGE','OPNAME')),
    qty_delta        numeric(19,4) NOT NULL,
    unit_cost        numeric(19,4) NOT NULL DEFAULT 0,
    balance_after    numeric(19,4) NOT NULL,
    ref_type         text NOT NULL DEFAULT '',
    ref_id           uuid,
    document_number  text NOT NULL DEFAULT '',
    note             text NOT NULL DEFAULT '',
    created_by       uuid,
    created_at       timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (id, created_at)
) PARTITION BY RANGE (created_at);
CREATE TABLE inventory_movements_default PARTITION OF inventory_movements DEFAULT;
CREATE INDEX inventory_movements_item_idx ON inventory_movements (warehouse_id, variant_id, created_at DESC);
CREATE INDEX inventory_movements_store_idx ON inventory_movements (store_id, created_at DESC);
CREATE INDEX inventory_movements_ref_idx ON inventory_movements (ref_type, ref_id);

-- +goose StatementBegin
CREATE FUNCTION inventory_movements_immutable() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'inventory_movements is append-only';
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd
CREATE TRIGGER inventory_movements_no_change BEFORE UPDATE OR DELETE ON inventory_movements
    FOR EACH ROW EXECUTE FUNCTION inventory_movements_immutable();

-- Selling prices with inheritance: an override at any org unit applies to everything below it.
-- Resolution: nearest ancestor-or-self with an effective price wins (store > region > company).
-- Rows are never edited; a new price closes the previous open one (history is kept).
CREATE TABLE prices (
    id              uuid PRIMARY KEY,
    company_id      uuid NOT NULL REFERENCES companies(id),
    org_unit_id     uuid NOT NULL REFERENCES org_units(id),
    variant_id      uuid NOT NULL REFERENCES product_variants(id),
    price           numeric(19,4) NOT NULL CHECK (price >= 0),
    min_qty         numeric(19,4) NOT NULL DEFAULT 1,
    effective_from  timestamptz NOT NULL DEFAULT now(),
    effective_to    timestamptz,
    created_by      uuid,
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX prices_lookup_idx ON prices (variant_id, org_unit_id, effective_from DESC);

-- +goose Down
DROP TABLE prices;
DROP TABLE inventory_movements;
DROP FUNCTION inventory_movements_immutable();
DROP TABLE inventory_balances;
