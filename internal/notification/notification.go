// Package notification raises in-app alerts. A notification is written once per recipient (chosen by permission and
// org scope when it is raised), so listing, unread counts and read-state are plain per-user queries.
package notification

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/ritel35k/backend/db/gen"
	"github.com/ritel35k/backend/internal/platform/eventbus"
	"github.com/ritel35k/backend/internal/platform/tx"
	"github.com/ritel35k/backend/internal/security"
	"github.com/ritel35k/backend/internal/settings"
	"github.com/ritel35k/backend/internal/stream"
)

const (
	Info     = "info"
	Warning  = "warning"
	Critical = "critical"
)

// Raise describes one alert. Recipients = active users holding Perm whose scope covers StoreID
// (StoreID nil → only users holding Perm company-wide).
type Raise struct {
	CompanyID  uuid.UUID
	StoreID    *uuid.UUID
	Perm       string
	Type       string
	Severity   string
	Title      string
	Body       string
	EntityType string
	EntityID   string
	Dedupe     string // same recipient + same key is delivered once
}

// Send writes the notification for every recipient and signals their open browser tabs. Returns the recipient count.
func Send(ctx context.Context, q *gen.Queries, r Raise) (int, error) {
	if r.Severity == "" {
		r.Severity = Info
	}
	users, err := q.NotificationRecipients(ctx, gen.NotificationRecipientsParams{CompanyID: r.CompanyID, Perm: r.Perm, StoreID: r.StoreID})
	if err != nil {
		return 0, err
	}
	for _, u := range users {
		id, _ := uuid.NewV7()
		if err := q.InsertNotification(ctx, gen.InsertNotificationParams{ID: id, CompanyID: r.CompanyID, UserID: u, StoreID: r.StoreID, Type: r.Type,
			Severity: r.Severity, Title: r.Title, Body: r.Body, EntityType: r.EntityType, EntityID: r.EntityID, DedupeKey: r.Dedupe}); err != nil {
			return 0, err
		}
		uid := u
		if err := stream.Notify(ctx, q, stream.Event{Kind: stream.KindNotification, CompanyID: r.CompanyID, UserID: &uid}); err != nil {
			return 0, err
		}
	}
	return len(users), nil
}

// IDR formats an amount as "Rp 1.234.567".
func IDR(d decimal.Decimal) string {
	n := d.Round(0)
	neg := n.IsNegative()
	s := n.Abs().String()
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte('.')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-Rp " + b.String()
	}
	return "Rp " + b.String()
}

// Register wires the event consumers.
func Register(bus *eventbus.Bus, t *tx.Manager) {
	bus.Subscribe("SaleVoided", "notification.void_alert", onVoid(t))
	bus.Subscribe("SaleRefunded", "notification.refund_alert", onRefund(t))
	bus.Subscribe("ShiftClosed", "notification.shift_variance", onShiftClosed(t))
	bus.Subscribe("StockTransferReceived", "notification.transfer_shortage", onTransferReceived(t))
	bus.Subscribe("SaleCompleted", "security.high_discount", onSaleCompleted(t))
}

type storeCtx struct {
	id   uuid.UUID
	name string
	snap settings.Snapshot
}

func loadStore(ctx context.Context, q *gen.Queries, companyID, storeID uuid.UUID) (*storeCtx, error) {
	st, err := q.GetStore(ctx, gen.GetStoreParams{ID: storeID, CompanyID: companyID})
	if err != nil {
		return nil, err
	}
	snap, err := settings.LoadSnapshot(ctx, q, companyID, st.OrgUnitID)
	if err != nil {
		return nil, err
	}
	return &storeCtx{id: st.ID, name: st.Name, snap: snap}, nil
}

func userName(ctx context.Context, q *gen.Queries, id uuid.UUID) string {
	u, err := q.GetUser(ctx, id)
	if err != nil {
		return "—"
	}
	return u.Name
}

