// Package purchasing implements purchase orders, goods receipts (GRN) and purchase returns.
package purchasing

import (
	"context"
	"errors"
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
	PermCreate  = "purchase_order.create"
	PermApprove = "purchase_order.approve"
	PermView    = "purchase_order.view"
	PermReceive = "goods_receipt.create"
	PermReturn  = "purchase_return.create"

	EventPOApproved     = "PurchaseOrderApproved"
	EventGoodsReceived  = "GoodsReceived"
	EventPurchaseReturn = "PurchaseReturned"
)

type Service struct {
	tx  *tx.Manager
	now func() time.Time
}

func NewService(t *tx.Manager) *Service { return &Service{tx: t, now: time.Now} }

type POLineInput struct {
	VariantID uuid.UUID        `json:"variant_id" validate:"required"`
	Qty       decimal.Decimal  `json:"qty"`
	UnitPrice *decimal.Decimal `json:"unit_price"` // default: supplier price, else the variant's purchase price
}

type POInput struct {
	SupplierID   uuid.UUID     `json:"supplier_id" validate:"required"`
	WarehouseID  uuid.UUID     `json:"warehouse_id" validate:"required"`
	ExpectedDate *time.Time    `json:"expected_date"`
	Note         string        `json:"note" validate:"max=300"`
	Lines        []POLineInput `json:"lines" validate:"required,min=1,max=500,dive"`
}

type POLine struct {
	gen.ListPOItemsRow
	LineTotal decimal.Decimal `json:"line_total"`
}

type PODetail struct {
	gen.GetPORow
	Lines []POLine `json:"lines"`
}

func (s *Service) loadPO(ctx context.Context, q *gen.Queries, p *access.Principal, id uuid.UUID) (*PODetail, error) {
	r, err := q.GetPO(ctx, gen.GetPOParams{ID: id, CompanyID: p.CompanyID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apperr.NotFound("purchase order")
	}
	if err != nil {
		return nil, err
	}
	if !viewable(p, r.Path, PermView, PermCreate, PermApprove, PermReceive) {
		return nil, apperr.NotFound("purchase order")
	}
	items, err := q.ListPOItems(ctx, id)
	if err != nil {
		return nil, err
	}
	d := &PODetail{GetPORow: r, Lines: make([]POLine, len(items))}
	for i, it := range items {
		d.Lines[i] = POLine{ListPOItemsRow: it, LineTotal: it.QtyOrdered.Mul(it.UnitPrice)}
	}
	return d, nil
}

// viewable: any of the given permissions applies at the document's path ("" = central).
func viewable(p *access.Principal, path string, perms ...string) bool {
	for _, perm := range perms {
		if inventory.CanAt(p, perm, path) {
			return true
		}
	}
	return false
}

func (s *Service) GetPO(ctx context.Context, id uuid.UUID) (*PODetail, error) {
	return s.loadPO(ctx, s.tx.Queries(), access.MustPrincipal(ctx), id)
}

type POFilter struct {
	StoreID    *uuid.UUID
	SupplierID *uuid.UUID
	Status, Q  string
}

func mergedScope(p *access.Principal, perms ...string) access.ScopeFilter {
	out := access.ScopeFilter{}
	for _, perm := range perms {
		g := p.Grants.Scope(perm)
		out.All = out.All || g.All
		out.Paths = append(out.Paths, g.Paths...)
	}
	return out
}

func (s *Service) ListPOs(ctx context.Context, f POFilter, pg pagination.Params) ([]gen.ListPOsRow, pagination.Meta, error) {
	p := access.MustPrincipal(ctx)
	all, paths := mergedScope(p, PermView, PermCreate, PermApprove, PermReceive).SQL()
	q := s.tx.Queries()
	rows, err := q.ListPOs(ctx, gen.ListPOsParams{CompanyID: p.CompanyID, AllScope: all, ScopePaths: paths, StoreID: f.StoreID, SupplierID: f.SupplierID, Status: f.Status, Q: f.Q,
		PageLimit: pg.Limit(), PageOffset: pg.Offset()})
	if err != nil {
		return nil, pagination.Meta{}, err
	}
	total, err := q.CountPOs(ctx, gen.CountPOsParams{CompanyID: p.CompanyID, AllScope: all, ScopePaths: paths, StoreID: f.StoreID, SupplierID: f.SupplierID, Status: f.Status, Q: f.Q})
	return rows, pagination.NewMeta(pg, total), err
}

// priceLines validates lines and fills default prices; returns the total.
func priceLines(ctx context.Context, q *gen.Queries, p *access.Principal, supplierID uuid.UUID, in []POLineInput) ([]POLineInput, decimal.Decimal, error) {
	out := make([]POLineInput, len(in))
	seen := map[uuid.UUID]bool{}
	total := decimal.Zero
	for i, l := range in {
		if seen[l.VariantID] {
			return nil, total, apperr.Validation(map[string]string{"lines": "each product may appear once per PO"})
		}
		seen[l.VariantID] = true
		if !l.Qty.IsPositive() {
			return nil, total, apperr.Validation(map[string]string{"lines": "qty must be positive"})
		}
		v, err := q.GetVariant(ctx, gen.GetVariantParams{ID: l.VariantID, CompanyID: p.CompanyID})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, total, apperr.Validation(map[string]string{"lines": "unknown variant"})
		}
		if err != nil {
			return nil, total, err
		}
		price := v.PurchasePrice
		if sp, err := q.SupplierPrice(ctx, gen.SupplierPriceParams{SupplierID: supplierID, VariantID: l.VariantID}); err == nil {
			price = sp
		}
		if l.UnitPrice != nil {
			price = *l.UnitPrice
		}
		if price.IsNegative() {
			return nil, total, apperr.Validation(map[string]string{"lines": "unit_price must not be negative"})
		}
		out[i] = POLineInput{VariantID: l.VariantID, Qty: l.Qty, UnitPrice: &price}
		total = total.Add(l.Qty.Mul(price))
	}
	return out, total.Round(2), nil
}

