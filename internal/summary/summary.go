// Package summary maintains the reporting read model (store_kpi_daily, sales_hourly, sales_by_product_daily,
// payment_method_daily). Sale/void/refund events only mark a (store, business_date) slice dirty; the worker then
// rebuilds that slice from the source tables. Rebuilding is idempotent, coalesces bursts, and doubles as the repair
// path, so the reconcile job just marks drifting slices dirty again.
package summary

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ritel35k/backend/db/gen"
	"github.com/ritel35k/backend/internal/platform/eventbus"
	"github.com/ritel35k/backend/internal/platform/tx"
	"github.com/ritel35k/backend/internal/stream"
)

// Event names (kept here so reporting does not import the POS package).
const (
	eventSaleCompleted = "SaleCompleted"
	eventSaleVoided    = "SaleVoided"
	eventSaleRefunded  = "SaleRefunded"
)

// Register subscribes the dirty-marking consumers. Consumers are idempotent (marking twice is the same as once).
func Register(bus *eventbus.Bus, t *tx.Manager) {
	bus.Subscribe(eventSaleCompleted, "summary.mark_sale", markSale(t))
	bus.Subscribe(eventSaleVoided, "summary.mark_sale", markSale(t))
	bus.Subscribe(eventSaleRefunded, "summary.mark_return", markReturn(t))
}

func markSale(t *tx.Manager) eventbus.Handler {
	return func(ctx context.Context, e eventbus.Event) error {
		return t.Do(ctx, func(q *gen.Queries) error {
			r, err := q.SaleBusinessDate(ctx, e.AggregateID)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			return q.MarkSummaryDirty(ctx, gen.MarkSummaryDirtyParams{StoreID: r.StoreID, BusinessDate: r.BusinessDate, CompanyID: r.CompanyID})
		})
	}
}

func markReturn(t *tx.Manager) eventbus.Handler {
	return func(ctx context.Context, e eventbus.Event) error {
		var p struct {
			ReturnID uuid.UUID `json:"return_id"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return nil // malformed payload can never succeed; do not wedge the relay
		}
		return t.Do(ctx, func(q *gen.Queries) error {
			r, err := q.ReturnBusinessDate(ctx, p.ReturnID)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			return q.MarkSummaryDirty(ctx, gen.MarkSummaryDirtyParams{StoreID: r.StoreID, BusinessDate: r.BusinessDate, CompanyID: r.CompanyID})
		})
	}
}

// rebuildSlice replaces every summary row of one store/day with a fresh aggregate of the source tables.
func rebuildSlice(ctx context.Context, q *gen.Queries, storeID uuid.UUID, day time.Time) error {
	if err := q.DeleteKpiSlice(ctx, gen.DeleteKpiSliceParams{StoreID: storeID, BusinessDate: day}); err != nil {
		return err
	}
	if err := q.DeleteHourlySlice(ctx, gen.DeleteHourlySliceParams{StoreID: storeID, BusinessDate: day}); err != nil {
		return err
	}
	if err := q.DeleteProductSlice(ctx, gen.DeleteProductSliceParams{StoreID: storeID, BusinessDate: day}); err != nil {
		return err
	}
	if err := q.DeletePaymentSlice(ctx, gen.DeletePaymentSliceParams{StoreID: storeID, BusinessDate: day}); err != nil {
		return err
	}
	if err := q.InsertKpiSlice(ctx, gen.InsertKpiSliceParams{StoreID: storeID, Day: day}); err != nil {
		return err
	}
	if err := q.InsertHourlySlice(ctx, gen.InsertHourlySliceParams{StoreID: storeID, Day: day}); err != nil {
		return err
	}
	if err := q.InsertProductSlice(ctx, gen.InsertProductSliceParams{StoreID: storeID, Day: day}); err != nil {
		return err
	}
	return q.InsertPaymentSlice(ctx, gen.InsertPaymentSliceParams{StoreID: storeID, Day: day})
}

// RebuildDirty claims up to batch dirty slices and rebuilds them in one transaction (a failure leaves them dirty).
// Returns the number of slices rebuilt; loop until it returns 0 to drain the queue.
func RebuildDirty(ctx context.Context, t *tx.Manager, batch int32) (int, error) {
	n := 0
	err := t.Do(ctx, func(q *gen.Queries) error {
		rows, err := q.ClaimSummaryDirty(ctx, batch)
		if err != nil {
			return err
		}
		companies := map[uuid.UUID]bool{}
		for _, r := range rows {
			if err := rebuildSlice(ctx, q, r.StoreID, r.BusinessDate); err != nil {
				return err
			}
			companies[r.CompanyID] = true
		}
		for c := range companies { // delivered to SSE listeners when this tx commits
			if err := stream.Notify(ctx, q, stream.Event{Kind: stream.KindDashboard, CompanyID: c}); err != nil {
				return err
			}
		}
		n = len(rows)
		return nil
	})
	return n, err
}

// Drain rebuilds until nothing is dirty. Used by tests and the one-shot CLI; the worker calls RebuildDirty on a ticker.
func Drain(ctx context.Context, t *tx.Manager) (int, error) {
	total := 0
	for {
		n, err := RebuildDirty(ctx, t, 100)
		if err != nil || n == 0 {
			return total, err
		}
		total += n
	}
}

// Reconcile compares the last `days` days of summaries with the source and re-marks every slice that disagrees.
// Returns how many slices drifted. Safe to run any time and repeatedly.
func Reconcile(ctx context.Context, t *tx.Manager, days int, now time.Time) (int, error) {
	since := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -days)
	drifted := 0
	err := t.Do(ctx, func(q *gen.Queries) error {
		type key struct {
			store uuid.UUID
			day   time.Time
		}
		seen := map[key]bool{}
		mark := func(store uuid.UUID, day time.Time) error {
			k := key{store, day}
			if seen[k] {
				return nil
			}
			seen[k] = true
			st, err := q.GetStoreCompany(ctx, store)
			if err != nil {
				return err
			}
			return q.MarkSummaryDirty(ctx, gen.MarkSummaryDirtyParams{StoreID: store, BusinessDate: day, CompanyID: st})
		}
		a, err := q.SummaryDrift(ctx, since)
		if err != nil {
			return err
		}
		for _, r := range a {
			if err := mark(r.StoreID, r.BusinessDate); err != nil {
				return err
			}
		}
		b, err := q.SummaryReturnDrift(ctx, since)
		if err != nil {
			return err
		}
		for _, r := range b {
			if err := mark(r.StoreID, r.BusinessDate); err != nil {
				return err
			}
		}
		drifted = len(seen)
		return nil
	})
	return drifted, err
}