// onVoid raises a fraud alert when one cashier's voids for the day reach pos.void_alert_per_day.
func onVoid(t *tx.Manager) eventbus.Handler {
	return func(ctx context.Context, e eventbus.Event) error {
		var p struct {
			StoreID   uuid.UUID `json:"store_id"`
			CashierID uuid.UUID `json:"cashier_id"`
		}
		if json.Unmarshal(e.Payload, &p) != nil {
			return nil
		}
		return t.Do(ctx, func(q *gen.Queries) error {
			sale, err := q.SaleBusinessDate(ctx, e.AggregateID)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			sc, err := loadStore(ctx, q, e.CompanyID, p.StoreID)
			if err != nil {
				return err
			}
			limit := sc.snap.Decimal("pos.void_alert_per_day").IntPart()
			if limit <= 0 {
				return nil
			}
			n, err := q.CountCashierVoids(ctx, gen.CountCashierVoidsParams{CashierID: p.CashierID, BusinessDate: sale.BusinessDate})
			if err != nil {
				return err
			}
			if n < limit {
				return nil
			}
			day := sale.BusinessDate.Format("2006-01-02")
			cashier := p.CashierID
			if _, _, err := security.Record(ctx, q, security.Event{CompanyID: e.CompanyID, StoreID: p.StoreID, Type: security.ExcessiveVoids, Severity: "critical", UserID: &cashier,
				SaleID: &e.AggregateID, Title: fmt.Sprintf("%d void oleh %s pada %s", n, userName(ctx, q, p.CashierID), day), Detail: map[string]any{"voids_today": n, "limit": limit, "business_date": day},
				Dedupe: fmt.Sprintf("voids:%s:%s:%d", p.CashierID, day, n)}); err != nil {
				return err
			}
			_, err = Send(ctx, q, Raise{CompanyID: e.CompanyID, StoreID: &p.StoreID, Perm: "report.sales.view", Type: "void_alert", Severity: Critical,
				Title:      fmt.Sprintf("%d transaksi dibatalkan oleh %s", n, userName(ctx, q, p.CashierID)),
				Body:       fmt.Sprintf("%s · %s. Batas peringatan: %d void per hari.", sc.name, day, limit),
				EntityType: "user", EntityID: p.CashierID.String(), Dedupe: fmt.Sprintf("voidalert:%s:%s:%d", p.CashierID, day, n)})
			return err
		})
	}
}

func onRefund(t *tx.Manager) eventbus.Handler {
	return func(ctx context.Context, e eventbus.Event) error {
		var p struct {
			StoreID  uuid.UUID       `json:"store_id"`
			Doc      string          `json:"document_number"`
			Amount   decimal.Decimal `json:"amount"`
			ReturnID uuid.UUID       `json:"return_id"`
		}
		if json.Unmarshal(e.Payload, &p) != nil {
			return nil
		}
		return t.Do(ctx, func(q *gen.Queries) error {
			sc, err := loadStore(ctx, q, e.CompanyID, p.StoreID)
			if err != nil {
				return err
			}
			limit := sc.snap.Decimal("notify.refund_amount")
			if limit.IsPositive() && p.Amount.LessThan(limit) {
				return nil
			}
			sid := e.AggregateID
			if _, _, err := security.Record(ctx, q, security.Event{CompanyID: e.CompanyID, StoreID: p.StoreID, Type: security.LargeRefund, Severity: "warning", SaleID: &sid,
				Title: "Retur besar " + IDR(p.Amount) + " · " + p.Doc, Detail: map[string]any{"amount": p.Amount.String(), "document_number": p.Doc, "threshold": limit.String()},
				Dedupe: "refund:" + p.ReturnID.String()}); err != nil {
				return err
			}
			_, err = Send(ctx, q, Raise{CompanyID: e.CompanyID, StoreID: &p.StoreID, Perm: "report.sales.view", Type: "large_refund", Severity: Warning,
				Title: "Retur besar " + IDR(p.Amount), Body: sc.name + " · " + p.Doc, EntityType: "sale", EntityID: e.AggregateID.String(), Dedupe: "refund:" + p.ReturnID.String()})
			return err
		})
	}
}

func onShiftClosed(t *tx.Manager) eventbus.Handler {
	return func(ctx context.Context, e eventbus.Event) error {
		var p struct {
			StoreID   uuid.UUID       `json:"store_id"`
			CashierID uuid.UUID       `json:"cashier_id"`
			Variance  decimal.Decimal `json:"variance"`
			Expected  decimal.Decimal `json:"expected"`
			Counted   decimal.Decimal `json:"counted"`
		}
		if json.Unmarshal(e.Payload, &p) != nil {
			return nil
		}
		return t.Do(ctx, func(q *gen.Queries) error {
			sc, err := loadStore(ctx, q, e.CompanyID, p.StoreID)
			if err != nil {
				return err
			}
			if p.Variance.Abs().LessThanOrEqual(sc.snap.Decimal("notify.shift_variance")) {
				return nil
			}
			sev := Warning
			if p.Variance.IsNegative() {
				sev = Critical // cash is missing
			}
			cashier := p.CashierID
			if _, _, err := security.Record(ctx, q, security.Event{CompanyID: e.CompanyID, StoreID: p.StoreID, Type: security.CashVariance, Severity: sev, UserID: &cashier,
				Title: "Selisih kas " + IDR(p.Variance) + " · " + userName(ctx, q, p.CashierID), Detail: map[string]any{"variance": p.Variance.String(), "expected": p.Expected.String(), "counted": p.Counted.String(), "shift_id": e.AggregateID},
				Dedupe: "shift:" + e.AggregateID.String()}); err != nil {
				return err
			}
			_, err = Send(ctx, q, Raise{CompanyID: e.CompanyID, StoreID: &p.StoreID, Perm: "report.sales.view", Type: "shift_variance", Severity: sev,
				Title:      "Selisih kas " + IDR(p.Variance) + " · " + userName(ctx, q, p.CashierID),
				Body:       fmt.Sprintf("%s · seharusnya %s, dihitung %s", sc.name, IDR(p.Expected), IDR(p.Counted)),
				EntityType: "shift", EntityID: e.AggregateID.String(), Dedupe: "shift:" + e.AggregateID.String()})
			return err
		})
	}
}