func insertPOItems(ctx context.Context, q *gen.Queries, poID uuid.UUID, lines []POLineInput) error {
	for i, l := range lines {
		iid, _ := uuid.NewV7()
		if _, err := q.InsertPOItem(ctx, gen.InsertPOItemParams{ID: iid, PoID: poID, LineNo: int32(i + 1), VariantID: l.VariantID, QtyOrdered: l.Qty, UnitPrice: *l.UnitPrice}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) CreatePO(ctx context.Context, in POInput) (*PODetail, error) {
	p := access.MustPrincipal(ctx)
	var id uuid.UUID
	err := s.tx.Do(ctx, func(q *gen.Queries) error {
		wh, _, err := inventory.WarehouseFor(ctx, q, p, in.WarehouseID, PermCreate)
		if err != nil {
			return err
		}
		sup, err := q.GetSupplier(ctx, gen.GetSupplierParams{ID: in.SupplierID, CompanyID: p.CompanyID})
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && sup.Status != "active") {
			return apperr.Validation(map[string]string{"supplier_id": "unknown or inactive supplier"})
		}
		if err != nil {
			return err
		}
		lines, total, err := priceLines(ctx, q, p, in.SupplierID, in.Lines)
		if err != nil {
			return err
		}
		doc, err := numbering.ForWarehouse(ctx, q, wh, "purchase_order", s.now())
		if err != nil {
			return err
		}
		id, _ = uuid.NewV7()
		if _, err := q.InsertPO(ctx, gen.InsertPOParams{ID: id, CompanyID: p.CompanyID, StoreID: wh.StoreID, WarehouseID: wh.ID, SupplierID: in.SupplierID,
			DocumentNumber: doc, ExpectedDate: in.ExpectedDate, Note: in.Note, Total: total, CreatedBy: p.UserID}); err != nil {
			return err
		}
		if err := insertPOItems(ctx, q, id, lines); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{StoreID: wh.StoreID, Action: "PO_CREATED", Entity: "purchase_order", EntityID: id.String(), New: map[string]any{"document_number": doc, "total": total.String()}})
	})
	if err != nil {
		return nil, err
	}
	return s.GetPO(ctx, id)
}

