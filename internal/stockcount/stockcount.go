// Package stockcount implements stock opname: snapshot → count → variance → approval → adjustment.
//
// Sales keep flowing while shelves are counted, so a naive "system qty − counted qty" would flag every
// item sold during the count as missing. Each counted line therefore records WHEN it was counted, and its
// expected quantity is  snapshot qty + all ledger movements between the snapshot and that moment.
// The approved adjustment is relative to the live balance, so nothing sold after the count is lost.
package stockcount

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/ritel35k/backend/db/gen"
	"github.com/ritel35k/backend/internal/access"
	"github.com/ritel35k/backend/internal/audit"
	"github.com/ritel35k/backend/internal/inventory"
	"github.com/ritel35k/backend/internal/numbering"
	"github.com/ritel35k/backend/internal/platform/outbox"
	"github.com/ritel35k/backend/internal/platform/tx"
	"github.com/ritel35k/backend/internal/settings"
	"github.com/ritel35k/backend/pkg/apperr"
	"github.com/ritel35k/backend/pkg/pagination"
)

const (
	PermCount   = "inventory.count"
	PermApprove = "inventory.count.approve"
	PermView    = "inventory.view"

	EventApproved = "StockCountApproved"
)

type Service struct {
	tx  *tx.Manager
	now func() time.Time
}

func NewService(t *tx.Manager) *Service { return &Service{tx: t, now: time.Now} }

type CreateInput struct {
	WarehouseID uuid.UUID   `json:"warehouse_id" validate:"required"`
	Kind        string      `json:"kind" validate:"required,oneof=full partial category cycle"`
	CategoryID  *uuid.UUID  `json:"category_id"`
	VariantIDs  []uuid.UUID `json:"variant_ids"`
	Note        string      `json:"note" validate:"max=300"`
}

type Line struct {
	gen.ListCountItemsRow
	Value decimal.Decimal `json:"variance_value"`
}

type Summary struct {
	Items         int             `json:"items"`
	Counted       int             `json:"counted"`
	Surplus       decimal.Decimal `json:"surplus_qty"`
	Shortage      decimal.Decimal `json:"shortage_qty"`
	VarianceValue decimal.Decimal `json:"variance_value"` // net, at moving-average cost
	AbsValue      decimal.Decimal `json:"abs_variance_value"`
}

type Detail struct {
	gen.GetCountRow
	Lines   []Line  `json:"lines"`
	Summary Summary `json:"summary"`
}

func (s *Service) load(ctx context.Context, q *gen.Queries, p *access.Principal, id uuid.UUID) (*Detail, error) {
	r, err := q.GetCount(ctx, gen.GetCountParams{ID: id, CompanyID: p.CompanyID})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !inventory.CanAt(p, PermView, r.Path) && !inventory.CanAt(p, PermCount, r.Path)) {
		return nil, apperr.NotFound("stock count")
	}
	if err != nil {
		return nil, err
	}
	items, err := q.ListCountItems(ctx, id)
	if err != nil {
		return nil, err
	}
	d := &Detail{GetCountRow: r, Lines: make([]Line, len(items))}
	d.Summary.Items = len(items)
	for i, it := range items {
		v := decimal.Zero
		if it.CountedQty != nil {
			d.Summary.Counted++
		}
		if it.Variance != nil {
			v = it.Variance.Mul(it.UnitCost)
			d.Summary.VarianceValue = d.Summary.VarianceValue.Add(v)
			d.Summary.AbsValue = d.Summary.AbsValue.Add(v.Abs())
			if it.Variance.IsPositive() {
				d.Summary.Surplus = d.Summary.Surplus.Add(*it.Variance)
			} else {
				d.Summary.Shortage = d.Summary.Shortage.Add(it.Variance.Neg())
			}
		}
		d.Lines[i] = Line{ListCountItemsRow: it, Value: v}
	}
	return d, nil
}

