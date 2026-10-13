-- +goose Up

-- Offline POS: a terminal's edge agent syncs what happened while the cloud was unreachable.

-- Device credential per terminal (separate table so the hash can never leak through terminal listings).
CREATE TABLE terminal_sync_tokens (
    terminal_id    uuid PRIMARY KEY REFERENCES pos_terminals(id) ON DELETE CASCADE,
    company_id     uuid NOT NULL,
    token_hash     text NOT NULL,
    token_hint     text NOT NULL DEFAULT '',
    created_at     timestamptz NOT NULL DEFAULT now(),
    last_sync_at   timestamptz,
    last_ip        text NOT NULL DEFAULT '',
    agent_version  text NOT NULL DEFAULT '',
    pending        integer NOT NULL DEFAULT 0,            -- commands still queued at the edge (last report)
    snapshot_version text NOT NULL DEFAULT ''             -- catalogue version the edge last downloaded
);
CREATE UNIQUE INDEX terminal_sync_tokens_hash_idx ON terminal_sync_tokens (token_hash);

ALTER TABLE sales
    ADD COLUMN offline          boolean NOT NULL DEFAULT false,
    ADD COLUMN occurred_at      timestamptz,
    ADD COLUMN local_number     text NOT NULL DEFAULT '',     -- the number printed on the receipt while offline
    ADD COLUMN snapshot_version text NOT NULL DEFAULT '';
CREATE INDEX sales_offline_idx ON sales (store_id, created_at DESC) WHERE offline;

ALTER TABLE cashier_shifts
    ADD COLUMN client_shift_id uuid,
    ADD COLUMN offline boolean NOT NULL DEFAULT false;
CREATE UNIQUE INDEX cashier_shifts_client_idx ON cashier_shifts (terminal_id, client_shift_id) WHERE client_shift_id IS NOT NULL;

-- Exception queue: a real sale is never rejected; whatever looks wrong is recorded here for a person to review.
CREATE TABLE offline_exceptions (
    id           uuid PRIMARY KEY,
    company_id   uuid NOT NULL,
    store_id     uuid NOT NULL REFERENCES stores(id),
    terminal_id  uuid REFERENCES pos_terminals(id),
    sale_id      uuid,
    shift_id     uuid,
    type         text NOT NULL,
    severity     text NOT NULL DEFAULT 'warning' CHECK (severity IN ('info','warning','critical')),
    title        text NOT NULL,
    detail       jsonb NOT NULL DEFAULT '{}'::jsonb,
    status       text NOT NULL DEFAULT 'open' CHECK (status IN ('open','reviewed','dismissed')),
    dedupe_key   text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now(),
    reviewed_by  uuid,
    reviewed_at  timestamptz,
    review_note  text NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX offline_exceptions_dedupe_idx ON offline_exceptions (company_id, dedupe_key) WHERE dedupe_key <> '';
CREATE INDEX offline_exceptions_store_idx ON offline_exceptions (store_id, created_at DESC);
CREATE INDEX offline_exceptions_status_idx ON offline_exceptions (company_id, status, created_at DESC);

ALTER TABLE security_events DROP CONSTRAINT security_events_type_check;
ALTER TABLE security_events ADD CONSTRAINT security_events_type_check CHECK (type IN
    ('UNAUTHORIZED_EXIT','UNKNOWN_TAG','RFID_MISMATCH','EXCESSIVE_VOIDS','LARGE_REFUND','HIGH_DISCOUNT','CASH_VARIANCE','READER_OFFLINE','OFFLINE_PRICE_MISMATCH'));

-- +goose Down
ALTER TABLE security_events DROP CONSTRAINT security_events_type_check;
ALTER TABLE security_events ADD CONSTRAINT security_events_type_check CHECK (type IN
    ('UNAUTHORIZED_EXIT','UNKNOWN_TAG','RFID_MISMATCH','EXCESSIVE_VOIDS','LARGE_REFUND','HIGH_DISCOUNT','CASH_VARIANCE','READER_OFFLINE'));
DROP TABLE offline_exceptions;
DROP INDEX cashier_shifts_client_idx;
ALTER TABLE cashier_shifts DROP COLUMN offline, DROP COLUMN client_shift_id;
DROP INDEX sales_offline_idx;
ALTER TABLE sales DROP COLUMN snapshot_version, DROP COLUMN local_number, DROP COLUMN occurred_at, DROP COLUMN offline;
DROP TABLE terminal_sync_tokens;