// UpdatePO replaces a draft (header + lines). Submitted POs are immutable.
func (s *Service) UpdatePO(ctx context.Context, id uuid.UUID, in POInput) (*PODetail, error) {
	p := access.MustPrincipal(ctx)
	err := s.tx.Do(ctx, func(q *gen.Queries) error {
		po, err := q.GetPOForUpdate(ctx, gen.GetPOForUpdateParams{ID: id, CompanyID: p.CompanyID})
		if errors.Is(err, pgx.ErrNoRows) {
			return apperr.NotFound("purchase order")
		}
		if err != nil {
			return err
		}
		if po.Status != "draft" {
			return apperr.Conflict("PO_NOT_EDITABLE", "Only draft purchase orders can be edited")
		}
		if _, _, err := inventory.WarehouseFor(ctx, q, p, po.WarehouseID, PermCreate); err != nil {
			return err
		}
		wh, _, err := inventory.WarehouseFor(ctx, q, p, in.WarehouseID, PermCreate)
		if err != nil {
			return err
		}
		if _, err := q.GetSupplier(ctx, gen.GetSupplierParams{ID: in.SupplierID, CompanyID: p.CompanyID}); err != nil {
			return apperr.Validation(map[string]string{"supplier_id": "unknown supplier"})
		}
		lines, total, err := priceLines(ctx, q, p, in.SupplierID, in.Lines)
		if err != nil {
			return err
		}
		if err := q.UpdatePODraft(ctx, gen.UpdatePODraftParams{ID: id, SupplierID: in.SupplierID, WarehouseID: wh.ID, StoreID: wh.StoreID, ExpectedDate: in.ExpectedDate, Note: in.Note, Total: total}); err != nil {
			return err
		}
		if err := q.DeletePOItems(ctx, id); err != nil {
			return err
		}
		return insertPOItems(ctx, q, id, lines)
	})
	if err != nil {
		return nil, err
	}
	return s.GetPO(ctx, id)
}

func (s *Service) transition(ctx context.Context, id uuid.UUID, fn func(q *gen.Queries, p *access.Principal, po gen.PurchaseOrder, path string) error) (*PODetail, error) {
	p := access.MustPrincipal(ctx)
	err := s.tx.Do(ctx, func(q *gen.Queries) error {
		po, err := q.GetPOForUpdate(ctx, gen.GetPOForUpdateParams{ID: id, CompanyID: p.CompanyID})
		if errors.Is(err, pgx.ErrNoRows) {
			return apperr.NotFound("purchase order")
		}
		if err != nil {
			return err
		}
		head, err := q.GetPO(ctx, gen.GetPOParams{ID: id, CompanyID: p.CompanyID})
		if err != nil {
			return err
		}
		if !viewable(p, head.Path, PermView, PermCreate, PermApprove, PermReceive) {
			return apperr.NotFound("purchase order")
		}
		return fn(q, p, po, head.Path)
	})
	if err != nil {
		return nil, err
	}
	return s.GetPO(ctx, id)
}

// settingsUnit is where a PO's settings are resolved: its store's unit, or the company root for central warehouses.
func settingsUnit(ctx context.Context, q *gen.Queries, p *access.Principal, storeID *uuid.UUID) (uuid.UUID, error) {
	if storeID != nil {
		st, err := q.GetStore(ctx, gen.GetStoreParams{ID: *storeID, CompanyID: p.CompanyID})
		return st.OrgUnitID, err
	}
	root, err := q.GetCompanyRootOrgUnit(ctx, p.CompanyID)
	return root.ID, err
}

// Submit sends a draft for approval. Small POs (total <= purchasing.po_approval_threshold) are approved on the spot.
func (s *Service) Submit(ctx context.Context, id uuid.UUID) (*PODetail, error) {
	return s.transition(ctx, id, func(q *gen.Queries, p *access.Principal, po gen.PurchaseOrder, path string) error {
		if !inventory.CanAt(p, PermCreate, path) {
			return apperr.Forbidden("not allowed")
		}
		if po.Status != "draft" {
			return apperr.Conflict("PO_BAD_STATE", "Only draft purchase orders can be submitted")
		}
		now := s.now()
		unit, err := settingsUnit(ctx, q, p, po.StoreID)
		if err != nil {
			return err
		}
		snap, err := settings.LoadSnapshot(ctx, q, p.CompanyID, unit)
		if err != nil {
			return err
		}
		if po.Total.LessThanOrEqual(snap.Decimal("purchasing.po_approval_threshold")) {
			if err := q.SetPOStatus(ctx, gen.SetPOStatusParams{ID: po.ID, Status: "approved", SubmittedAt: &now, ApprovedBy: &p.UserID, ApprovedAt: &now}); err != nil {
				return err
			}
			if err := audit.Record(ctx, q, audit.Entry{StoreID: po.StoreID, Action: "PO_AUTO_APPROVED", Entity: "purchase_order", EntityID: po.ID.String(), New: map[string]any{"total": po.Total.String()}}); err != nil {
				return err
			}
			return outbox.Publish(ctx, q, p.CompanyID, EventPOApproved, "purchase_order", po.ID, map[string]any{"document_number": po.DocumentNumber, "total": po.Total.String(), "auto": true})
		}
		if err := q.SetPOStatus(ctx, gen.SetPOStatusParams{ID: po.ID, Status: "submitted", SubmittedAt: &now}); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{StoreID: po.StoreID, Action: "PO_SUBMITTED", Entity: "purchase_order", EntityID: po.ID.String(), New: map[string]any{"total": po.Total.String()}})
	})
}

