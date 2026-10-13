-- +goose Up

-- Store-level RFID devices. A reader (gate, POS pad, shelf, handheld) authenticates with its own token — never a user JWT.
-- Only a hash of the token is stored; the plaintext is shown once at enrollment / rotation.
CREATE TABLE rfid_readers (
    id            uuid PRIMARY KEY,
    company_id    uuid NOT NULL REFERENCES companies(id),
    store_id      uuid NOT NULL REFERENCES stores(id),
    code          text NOT NULL,
    name          text NOT NULL,
    kind          text NOT NULL CHECK (kind IN ('gate','pos','shelf','handheld')),
    status        text NOT NULL DEFAULT 'active' CHECK (status IN ('active','inactive')),
    token_hash    text NOT NULL,
    token_hint    text NOT NULL DEFAULT '',          -- last characters, to tell tokens apart in the UI
    last_seen_at  timestamptz,
    last_ip       text NOT NULL DEFAULT '',
    agent_version text NOT NULL DEFAULT '',
    stats         jsonb NOT NULL DEFAULT '{}'::jsonb,   -- last heartbeat: buffered events, local alarms, snapshot size/age
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    created_by    uuid,
    UNIQUE (store_id, code)
);
CREATE UNIQUE INDEX rfid_readers_token_idx ON rfid_readers (token_hash);

-- One row per physical tag. EPC is stored upper-case hex. The status machine is enforced in code (internal/rfid).
CREATE TABLE rfid_tags (
    id             uuid PRIMARY KEY,
    company_id     uuid NOT NULL REFERENCES companies(id),
    epc            text NOT NULL,
    variant_id     uuid NOT NULL REFERENCES product_variants(id),
    status         text NOT NULL DEFAULT 'IN_STOCK' CHECK (status IN
        ('REGISTERED','IN_STOCK','ON_SHELF','RESERVED','SOLD','RETURNED','DAMAGED','LOST')),
    store_id       uuid REFERENCES stores(id),       -- NULL = central warehouse / not yet in a store
    location       text NOT NULL DEFAULT '',
    sold_sale_id   uuid,
    sold_at        timestamptz,
    last_seen_at   timestamptz,
    last_reader_id uuid,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    created_by     uuid,
    UNIQUE (company_id, epc)
);
CREATE INDEX rfid_tags_store_status_idx ON rfid_tags (store_id, status);
CREATE INDEX rfid_tags_variant_idx ON rfid_tags (variant_id, store_id);
CREATE INDEX rfid_tags_store_updated_idx ON rfid_tags (store_id, updated_at);   -- edge delta sync
CREATE INDEX rfid_tags_sale_idx ON rfid_tags (sold_sale_id) WHERE sold_sale_id IS NOT NULL;

-- Raw reads reported by the edge (batched, idempotent per reader/client_event_id/read_at). Partitioned monthly.
CREATE TABLE rfid_events (
    id               uuid NOT NULL,
    company_id       uuid NOT NULL,
    store_id         uuid NOT NULL,
    reader_id        uuid NOT NULL,
    client_event_id  uuid NOT NULL,
    epc              text NOT NULL,
    direction        text NOT NULL DEFAULT 'unknown' CHECK (direction IN ('in','out','unknown')),
    rssi             smallint,
    read_at          timestamptz NOT NULL,
    received_at      timestamptz NOT NULL DEFAULT now(),
    local_decision   text NOT NULL DEFAULT '',        -- what the edge decided offline: allow | alarm | ''
    decision         text NOT NULL DEFAULT 'ignored', -- what the server decided: allow | alarm | ignored
    reason           text NOT NULL DEFAULT '',
    security_event_id uuid,
    PRIMARY KEY (id, read_at),
    UNIQUE (reader_id, client_event_id, read_at)
) PARTITION BY RANGE (read_at);
CREATE TABLE rfid_events_default PARTITION OF rfid_events DEFAULT;
CREATE INDEX rfid_events_epc_idx ON rfid_events (company_id, epc, read_at DESC);
CREATE INDEX rfid_events_store_idx ON rfid_events (store_id, read_at DESC);

-- Everything the loss-prevention team needs to look at: RFID alarms and rule hits (voids, refunds, discounts, cash).
CREATE TABLE security_events (
    id              uuid PRIMARY KEY,
    company_id      uuid NOT NULL REFERENCES companies(id),
    store_id        uuid NOT NULL REFERENCES stores(id),
    reader_id       uuid REFERENCES rfid_readers(id),
    type            text NOT NULL CHECK (type IN ('UNAUTHORIZED_EXIT','UNKNOWN_TAG','RFID_MISMATCH','EXCESSIVE_VOIDS','LARGE_REFUND','HIGH_DISCOUNT','CASH_VARIANCE','READER_OFFLINE')),
    severity        text NOT NULL DEFAULT 'warning' CHECK (severity IN ('info','warning','critical')),
    title           text NOT NULL,
    epc             text NOT NULL DEFAULT '',
    variant_id      uuid,
    sale_id         uuid,
    user_id         uuid,
    detail          jsonb NOT NULL DEFAULT '{}'::jsonb,
    status          text NOT NULL DEFAULT 'open' CHECK (status IN ('open','acknowledged','resolved','false_alarm')),
    dedupe_key      text NOT NULL DEFAULT '',
    occurred_at     timestamptz NOT NULL DEFAULT now(),
    created_at      timestamptz NOT NULL DEFAULT now(),
    acknowledged_by uuid,
    acknowledged_at timestamptz,
    resolved_by     uuid,
    resolved_at     timestamptz,
    resolution_note text NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX security_events_dedupe_idx ON security_events (company_id, dedupe_key) WHERE dedupe_key <> '';
CREATE INDEX security_events_store_idx ON security_events (store_id, occurred_at DESC);
CREATE INDEX security_events_status_idx ON security_events (company_id, status, occurred_at DESC);

-- Which tags left in which sale line (so void/return can put exactly those tags back).
CREATE TABLE sale_item_tags (
    sale_item_id uuid NOT NULL REFERENCES sale_items(id) ON DELETE CASCADE,
    epc          text NOT NULL,
    returned_at  timestamptz,
    PRIMARY KEY (sale_item_id, epc)
);

-- Tags seen during an RFID stock count (accumulates over several scan batches; the same tag counts once).
CREATE TABLE stock_count_tags (
    count_id  uuid NOT NULL REFERENCES stock_counts(id) ON DELETE CASCADE,
    epc       text NOT NULL,
    variant_id uuid NOT NULL,
    PRIMARY KEY (count_id, epc)
);

-- +goose Down
DROP TABLE stock_count_tags;
DROP TABLE sale_item_tags;
DROP TABLE security_events;
DROP TABLE rfid_events;
DROP TABLE rfid_tags;
DROP TABLE rfid_readers;
