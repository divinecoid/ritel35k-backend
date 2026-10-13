// Package catalog manages company-wide product taxonomy: categories, brands, units of measure.
// These are global master data: one definition, available in every store.
package catalog

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

func dup(err error, code string) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		return apperr.Conflict(code, "Code already exists")
	}
	return err
}

func (s *Service) write(ctx context.Context, entity, id string, fn func(q *gen.Queries, p *access.Principal) error) error {
	p := access.MustPrincipal(ctx)
	if err := access.RequireAll(p, access.ProductManage); err != nil {
		return err
	}
	return s.tx.Do(ctx, func(q *gen.Queries) error {
		if err := fn(q, p); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{Action: audit.CatalogChanged, Entity: entity, EntityID: id})
	})
}

func (s *Service) Categories(ctx context.Context) ([]gen.Category, error) {
	return s.tx.Queries().ListCategories(ctx, access.MustPrincipal(ctx).CompanyID)
}

func (s *Service) CreateCategory(ctx context.Context, code, name string, parent *uuid.UUID) (out gen.Category, err error) {
	id, _ := uuid.NewV7()
	err = s.write(ctx, "category", id.String(), func(q *gen.Queries, p *access.Principal) (e error) {
		if parent != nil {
			cats, e := q.ListCategories(ctx, p.CompanyID)
			if e != nil {
				return e
			}
			found := false
			for _, c := range cats {
				found = found || c.ID == *parent
			}
			if !found {
				return apperr.Validation(map[string]string{"parent_id": "unknown category"})
			}
		}
		out, e = q.CreateCategory(ctx, gen.CreateCategoryParams{ID: id, CompanyID: p.CompanyID, ParentID: parent, Code: code, Name: name, CreatedBy: &p.UserID})
		return dup(e, "CATEGORY_CODE_TAKEN")
	})
	return
}

func (s *Service) UpdateCategory(ctx context.Context, id uuid.UUID, name *string, parent *uuid.UUID, status *string) (out gen.Category, err error) {
	err = s.write(ctx, "category", id.String(), func(q *gen.Queries, p *access.Principal) (e error) {
		if parent != nil && *parent == id {
			return apperr.Validation(map[string]string{"parent_id": "cannot be its own parent"})
		}
		out, e = q.UpdateCategory(ctx, gen.UpdateCategoryParams{ID: id, CompanyID: p.CompanyID, Name: name, ParentID: parent, Status: status})
		if errors.Is(e, pgx.ErrNoRows) {
			return apperr.NotFound("category")
		}
		return e
	})
	return
}

func (s *Service) Brands(ctx context.Context) ([]gen.Brand, error) {
	return s.tx.Queries().ListBrands(ctx, access.MustPrincipal(ctx).CompanyID)
}

func (s *Service) CreateBrand(ctx context.Context, code, name string) (out gen.Brand, err error) {
	id, _ := uuid.NewV7()
	err = s.write(ctx, "brand", id.String(), func(q *gen.Queries, p *access.Principal) (e error) {
		out, e = q.CreateBrand(ctx, gen.CreateBrandParams{ID: id, CompanyID: p.CompanyID, Code: code, Name: name, CreatedBy: &p.UserID})
		return dup(e, "BRAND_CODE_TAKEN")
	})
	return
}

func (s *Service) UpdateBrand(ctx context.Context, id uuid.UUID, name, status *string) (out gen.Brand, err error) {
	err = s.write(ctx, "brand", id.String(), func(q *gen.Queries, p *access.Principal) (e error) {
		out, e = q.UpdateBrand(ctx, gen.UpdateBrandParams{ID: id, CompanyID: p.CompanyID, Name: name, Status: status})
		if errors.Is(e, pgx.ErrNoRows) {
			return apperr.NotFound("brand")
		}
		return e
	})
	return
}

func (s *Service) UOMs(ctx context.Context) ([]gen.Uom, error) {
	return s.tx.Queries().ListUOMs(ctx, access.MustPrincipal(ctx).CompanyID)
}

