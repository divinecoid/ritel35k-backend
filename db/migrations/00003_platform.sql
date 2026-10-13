-- +goose Up

-- Transactional outbox: written in the same tx as the state change, relayed by cmd/worker.
CREATE TABLE outbox (
    id              bigserial PRIMARY KEY,
    company_id      uuid NOT NULL,
    event_type      text NOT NULL,
    aggregate_type  text NOT NULL,
    aggregate_id    uuid NOT NULL,
    payload         jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at      timestamptz NOT NULL DEFAULT now(),
    published_at    timestamptz,
    attempts        integer NOT NULL DEFAULT 0,
    last_error      text
);
CREATE INDEX outbox_unpublished_idx ON outbox (id) WHERE published_at IS NULL;

-- Idempotency for event consumers.
CREATE TABLE processed_events (
    consumer      text NOT NULL,
    event_id      bigint NOT NULL,
    processed_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (consumer, event_id)
);

-- Race-free document numbering (never COUNT(*)+1).
CREATE TABLE document_sequences (
    company_id  uuid NOT NULL,
    store_id    uuid NOT NULL,
    doc_type    text NOT NULL,
    period      text NOT NULL,          -- e.g. 202610, or 'ALL' for non-periodic formats
    last_no     bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (store_id, doc_type, period)
);

-- Append-only audit log, partitioned by month. Partitions are created by the worker (EnsurePartitions).
CREATE TABLE audit_logs (
    id          uuid NOT NULL,
    company_id  uuid NOT NULL,
    store_id    uuid,
    user_id     uuid,
    action      text NOT NULL,
    entity      text NOT NULL,
    entity_id   text NOT NULL DEFAULT '',
    old_value   jsonb,
    new_value   jsonb,
    ip_address  text NOT NULL DEFAULT '',
    user_agent  text NOT NULL DEFAULT '',
    device_id   text NOT NULL DEFAULT '',
    request_id  text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (id, created_at)
) PARTITION BY RANGE (created_at);
CREATE TABLE audit_logs_default PARTITION OF audit_logs DEFAULT;
CREATE INDEX audit_logs_company_time_idx ON audit_logs (company_id, created_at DESC);
CREATE INDEX audit_logs_entity_idx ON audit_logs (entity, entity_id);

-- +goose StatementBegin
CREATE FUNCTION audit_logs_immutable() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'audit_logs is append-only';
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER audit_logs_no_update BEFORE UPDATE OR DELETE ON audit_logs
    FOR EACH ROW EXECUTE FUNCTION audit_logs_immutable();

-- +goose Down
DROP TABLE audit_logs;
DROP FUNCTION audit_logs_immutable();
DROP TABLE document_sequences;
DROP TABLE processed_events;
DROP TABLE outbox;