func (s *Service) Get(ctx context.Context, id uuid.UUID) (*Detail, error) {
	return s.load(ctx, s.tx.Queries(), access.MustPrincipal(ctx), id)
}

func (s *Service) List(ctx context.Context, storeID *uuid.UUID, status string, pg pagination.Params) ([]gen.ListCountsRow, error) {
	p := access.MustPrincipal(ctx)
	sc := access.ScopeFilter{}
	for _, perm := range []string{PermCount, PermView, PermApprove} {
		g := p.Grants.Scope(perm)
		sc.All = sc.All || g.All
		sc.Paths = append(sc.Paths, g.Paths...)
	}
	all, paths := sc.SQL()
	return s.tx.Queries().ListCounts(ctx, gen.ListCountsParams{CompanyID: p.CompanyID, AllScope: all, ScopePaths: paths, StoreID: storeID, Status: status, PageLimit: pg.Limit(), PageOffset: pg.Offset()})
}

// Create freezes a snapshot of system quantities for the items in scope and opens the count.
func (s *Service) Create(ctx context.Context, in CreateInput) (*Detail, error) {
	p := access.MustPrincipal(ctx)
	switch in.Kind {
	case "category":
		if in.CategoryID == nil {
			return nil, apperr.Validation(map[string]string{"category_id": "required for a category count"})
		}
	case "partial", "cycle":
		if len(in.VariantIDs) == 0 {
			return nil, apperr.Validation(map[string]string{"variant_ids": "required for a partial / cycle count"})
		}
	}
	var id uuid.UUID
	err := s.tx.Do(ctx, func(q *gen.Queries) error {
		wh, _, err := inventory.WarehouseFor(ctx, q, p, in.WarehouseID, PermCount)
		if err != nil {
			return err
		}
		doc, err := numbering.ForWarehouse(ctx, q, wh, "stock_count", s.now())
		if err != nil {
			return err
		}
		id, _ = uuid.NewV7()
		if _, err := q.InsertCount(ctx, gen.InsertCountParams{ID: id, CompanyID: p.CompanyID, StoreID: wh.StoreID, WarehouseID: wh.ID, DocumentNumber: doc, Kind: in.Kind, Note: in.Note, CreatedBy: p.UserID}); err != nil {
			return err
		}
		ids := in.VariantIDs
		if ids == nil {
			ids = []uuid.UUID{}
		}
		cat := in.CategoryID
		if in.Kind != "category" {
			cat = nil
		}
		n, err := q.SnapshotCountItems(ctx, gen.SnapshotCountItemsParams{CountID: id, WarehouseID: wh.ID, CategoryID: cat, VariantIds: ids})
		if err != nil {
			return err
		}
		// partial/cycle counts may include items that have no balance row yet (system qty 0)
		if in.Kind == "partial" || in.Kind == "cycle" {
			for _, vid := range ids {
				v, err := q.GetVariant(ctx, gen.GetVariantParams{ID: vid, CompanyID: p.CompanyID})
				if err != nil {
					return apperr.Validation(map[string]string{"variant_ids": "unknown variant"})
				}
				iid, _ := uuid.NewV7()
				if err := q.InsertCountItemIfMissing(ctx, gen.InsertCountItemIfMissingParams{ID: iid, CountID: id, VariantID: vid, SystemQty: decimal.Zero, UnitCost: v.Cost}); err != nil {
					return err
				}
			}
			n = int64(len(ids))
		}
		if n == 0 {
			return apperr.Conflict("NOTHING_TO_COUNT", "No stock lines match this count")
		}
		return audit.Record(ctx, q, audit.Entry{StoreID: wh.StoreID, Action: "STOCK_COUNT_STARTED", Entity: "stock_count", EntityID: id.String(), New: map[string]any{"document_number": doc, "kind": in.Kind, "items": n}})
	})
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

type CountEntry struct {
	VariantID  uuid.UUID       `json:"variant_id" validate:"required"`
	CountedQty decimal.Decimal `json:"counted_qty"`
}

func (s *Service) open(ctx context.Context, q *gen.Queries, p *access.Principal, id uuid.UUID) (gen.StockCount, gen.GetCountRow, error) {
	c, err := q.GetCountForUpdate(ctx, gen.GetCountForUpdateParams{ID: id, CompanyID: p.CompanyID})
	if errors.Is(err, pgx.ErrNoRows) {
		return c, gen.GetCountRow{}, apperr.NotFound("stock count")
	}
	if err != nil {
		return c, gen.GetCountRow{}, err
	}
	head, err := q.GetCount(ctx, gen.GetCountParams{ID: id, CompanyID: p.CompanyID})
	if err != nil {
		return c, head, err
	}
	if !inventory.CanAt(p, PermCount, head.Path) {
		return c, head, apperr.NotFound("stock count")
	}
	if c.Status != "counting" {
		return c, head, apperr.Conflict("COUNT_NOT_OPEN", "This count is no longer open for counting (status: "+c.Status+")")
	}
	return c, head, nil
}

// SetCounts records counted quantities (overwrites). Each line stamps its own counted_at.
func (s *Service) SetCounts(ctx context.Context, id uuid.UUID, entries []CountEntry) (*Detail, error) {
	p := access.MustPrincipal(ctx)
	err := s.tx.Do(ctx, func(q *gen.Queries) error {
		if _, _, err := s.open(ctx, q, p, id); err != nil {
			return err
		}
		for _, e := range entries {
			if e.CountedQty.IsNegative() {
				return apperr.Validation(map[string]string{"counted_qty": "must not be negative"})
			}
			n, err := q.SetCountedQty(ctx, gen.SetCountedQtyParams{CountID: id, VariantID: e.VariantID, CountedQty: &e.CountedQty})
			if err != nil {
				return err
			}
			if n == 0 {
				return apperr.Validation(map[string]string{"variant_id": "item is not part of this count"})
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

// Scan adds `qty` (default 1) to the counted quantity of the item with this barcode — the fast path for
// barcode counting. In a full count an unknown-to-system item is allowed (system qty 0 = a surplus).
func (s *Service) Scan(ctx context.Context, id uuid.UUID, barcode string, qty decimal.Decimal) (map[string]any, error) {
	p := access.MustPrincipal(ctx)
	if !qty.IsPositive() {
		qty = decimal.NewFromInt(1)
	}
	var out map[string]any
	err := s.tx.Do(ctx, func(q *gen.Queries) error {
		c, _, err := s.open(ctx, q, p, id)
		if err != nil {
			return err
		}
		b, err := q.LookupBarcode(ctx, gen.LookupBarcodeParams{CompanyID: p.CompanyID, Barcode: strings.TrimSpace(barcode)})
		if errors.Is(err, pgx.ErrNoRows) {
			return apperr.New(404, "UNKNOWN_BARCODE", "Barcode not found: "+barcode)
		}
		if err != nil {
			return err
		}
		total, err := q.AddCountedQty(ctx, gen.AddCountedQtyParams{CountID: id, VariantID: b.VariantID, CountedQty: &qty})
		if errors.Is(err, pgx.ErrNoRows) {
			if c.Kind != "full" {
				return apperr.Conflict("NOT_IN_COUNT", b.ProductName+" is not part of this count")
			}
			v, err := q.GetVariant(ctx, gen.GetVariantParams{ID: b.VariantID, CompanyID: p.CompanyID})
			if err != nil {
				return err
			}
			iid, _ := uuid.NewV7()
			if err := q.InsertCountItemIfMissing(ctx, gen.InsertCountItemIfMissingParams{ID: iid, CountID: id, VariantID: b.VariantID, SystemQty: decimal.Zero, UnitCost: v.Cost}); err != nil {
				return err
			}
			total, err = q.AddCountedQty(ctx, gen.AddCountedQtyParams{CountID: id, VariantID: b.VariantID, CountedQty: &qty})
			if err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		out = map[string]any{"variant_id": b.VariantID, "sku": b.Sku, "name": b.ProductName, "counted_qty": total}
		return nil
	})
	return out, err
}

// Submit computes expected quantities and variances. Items nobody counted must be dealt with explicitly:
// `uncounted = "zero"` treats them as zero on hand (typical for a full count), `"ignore"` leaves them alone.
func (s *Service) Submit(ctx context.Context, id uuid.UUID, uncounted string) (*Detail, error) {
	p := access.MustPrincipal(ctx)
	err := s.tx.Do(ctx, func(q *gen.Queries) error {
		c, _, err := s.open(ctx, q, p, id)
		if err != nil {
			return err
		}
		items, err := q.ListCountItems(ctx, id)
		if err != nil {
			return err
		}
		missing := 0
		for _, it := range items {
			if it.CountedQty == nil {
				missing++
			}
		}
		if missing > 0 && uncounted != "zero" && uncounted != "ignore" {
			return apperr.Conflict("UNCOUNTED_ITEMS", "Some items were not counted").WithDetails(map[string]any{"uncounted": missing, "choose": []string{"zero", "ignore"}})
		}
		now := s.now()
		for _, it := range items {
			countedAt := now
			counted := it.CountedQty
			if counted == nil {
				if uncounted == "ignore" {
					continue
				}
				z := decimal.Zero
				counted = &z
			} else if it.CountedAt != nil {
				countedAt = *it.CountedAt
			}
			mv, err := q.MovementsSince(ctx, gen.MovementsSinceParams{WarehouseID: c.WarehouseID, VariantID: it.VariantID, CreatedAt: c.SnapshotAt, CreatedAt_2: countedAt})
			if err != nil {
				return err
			}
			expected := it.SystemQty.Add(mv)
			if err := q.SetCountVariance(ctx, gen.SetCountVarianceParams{ID: it.ID, ExpectedQty: &expected, Variance: ptr(counted.Sub(expected))}); err != nil {
				return err
			}
			if it.CountedQty == nil { // persist the implied zero so the document is self-explanatory
				if _, err := q.SetCountedQty(ctx, gen.SetCountedQtyParams{CountID: id, VariantID: it.VariantID, CountedQty: counted}); err != nil {
					return err
				}
			}
		}
		return q.SetCountStatus(ctx, gen.SetCountStatusParams{ID: id, Status: "submitted", SubmittedBy: &p.UserID, SubmittedAt: &now})
	})
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

// Approve turns variances into OPNAME ledger movements (relative to the live balance). When the variance
// is large (see inventory.count_self_approve_limit) the approver must not be the person who submitted it.
func (s *Service) Approve(ctx context.Context, id uuid.UUID) (*Detail, error) {
	p := access.MustPrincipal(ctx)
	err := s.tx.Do(ctx, func(q *gen.Queries) error {
		c, err := q.GetCountForUpdate(ctx, gen.GetCountForUpdateParams{ID: id, CompanyID: p.CompanyID})
		if errors.Is(err, pgx.ErrNoRows) {
			return apperr.NotFound("stock count")
		}
		if err != nil {
			return err
		}
		head, err := q.GetCount(ctx, gen.GetCountParams{ID: id, CompanyID: p.CompanyID})
		if err != nil {
			return err
		}
		if !inventory.CanAt(p, PermApprove, head.Path) {
			return apperr.Forbidden("not allowed to approve stock counts here")
		}
		if c.Status != "submitted" {
			return apperr.Conflict("COUNT_BAD_STATE", "Only submitted counts can be approved")
		}
		d, err := s.load(ctx, q, p, id)
		if err != nil {
			return err
		}
		unit, err := settingsUnit(ctx, q, p, c.StoreID)
		if err != nil {
			return err
		}
		snap, err := settings.LoadSnapshot(ctx, q, p.CompanyID, unit)
		if err != nil {
			return err
		}
		if d.Summary.AbsValue.GreaterThan(snap.Decimal("inventory.count_self_approve_limit")) && c.SubmittedBy != nil && *c.SubmittedBy == p.UserID {
			return apperr.Conflict("SELF_APPROVAL_FORBIDDEN", "Large variances must be approved by someone other than the person who submitted the count")
		}
		for _, it := range d.Lines {
			if it.Variance == nil || it.Variance.IsZero() {
				continue
			}
			if _, err := inventory.Apply(ctx, q, inventory.Movement{CompanyID: p.CompanyID, StoreID: c.StoreID, WarehouseID: c.WarehouseID, VariantID: it.VariantID, Type: inventory.Opname,
				Qty: *it.Variance, UnitCost: it.UnitCost, RefType: "stock_count", RefID: &id, DocumentNo: c.DocumentNumber, Note: "stock opname", By: &p.UserID, AllowNegative: true}); err != nil {
				return err
			}
		}
		now := s.now()
		if err := q.SetCountStatus(ctx, gen.SetCountStatusParams{ID: id, Status: "approved", ApprovedBy: &p.UserID, ApprovedAt: &now}); err != nil {
			return err
		}
		if err := audit.Record(ctx, q, audit.Entry{StoreID: c.StoreID, Action: "STOCK_COUNT_APPROVED", Entity: "stock_count", EntityID: id.String(),
			New: map[string]any{"document_number": c.DocumentNumber, "net_value": d.Summary.VarianceValue.Round(2).String(), "shortage_qty": d.Summary.Shortage.String(), "surplus_qty": d.Summary.Surplus.String()}}); err != nil {
			return err
		}
		return outbox.Publish(ctx, q, p.CompanyID, EventApproved, "stock_count", id, map[string]any{"document_number": c.DocumentNumber, "store_id": c.StoreID,
			"net_value": d.Summary.VarianceValue.Round(2).String(), "abs_value": d.Summary.AbsValue.Round(2).String()})
	})
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

func (s *Service) Cancel(ctx context.Context, id uuid.UUID) (*Detail, error) {
	p := access.MustPrincipal(ctx)
	err := s.tx.Do(ctx, func(q *gen.Queries) error {
		c, err := q.GetCountForUpdate(ctx, gen.GetCountForUpdateParams{ID: id, CompanyID: p.CompanyID})
		if errors.Is(err, pgx.ErrNoRows) {
			return apperr.NotFound("stock count")
		}
		if err != nil {
			return err
		}
		head, _ := q.GetCount(ctx, gen.GetCountParams{ID: id, CompanyID: p.CompanyID})
		if !inventory.CanAt(p, PermCount, head.Path) && !inventory.CanAt(p, PermApprove, head.Path) {
			return apperr.NotFound("stock count")
		}
		if c.Status == "approved" || c.Status == "cancelled" {
			return apperr.Conflict("COUNT_BAD_STATE", "This count can no longer be cancelled")
		}
		if err := q.SetCountStatus(ctx, gen.SetCountStatusParams{ID: id, Status: "cancelled"}); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{StoreID: c.StoreID, Action: "STOCK_COUNT_CANCELLED", Entity: "stock_count", EntityID: id.String()})
	})
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

func settingsUnit(ctx context.Context, q *gen.Queries, p *access.Principal, storeID *uuid.UUID) (uuid.UUID, error) {
	if storeID != nil {
		st, err := q.GetStore(ctx, gen.GetStoreParams{ID: *storeID, CompanyID: p.CompanyID})
		return st.OrgUnitID, err
	}
	root, err := q.GetCompanyRootOrgUnit(ctx, p.CompanyID)
	return root.ID, err
}

func ptr[T any](v T) *T { return &v }
