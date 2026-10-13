// Package outbox implements the transactional outbox: Publish inside the business tx, Relay in the worker.
package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"github.com/ritel35k/backend/db/gen"
	"github.com/ritel35k/backend/internal/platform/eventbus"
	"github.com/ritel35k/backend/internal/platform/tx"
)

const maxAttempts = 10

// Publish writes an event to the outbox using the caller's transaction.
func Publish(ctx context.Context, q *gen.Queries, companyID uuid.UUID, eventType, aggregateType string, aggregateID uuid.UUID, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal outbox payload: %w", err)
	}
	return q.InsertOutbox(ctx, gen.InsertOutboxParams{
		CompanyID: companyID, EventType: eventType, AggregateType: aggregateType, AggregateID: aggregateID, Payload: raw,
	})
}

type Relay struct {
	tx  *tx.Manager
	bus *eventbus.Bus
	log zerolog.Logger
}

func NewRelay(t *tx.Manager, bus *eventbus.Bus, log zerolog.Logger) *Relay {
	return &Relay{tx: t, bus: bus, log: log}
}

// RunOnce claims a batch (SKIP LOCKED, safe with many workers) and dispatches it. Returns events handled.
func (r *Relay) RunOnce(ctx context.Context, batch int32) (int, error) {
	n := 0
	err := r.tx.Do(ctx, func(q *gen.Queries) error {
		rows, err := q.ClaimOutbox(ctx, gen.ClaimOutboxParams{MaxAttempts: maxAttempts, Batch: batch})
		if err != nil {
			return err
		}
		for _, row := range rows {
			ev := eventbus.Event{
				ID: row.ID, CompanyID: row.CompanyID, Type: row.EventType, AggregateType: row.AggregateType,
				AggregateID: row.AggregateID, Payload: row.Payload, CreatedAt: row.CreatedAt,
			}
			if herr := r.dispatch(ctx, q, ev); herr != nil {
				r.log.Error().Err(herr).Int64("event_id", row.ID).Str("type", row.EventType).Msg("outbox handler failed")
				if err := q.MarkOutboxFailed(ctx, gen.MarkOutboxFailedParams{ID: row.ID, LastError: ptr(herr.Error())}); err != nil {
					return err
				}
				continue
			}
			if err := q.MarkOutboxPublished(ctx, row.ID); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	return n, err
}

func (r *Relay) dispatch(ctx context.Context, q *gen.Queries, ev eventbus.Event) error {
	for _, d := range r.bus.For(ev.Type) {
		first, err := q.MarkEventProcessed(ctx, gen.MarkEventProcessedParams{Consumer: d.Consumer, EventID: ev.ID})
		if err != nil {
			return err
		}
		if first == 0 {
			continue // already processed by this consumer
		}
		if err := d.Fn(ctx, ev); err != nil {
			// un-mark so the retry runs this consumer again
			if uerr := q.UnmarkEventProcessed(ctx, gen.UnmarkEventProcessedParams{Consumer: d.Consumer, EventID: ev.ID}); uerr != nil {
				return uerr
			}
			return fmt.Errorf("%s: %w", d.Consumer, err)
		}
	}
	return nil
}

// Run polls until ctx is cancelled.
func (r *Relay) Run(ctx context.Context, poll time.Duration) {
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		for {
			n, err := r.RunOnce(ctx, 100)
			if err != nil {
				if ctx.Err() == nil {
					r.log.Error().Err(err).Msg("outbox relay")
				}
				break
			}
			if n == 0 {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func ptr[T any](v T) *T { return &v }