func onTransferReceived(t *tx.Manager) eventbus.Handler {
	return func(ctx context.Context, e eventbus.Event) error {
		var p struct {
			Doc      string          `json:"document_number"`
			Shortage decimal.Decimal `json:"shortage_value"`
			To       uuid.UUID       `json:"to"`
		}
		if json.Unmarshal(e.Payload, &p) != nil || !p.Shortage.IsPositive() {
			return nil
		}
		return t.Do(ctx, func(q *gen.Queries) error {
			tr, err := q.GetTransferStores(ctx, e.AggregateID)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			// the sending store's managers care most about a shortage; head office sees it through the same scope rule
			store := tr.FromStoreID
			if store == nil {
				store = tr.ToStoreID
			}
			_, err = Send(ctx, q, Raise{CompanyID: e.CompanyID, StoreID: store, Perm: "inventory.transfer.approve", Type: "transfer_shortage", Severity: Warning,
				Title: "Barang kurang saat diterima " + p.Doc, Body: "Nilai selisih " + IDR(p.Shortage), EntityType: "stock_transfer", EntityID: e.AggregateID.String(),
				Dedupe: "transfer:" + e.AggregateID.String()})
			return err
		})
	}
}

// onSaleCompleted flags a manual discount above security.discount_percent of the sale's subtotal.
func onSaleCompleted(t *tx.Manager) eventbus.Handler {
	return func(ctx context.Context, e eventbus.Event) error {
		return t.Do(ctx, func(q *gen.Queries) error {
			f, err := q.SaleDiscountFacts(ctx, e.AggregateID)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			if f.Status != "completed" || !f.Subtotal.IsPositive() {
				return nil
			}
			manual := f.DiscountTotal.Sub(f.PromoDiscount).Sub(f.VoucherDiscount) // what the cashier gave beyond promotions and vouchers
			if !manual.IsPositive() {
				return nil
			}
			sc, err := loadStore(ctx, q, e.CompanyID, f.StoreID)
			if err != nil {
				return err
			}
			limit := sc.snap.Decimal("security.discount_percent")
			pct := manual.Div(f.Subtotal).Mul(decimal.NewFromInt(100))
			if limit.IsNegative() || pct.LessThanOrEqual(limit) {
				return nil
			}
			cashier, sale := f.CashierID, e.AggregateID
			title := fmt.Sprintf("Diskon manual %s%% (%s) · %s", pct.Round(1), IDR(manual), f.DocumentNumber)
			id, created, err := security.Record(ctx, q, security.Event{CompanyID: e.CompanyID, StoreID: f.StoreID, Type: security.HighDiscount, Severity: "warning", UserID: &cashier, SaleID: &sale,
				Title: title, Detail: map[string]any{"percent": pct.Round(1).String(), "amount": manual.String(), "subtotal": f.Subtotal.String(), "limit": limit.String(), "document_number": f.DocumentNumber},
				Dedupe: "discount:" + sale.String()})
			if err != nil || !created {
				return err
			}
			_, err = Send(ctx, q, Raise{CompanyID: e.CompanyID, StoreID: &f.StoreID, Perm: security.PermView, Type: "high_discount", Severity: Warning, Title: title,
				Body: sc.name + " · " + userName(ctx, q, f.CashierID), EntityType: "security_event", EntityID: id.String(), Dedupe: "sec:" + id.String()})
			return err
		})
	}
}

// CheckLowStock raises one notification per store per day listing how many SKUs are at or below reorder point.
func CheckLowStock(ctx context.Context, t *tx.Manager, now time.Time) (int, error) {
	sent := 0
	err := t.Do(ctx, func(q *gen.Queries) error {
		rows, err := q.LowStockByStore(ctx)
		if err != nil {
			return err
		}
		for _, r := range rows {
			sid := r.StoreID
			n, err := Send(ctx, q, Raise{CompanyID: r.CompanyID, StoreID: &sid, Perm: "purchase_order.create", Type: "low_stock", Severity: Warning,
				Title: fmt.Sprintf("%d produk di bawah titik pesan ulang", r.Skus), Body: r.StoreName, EntityType: "store", EntityID: r.StoreID.String(),
				Dedupe: "lowstock:" + r.StoreID.String() + ":" + now.Format("2006-01-02")})
			if err != nil {
				return err
			}
			sent += n
		}
		return nil
	})
	return sent, err
}

// Purge removes notifications older than the retention window.
func Purge(ctx context.Context, t *tx.Manager, olderThan time.Time) (int64, error) {
	var n int64
	err := t.Do(ctx, func(q *gen.Queries) error {
		var err error
		n, err = q.PurgeOldNotifications(ctx, olderThan)
		return err
	})
	return n, err
}
