// Package supplier manages the global supplier master and supplier↔variant pricing.
package supplier

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shopspring/decimal"

	"github.com/ritel35k/backend/db/gen"
	"github.com/ritel35k/backend/internal/access"
	"github.com/ritel35k/backend/internal/audit"
	"github.com/ritel35k/backend/internal/httpx"
	"github.com/ritel35k/backend/internal/httpx/middleware"
	"github.com/ritel35k/backend/internal/httpx/response"
	"github.com/ritel35k/backend/internal/platform/tx"
	"github.com/ritel35k/backend/pkg/apperr"
	"github.com/ritel35k/backend/pkg/pagination"
)

type Service struct{ tx *tx.Manager }

func NewService(t *tx.Manager) *Service { return &Service{tx: t} }

func (s *Service) List(ctx context.Context, qstr string, pg pagination.Params) ([]gen.Supplier, pagination.Meta, error) {
	p := access.MustPrincipal(ctx)
	q := s.tx.Queries()
	rows, err := q.ListSuppliers(ctx, gen.ListSuppliersParams{CompanyID: p.CompanyID, Q: qstr, PageLimit: pg.Limit(), PageOffset: pg.Offset()})
	if err != nil {
		return nil, pagination.Meta{}, err
	}
	total, err := q.CountSuppliers(ctx, gen.CountSuppliersParams{CompanyID: p.CompanyID, Q: qstr})
	return rows, pagination.NewMeta(pg, total), err
}