func (s *Service) CreateUOM(ctx context.Context, code, name string, decimal bool) (out gen.Uom, err error) {
	id, _ := uuid.NewV7()
	err = s.write(ctx, "uom", id.String(), func(q *gen.Queries, p *access.Principal) (e error) {
		out, e = q.CreateUOM(ctx, gen.CreateUOMParams{ID: id, CompanyID: p.CompanyID, Code: code, Name: name, AllowsDecimal: decimal, CreatedBy: &p.UserID})
		return dup(e, "UOM_CODE_TAKEN")
	})
	return
}

// ---- HTTP ----

type Handler struct {
	httpx.Base
	svc *Service
}

func NewHandler(b httpx.Base, s *Service) *Handler { return &Handler{Base: b, svc: s} }

func (h *Handler) Routes(r chi.Router, g *middleware.Guard) {
	view, manage := g.Perm(access.ProductView), g.Perm(access.ProductManage)
	r.With(view).Get("/categories", h.listCategories)
	r.With(manage).Post("/categories", h.createCategory)
	r.With(manage).Patch("/categories/{id}", h.updateCategory)
	r.With(view).Get("/brands", h.listBrands)
	r.With(manage).Post("/brands", h.createBrand)
	r.With(manage).Patch("/brands/{id}", h.updateBrand)
	r.With(view).Get("/uoms", h.listUOMs)
	r.With(manage).Post("/uoms", h.createUOM)
}

func (h *Handler) out(w http.ResponseWriter, r *http.Request, v any, err error, created bool) {
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	if created {
		response.Created(w, v)
		return
	}
	response.OK(w, v)
}

func (h *Handler) listCategories(w http.ResponseWriter, r *http.Request) {
	v, err := h.svc.Categories(r.Context())
	h.out(w, r, v, err, false)
}

type catReq struct {
	Code     string     `json:"code" validate:"required,min=2,max=32"`
	Name     string     `json:"name" validate:"required,max=120"`
	ParentID *uuid.UUID `json:"parent_id"`
}

func (h *Handler) createCategory(w http.ResponseWriter, r *http.Request) {
	var req catReq
	if err := httpx.Bind(r, &req); err != nil {
		h.Fail(w, r, err)
		return
	}
	v, err := h.svc.CreateCategory(r.Context(), req.Code, req.Name, req.ParentID)
	h.out(w, r, v, err, true)
}

type updReq struct {
	Name     *string    `json:"name" validate:"omitempty,max=120"`
	ParentID *uuid.UUID `json:"parent_id"`
	Status   *string    `json:"status" validate:"omitempty,oneof=active inactive"`
}

func (h *Handler) updateCategory(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	var req updReq
	if err := httpx.Bind(r, &req); err != nil {
		h.Fail(w, r, err)
		return
	}
	v, err := h.svc.UpdateCategory(r.Context(), id, req.Name, req.ParentID, req.Status)
	h.out(w, r, v, err, false)
}

func (h *Handler) listBrands(w http.ResponseWriter, r *http.Request) {
	v, err := h.svc.Brands(r.Context())
	h.out(w, r, v, err, false)
}

func (h *Handler) createBrand(w http.ResponseWriter, r *http.Request) {
	var req catReq
	if err := httpx.Bind(r, &req); err != nil {
		h.Fail(w, r, err)
		return
	}
	v, err := h.svc.CreateBrand(r.Context(), req.Code, req.Name)
	h.out(w, r, v, err, true)
}

func (h *Handler) updateBrand(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	var req updReq
	if err := httpx.Bind(r, &req); err != nil {
		h.Fail(w, r, err)
		return
	}
	v, err := h.svc.UpdateBrand(r.Context(), id, req.Name, req.Status)
	h.out(w, r, v, err, false)
}

func (h *Handler) listUOMs(w http.ResponseWriter, r *http.Request) {
	v, err := h.svc.UOMs(r.Context())
	h.out(w, r, v, err, false)
}

type uomReq struct {
	Code          string `json:"code" validate:"required,min=1,max=16"`
	Name          string `json:"name" validate:"required,max=60"`
	AllowsDecimal bool   `json:"allows_decimal"`
}

func (h *Handler) createUOM(w http.ResponseWriter, r *http.Request) {
	var req uomReq
	if err := httpx.Bind(r, &req); err != nil {
		h.Fail(w, r, err)
		return
	}
	v, err := h.svc.CreateUOM(r.Context(), req.Code, req.Name, req.AllowsDecimal)
	h.out(w, r, v, err, true)
}