// Approve is for POs above the auto-approval threshold. The approver must differ from the creator.
func (s *Service) Approve(ctx context.Context, id uuid.UUID) (*PODetail, error) {
	return s.transition(ctx, id, func(q *gen.Queries, p *access.Principal, po gen.PurchaseOrder, path string) error {
		if !inventory.CanAt(p, PermApprove, path) {
			return apperr.Forbidden("not allowed to approve purchase orders here")
		}
		if po.Status != "submitted" {
			return apperr.Conflict("PO_BAD_STATE", "Only submitted purchase orders can be approved")
		}
		if po.CreatedBy == p.UserID {
			return apperr.Conflict("SELF_APPROVAL_FORBIDDEN", "A purchase order cannot be approved by its creator")
		}
		now := s.now()
		if err := q.SetPOStatus(ctx, gen.SetPOStatusParams{ID: po.ID, Status: "approved", ApprovedBy: &p.UserID, ApprovedAt: &now}); err != nil {
			return err
		}
		if err := audit.Record(ctx, q, audit.Entry{StoreID: po.StoreID, Action: "PO_APPROVED", Entity: "purchase_order", EntityID: po.ID.String(), New: map[string]any{"total": po.Total.String()}}); err != nil {
			return err
		}
		return outbox.Publish(ctx, q, p.CompanyID, EventPOApproved, "purchase_order", po.ID, map[string]any{"document_number": po.DocumentNumber, "total": po.Total.String(), "auto": false})
	})
}

// Cancel voids a PO that has not received anything yet.
func (s *Service) Cancel(ctx context.Context, id uuid.UUID, reason string) (*PODetail, error) {
	return s.transition(ctx, id, func(q *gen.Queries, p *access.Principal, po gen.PurchaseOrder, path string) error {
		if !inventory.CanAt(p, PermCreate, path) && !inventory.CanAt(p, PermApprove, path) {
			return apperr.Forbidden("not allowed")
		}
		if po.Status == "partially_received" || po.Status == "received" || po.Status == "cancelled" {
			return apperr.Conflict("PO_BAD_STATE", "This purchase order can no longer be cancelled")
		}
		if err := q.SetPOStatus(ctx, gen.SetPOStatusParams{ID: po.ID, Status: "cancelled", CancelReason: &reason}); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{StoreID: po.StoreID, Action: "PO_CANCELLED", Entity: "purchase_order", EntityID: po.ID.String(), New: reason})
	})
}

// CloseShort marks a partially received PO as complete (the supplier will not deliver the rest).
func (s *Service) CloseShort(ctx context.Context, id uuid.UUID) (*PODetail, error) {
	return s.transition(ctx, id, func(q *gen.Queries, p *access.Principal, po gen.PurchaseOrder, path string) error {
		if !inventory.CanAt(p, PermReceive, path) && !inventory.CanAt(p, PermApprove, path) {
			return apperr.Forbidden("not allowed")
		}
		if po.Status != "partially_received" {
			return apperr.Conflict("PO_BAD_STATE", "Only partially received purchase orders can be closed short")
		}
		if err := q.SetPOStatus(ctx, gen.SetPOStatusParams{ID: po.ID, Status: "received"}); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{StoreID: po.StoreID, Action: "PO_CLOSED_SHORT", Entity: "purchase_order", EntityID: po.ID.String()})
	})
}
