// Package approval is the manager's inbox: one list of everything waiting for a decision, assembled from the
// documents that already carry their own approval state (purchase orders, stock transfers, stock counts).
// Each type is visible only where the caller holds that type's approve permission.
package approval

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/ritel35k/backend/db/gen"
	"github.com/ritel35k/backend/internal/access"
	"github.com/ritel35k/backend/internal/httpx"
	"github.com/ritel35k/backend/internal/httpx/middleware"
	"github.com/ritel35k/backend/internal/httpx/response"
	"github.com/ritel35k/backend/internal/platform/tx"
)

const (
	PermPO       = "purchase_order.approve"
	PermTransfer = "inventory.transfer.approve"
	PermCount    = "inventory.count.approve"
)

type Item struct {
	Type           string           `json:"type"` // purchase_order | stock_transfer | stock_count
	ID             uuid.UUID        `json:"id"`
	DocumentNumber string           `json:"document_number"`
	Title          string           `json:"title"`
	Subtitle       string           `json:"subtitle"`
	Amount         *decimal.Decimal `json:"amount,omitempty"`
	RequestedBy    string           `json:"requested_by"`
	RequestedByID  uuid.UUID        `json:"requested_by_id"`
	Own            bool             `json:"own"` // the caller raised it themselves (some rules forbid self-approval)
	RequestedAt    *time.Time       `json:"requested_at"`
	Href           string           `json:"href"`
}

type Inbox struct {
	Items  []Item         `json:"items"`
	Counts map[string]int `json:"counts"`
	Total  int            `json:"total"`
}

type Service struct{ tx *tx.Manager }

func NewService(t *tx.Manager) *Service { return &Service{tx: t} }

func (s *Service) Inbox(ctx context.Context) (*Inbox, error) {
	p := access.MustPrincipal(ctx)
	q := s.tx.Queries()
	out := &Inbox{Items: []Item{}, Counts: map[string]int{"purchase_order": 0, "stock_transfer": 0, "stock_count": 0}}

	if sc := p.Grants.Scope(PermPO); !sc.Empty() {
		all, paths := sc.SQL()
		rows, err := q.PendingPurchaseOrders(ctx, gen.PendingPurchaseOrdersParams{CompanyID: p.CompanyID, AllScope: all, ScopePaths: paths})
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			where := r.StoreName
			if where == "" {
				where = r.WarehouseName
			}
			amt := r.Total
			out.Items = append(out.Items, Item{Type: "purchase_order", ID: r.ID, DocumentNumber: r.DocumentNumber, Title: "PO " + r.SupplierName, Subtitle: where,
				Amount: &amt, RequestedBy: r.RequestedBy, RequestedByID: r.CreatedBy, Own: r.CreatedBy == p.UserID, RequestedAt: r.SubmittedAt, Href: "/purchasing?tab=po&id=" + r.ID.String()})
		}
	}
	if sc := p.Grants.Scope(PermTransfer); !sc.Empty() {
		all, paths := sc.SQL()
		rows, err := q.PendingTransfers(ctx, gen.PendingTransfersParams{CompanyID: p.CompanyID, AllScope: all, ScopePaths: paths})
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			out.Items = append(out.Items, Item{Type: "stock_transfer", ID: r.ID, DocumentNumber: r.DocumentNumber, Title: "Transfer stok", Subtitle: r.FromName + " → " + r.ToName,
				RequestedBy: r.RequestedBy, RequestedByID: r.CreatedBy, Own: r.CreatedBy == p.UserID, RequestedAt: r.SubmittedAt, Href: "/transfers?id=" + r.ID.String()})
		}
	}
	if sc := p.Grants.Scope(PermCount); !sc.Empty() {
		all, paths := sc.SQL()
		rows, err := q.PendingStockCounts(ctx, gen.PendingStockCountsParams{CompanyID: p.CompanyID, AllScope: all, ScopePaths: paths})
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			where := r.StoreName
			if where == "" {
				where = r.WarehouseName
			}
			v := r.VarianceValue
			by := r.CreatedBy
			if r.SubmittedBy != nil {
				by = *r.SubmittedBy
			}
			out.Items = append(out.Items, Item{Type: "stock_count", ID: r.ID, DocumentNumber: r.DocumentNumber, Title: "Stock opname (" + r.Kind + ")", Subtitle: where,
				Amount: &v, RequestedBy: r.RequestedBy, RequestedByID: by, Own: by == p.UserID, RequestedAt: r.SubmittedAt, Href: "/stock-counts/" + r.ID.String()})
		}
	}
	for _, it := range out.Items {
		out.Counts[it.Type]++
	}
	out.Total = len(out.Items)
	sort.SliceStable(out.Items, func(i, j int) bool {
		a, b := out.Items[i].RequestedAt, out.Items[j].RequestedAt
		if a == nil || b == nil {
			return a != nil
		}
		return a.Before(*b) // oldest waiting first
	})
	return out, nil
}

type Handler struct {
	httpx.Base
	svc *Service
}

func NewHandler(b httpx.Base, s *Service) *Handler { return &Handler{Base: b, svc: s} }

func (h *Handler) Routes(r chi.Router, g *middleware.Guard) {
	r.With(g.AnyPerm(PermPO, PermTransfer, PermCount)).Get("/approvals", func(w http.ResponseWriter, r *http.Request) {
		out, err := h.svc.Inbox(r.Context())
		if err != nil {
			h.Fail(w, r, err)
			return
		}
		response.OK(w, out)
	})
}
