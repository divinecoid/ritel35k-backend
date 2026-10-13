// Package warehouse manages warehouses (store-bound or central).
package warehouse

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ritel35k/backend/db/gen"
	"github.com/ritel35k/backend/internal/access"
	"github.com/ritel35k/backend/internal/audit"
	"github.com/ritel35k/backend/internal/httpx"
	"github.com/ritel35k/backend/internal/httpx/middleware"
	"github.com/ritel35k/backend/internal/httpx/response"
	"github.com/ritel35k/backend/internal/platform/tx"
	"github.com/ritel35k/backend/pkg/apperr"
)

type Service struct{ tx *tx.Manager }

func NewService(t *tx.Manager) *Service { return &Service{tx: t} }

func (s *Service) List(ctx context.Context, storeID *uuid.UUID) ([]gen.Warehouse, error) {
	p := access.MustPrincipal(ctx)
	all, paths := p.Grants.Scope(access.WarehouseView).SQL()
	return s.tx.Queries().ListWarehouses(ctx, gen.ListWarehousesParams{CompanyID: p.CompanyID, AllScope: all, ScopePaths: paths, StoreID: storeID})
}

type CreateInput struct {
	StoreID *uuid.UUID
	Code    string
	Name    string
	Type    string
}

func (s *Service) Create(ctx context.Context, in CreateInput) (*gen.Warehouse, error) {
	p := access.MustPrincipal(ctx)
	var out gen.Warehouse
	err := s.tx.Do(ctx, func(q *gen.Queries) error {
		if in.StoreID == nil {
			if err := access.RequireAll(p, access.WarehouseManage); err != nil {
				return err
			}
		} else {
			st, err := q.GetStore(ctx, gen.GetStoreParams{ID: *in.StoreID, CompanyID: p.CompanyID})
			if errors.Is(err, pgx.ErrNoRows) {
				return apperr.Validation(map[string]string{"store_id": "unknown store"})
			}
			if err != nil {
				return err
			}
			if !p.Grants.Can(access.WarehouseManage, st.Path) {
				return apperr.Forbidden("not allowed for this store")
			}
		}
		id, _ := uuid.NewV7()
		w, err := q.CreateWarehouse(ctx, gen.CreateWarehouseParams{ID: id, CompanyID: p.CompanyID, StoreID: in.StoreID, Code: in.Code,
			Name: in.Name, Type: in.Type, IsDefault: false, CreatedBy: &p.UserID})
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "23505" {
			return apperr.Conflict("WAREHOUSE_CODE_TAKEN", "Warehouse code already in use")
		}
		if err != nil {
			return err
		}
		out = w
		return audit.Record(ctx, q, audit.Entry{StoreID: in.StoreID, Action: audit.WarehouseCreated, Entity: "warehouse", EntityID: id.String(), New: w})
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *Service) Update(ctx context.Context, id uuid.UUID, name, status *string) (*gen.Warehouse, error) {
	p := access.MustPrincipal(ctx)
	var out gen.Warehouse
	err := s.tx.Do(ctx, func(q *gen.Queries) error {
		cur, err := q.GetWarehouse(ctx, gen.GetWarehouseParams{ID: id, CompanyID: p.CompanyID})
		if errors.Is(err, pgx.ErrNoRows) {
			return apperr.NotFound("warehouse")
		}
		if err != nil {
			return err
		}
		if cur.StoreID == nil {
			if err := access.RequireAll(p, access.WarehouseManage); err != nil {
				return err
			}
		} else {
			st, err := q.GetStore(ctx, gen.GetStoreParams{ID: *cur.StoreID, CompanyID: p.CompanyID})
			if err != nil {
				return err
			}
			if !p.Grants.Can(access.WarehouseManage, st.Path) {
				return apperr.NotFound("warehouse")
			}
		}
		out, err = q.UpdateWarehouse(ctx, gen.UpdateWarehouseParams{ID: id, CompanyID: p.CompanyID, Name: name, Status: status})
		if err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{StoreID: cur.StoreID, Action: audit.WarehouseUpdated, Entity: "warehouse", EntityID: id.String(), Old: cur, New: out})
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

type Handler struct {
	httpx.Base
	svc *Service
}

func NewHandler(b httpx.Base, s *Service) *Handler { return &Handler{Base: b, svc: s} }

func (h *Handler) Routes(r chi.Router, g *middleware.Guard) {
	r.Route("/warehouses", func(r chi.Router) {
		r.With(g.Perm(access.WarehouseView)).Get("/", h.list)
		r.With(g.AnyPerm("inventory.transfer", access.WarehouseView)).Get("/directory", h.directory)
		r.With(g.Perm(access.WarehouseManage)).Post("/", h.create)
		r.With(g.Perm(access.WarehouseManage)).Patch("/{id}", h.update)
	})
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	sid, err := httpx.QueryUUID(r, "store_id")
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	out, err := h.svc.List(r.Context(), sid)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, out)
}

type createRequest struct {
	StoreID *uuid.UUID `json:"store_id"`
	Code    string     `json:"code" validate:"required,min=2,max=32"`
	Name    string     `json:"name" validate:"required,max=120"`
	Type    string     `json:"type" validate:"required,oneof=store main transit damaged return"`
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if err := httpx.Bind(r, &req); err != nil {
		h.Fail(w, r, err)
		return
	}
	out, err := h.svc.Create(r.Context(), CreateInput{StoreID: req.StoreID, Code: req.Code, Name: req.Name, Type: req.Type})
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.Created(w, out)
}

type updateRequest struct {
	Name   *string `json:"name" validate:"omitempty,max=120"`
	Status *string `json:"status" validate:"omitempty,oneof=active inactive"`
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	var req updateRequest
	if err := httpx.Bind(r, &req); err != nil {
		h.Fail(w, r, err)
		return
	}
	out, err := h.svc.Update(r.Context(), id, req.Name, req.Status)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, out)
}

// directory lists every active warehouse (code/name only) for choosing a transfer destination.
func (h *Handler) directory(w http.ResponseWriter, r *http.Request) {
	p := access.MustPrincipal(r.Context())
	out, err := h.svc.tx.Queries().ListWarehouseDirectory(r.Context(), p.CompanyID)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, out)
}