func (s *Service) Get(ctx context.Context, id uuid.UUID) (*gen.Supplier, error) {
	p := access.MustPrincipal(ctx)
	r, err := s.tx.Queries().GetSupplier(ctx, gen.GetSupplierParams{ID: id, CompanyID: p.CompanyID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apperr.NotFound("supplier")
	}
	return &r, err
}

type CreateInput struct {
	Code, Name, PIC, Phone, Email, Address, NPWP, Currency string
	PaymentTermDays, LeadTimeDays                          int32
	BankAccount                                            json.RawMessage
}

func (s *Service) Create(ctx context.Context, in CreateInput) (*gen.Supplier, error) {
	p := access.MustPrincipal(ctx)
	if err := access.RequireAll(p, access.SupplierManage); err != nil {
		return nil, err
	}
	if in.Currency == "" {
		in.Currency = "IDR"
	}
	if len(in.BankAccount) == 0 {
		in.BankAccount = json.RawMessage(`{}`)
	}
	var out gen.Supplier
	err := s.tx.Do(ctx, func(q *gen.Queries) error {
		id, _ := uuid.NewV7()
		r, err := q.CreateSupplier(ctx, gen.CreateSupplierParams{ID: id, CompanyID: p.CompanyID, Code: in.Code, Name: in.Name, Pic: in.PIC, Phone: in.Phone,
			Email: in.Email, Address: in.Address, Npwp: in.NPWP, PaymentTermDays: in.PaymentTermDays, BankAccount: in.BankAccount,
			LeadTimeDays: in.LeadTimeDays, Currency: in.Currency, CreatedBy: &p.UserID})
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "23505" {
			return apperr.Conflict("SUPPLIER_CODE_TAKEN", "Supplier code already exists")
		}
		if err != nil {
			return err
		}
		out = r
		return audit.Record(ctx, q, audit.Entry{Action: audit.SupplierCreated, Entity: "supplier", EntityID: id.String(), New: r})
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

type UpdateInput struct {
	Name, PIC, Phone, Email, Address, NPWP, Currency, Status *string
	PaymentTermDays, LeadTimeDays                            *int32
	BankAccount                                              json.RawMessage
}

func (s *Service) Update(ctx context.Context, id uuid.UUID, in UpdateInput) (*gen.Supplier, error) {
	p := access.MustPrincipal(ctx)
	if err := access.RequireAll(p, access.SupplierManage); err != nil {
		return nil, err
	}
	var out gen.Supplier
	err := s.tx.Do(ctx, func(q *gen.Queries) error {
		r, err := q.UpdateSupplier(ctx, gen.UpdateSupplierParams{ID: id, CompanyID: p.CompanyID, Name: in.Name, Pic: in.PIC, Phone: in.Phone, Email: in.Email,
			Address: in.Address, Npwp: in.NPWP, PaymentTermDays: in.PaymentTermDays, BankAccount: in.BankAccount, LeadTimeDays: in.LeadTimeDays,
			Currency: in.Currency, Status: in.Status})
		if errors.Is(err, pgx.ErrNoRows) {
			return apperr.NotFound("supplier")
		}
		if err != nil {
			return err
		}
		out = r
		return audit.Record(ctx, q, audit.Entry{Action: audit.SupplierUpdated, Entity: "supplier", EntityID: id.String(), New: in})
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

type LinkInput struct {
	VariantID     uuid.UUID
	SupplierSKU   string
	PurchasePrice decimal.Decimal
	MOQ           decimal.Decimal
	LeadTimeDays  int32
}

func (s *Service) LinkProduct(ctx context.Context, supplierID uuid.UUID, in LinkInput) (*gen.SupplierProduct, error) {
	p := access.MustPrincipal(ctx)
	if err := access.RequireAll(p, access.SupplierManage); err != nil {
		return nil, err
	}
	var out gen.SupplierProduct
	err := s.tx.Do(ctx, func(q *gen.Queries) error {
		if _, err := q.GetSupplier(ctx, gen.GetSupplierParams{ID: supplierID, CompanyID: p.CompanyID}); err != nil {
			return apperr.NotFound("supplier")
		}
		if _, err := q.GetVariant(ctx, gen.GetVariantParams{ID: in.VariantID, CompanyID: p.CompanyID}); err != nil {
			return apperr.Validation(map[string]string{"variant_id": "unknown variant"})
		}
		r, err := q.UpsertSupplierProduct(ctx, gen.UpsertSupplierProductParams{SupplierID: supplierID, VariantID: in.VariantID, CompanyID: p.CompanyID,
			SupplierSku: in.SupplierSKU, PurchasePrice: in.PurchasePrice, Moq: in.MOQ, LeadTimeDays: in.LeadTimeDays})
		out = r
		return err
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *Service) Products(ctx context.Context, supplierID uuid.UUID) ([]gen.ListSupplierProductsRow, error) {
	p := access.MustPrincipal(ctx)
	return s.tx.Queries().ListSupplierProducts(ctx, gen.ListSupplierProductsParams{SupplierID: supplierID, CompanyID: p.CompanyID})
}

func (s *Service) Unlink(ctx context.Context, supplierID, variantID uuid.UUID) error {
	p := access.MustPrincipal(ctx)
	if err := access.RequireAll(p, access.SupplierManage); err != nil {
		return err
	}
	n, err := s.tx.Queries().DeleteSupplierProduct(ctx, gen.DeleteSupplierProductParams{SupplierID: supplierID, VariantID: variantID, CompanyID: p.CompanyID})
	if err != nil {
		return err
	}
	if n == 0 {
		return apperr.NotFound("supplier product")
	}
	return nil
}

// ---- HTTP ----

type Handler struct {
	httpx.Base
	svc *Service
}

func NewHandler(b httpx.Base, s *Service) *Handler { return &Handler{Base: b, svc: s} }

func (h *Handler) Routes(r chi.Router, g *middleware.Guard) {
	view, manage := g.Perm(access.SupplierView), g.Perm(access.SupplierManage)
	r.Route("/suppliers", func(r chi.Router) {
		r.With(view).Get("/", h.list)
		r.With(view).Get("/{id}", h.get)
		r.With(manage).Post("/", h.create)
		r.With(manage).Patch("/{id}", h.update)
		r.With(view).Get("/{id}/products", h.products)
		r.With(manage).Put("/{id}/products", h.link)
		r.With(manage).Delete("/{id}/products/{variantId}", h.unlink)
	})
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	out, meta, err := h.svc.List(r.Context(), r.URL.Query().Get("q"), pagination.FromRequest(r))
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OKMeta(w, out, meta)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	out, err := h.svc.Get(r.Context(), id)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, out)
}

type createRequest struct {
	Code            string          `json:"code" validate:"required,min=2,max=32"`
	Name            string          `json:"name" validate:"required,max=200"`
	PIC             string          `json:"pic" validate:"max=120"`
	Phone           string          `json:"phone" validate:"max=40"`
	Email           string          `json:"email" validate:"omitempty,email"`
	Address         string          `json:"address" validate:"max=500"`
	NPWP            string          `json:"npwp" validate:"max=32"`
	PaymentTermDays int32           `json:"payment_term_days" validate:"min=0,max=365"`
	LeadTimeDays    int32           `json:"lead_time_days" validate:"min=0,max=365"`
	Currency        string          `json:"currency" validate:"omitempty,len=3"`
	BankAccount     json.RawMessage `json:"bank_account"`
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if err := httpx.Bind(r, &req); err != nil {
		h.Fail(w, r, err)
		return
	}
	out, err := h.svc.Create(r.Context(), CreateInput{Code: req.Code, Name: req.Name, PIC: req.PIC, Phone: req.Phone, Email: req.Email, Address: req.Address,
		NPWP: req.NPWP, Currency: req.Currency, PaymentTermDays: req.PaymentTermDays, LeadTimeDays: req.LeadTimeDays, BankAccount: req.BankAccount})
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.Created(w, out)
}

type updateRequest struct {
	Name            *string         `json:"name" validate:"omitempty,max=200"`
	PIC             *string         `json:"pic" validate:"omitempty,max=120"`
	Phone           *string         `json:"phone" validate:"omitempty,max=40"`
	Email           *string         `json:"email" validate:"omitempty,email"`
	Address         *string         `json:"address" validate:"omitempty,max=500"`
	NPWP            *string         `json:"npwp" validate:"omitempty,max=32"`
	PaymentTermDays *int32          `json:"payment_term_days" validate:"omitempty,min=0,max=365"`
	LeadTimeDays    *int32          `json:"lead_time_days" validate:"omitempty,min=0,max=365"`
	Currency        *string         `json:"currency" validate:"omitempty,len=3"`
	Status          *string         `json:"status" validate:"omitempty,oneof=active inactive"`
	BankAccount     json.RawMessage `json:"bank_account"`
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
	out, err := h.svc.Update(r.Context(), id, UpdateInput{Name: req.Name, PIC: req.PIC, Phone: req.Phone, Email: req.Email, Address: req.Address, NPWP: req.NPWP,
		PaymentTermDays: req.PaymentTermDays, LeadTimeDays: req.LeadTimeDays, Currency: req.Currency, Status: req.Status, BankAccount: req.BankAccount})
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, out)
}

func (h *Handler) products(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	out, err := h.svc.Products(r.Context(), id)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, out)
}

type linkRequest struct {
	VariantID     uuid.UUID       `json:"variant_id" validate:"required"`
	SupplierSKU   string          `json:"supplier_sku" validate:"max=64"`
	PurchasePrice decimal.Decimal `json:"purchase_price"`
	MOQ           decimal.Decimal `json:"moq"`
	LeadTimeDays  int32           `json:"lead_time_days" validate:"min=0,max=365"`
}

func (h *Handler) link(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	var req linkRequest
	if err := httpx.Bind(r, &req); err != nil {
		h.Fail(w, r, err)
		return
	}
	if req.MOQ.IsZero() {
		req.MOQ = decimal.NewFromInt(1)
	}
	out, err := h.svc.LinkProduct(r.Context(), id, LinkInput{VariantID: req.VariantID, SupplierSKU: req.SupplierSKU, PurchasePrice: req.PurchasePrice, MOQ: req.MOQ, LeadTimeDays: req.LeadTimeDays})
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, out)
}

func (h *Handler) unlink(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	vid, err := httpx.PathUUID(r, "variantId")
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	if err := h.svc.Unlink(r.Context(), id, vid); err != nil {
		h.Fail(w, r, err)
		return
	}
	response.NoContent(w)
}
