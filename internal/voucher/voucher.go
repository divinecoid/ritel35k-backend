// Package voucher issues and validates vouchers (fixed or percent, optional min spend, usage limits,
// personal vouchers). Redemption is an atomic claim, so concurrent checkouts cannot exceed max_uses.
package voucher

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"strings"
	"time"

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

const Perm = "promotion.manage"

// alphabet avoids look-alikes (0/O, 1/I/L) so codes can be read out over the phone.
const alphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

func genCode() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	var s strings.Builder
	for i, x := range b {
		if i == 4 {
			s.WriteByte('-')
		}
		s.WriteByte(alphabet[int(x)%len(alphabet)])
	}
	return s.String()
}

// Discount is what the voucher takes off `base` (the amount after promotions).
func Discount(v gen.Voucher, base decimal.Decimal) decimal.Decimal {
	var d decimal.Decimal
	if v.Kind == "fixed" {
		d = v.Value
	} else {
		d = base.Mul(v.Value).Div(decimal.NewFromInt(100)).Round(0)
		if v.MaxDiscount != nil && d.GreaterThan(*v.MaxDiscount) {
			d = *v.MaxDiscount
		}
	}
	return decimal.Min(d, base).Round(0)
}

// Check validates a code for a cart and returns the voucher with its discount. It does NOT claim it.
func Check(ctx context.Context, q *gen.Queries, companyID uuid.UUID, code string, base decimal.Decimal, customerID *uuid.UUID, now time.Time) (*gen.Voucher, decimal.Decimal, error) {
	v, err := q.GetVoucherByCode(ctx, gen.GetVoucherByCodeParams{CompanyID: companyID, Code: strings.TrimSpace(code)})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, decimal.Zero, apperr.New(404, "VOUCHER_NOT_FOUND", "Voucher code not found")
	}
	if err != nil {
		return nil, decimal.Zero, err
	}
	switch {
	case v.Status != "active":
		return nil, decimal.Zero, apperr.Conflict("VOUCHER_INACTIVE", "This voucher is not active")
	case v.ValidFrom != nil && now.Before(*v.ValidFrom):
		return nil, decimal.Zero, apperr.Conflict("VOUCHER_NOT_YET_VALID", "This voucher is not valid yet")
	case v.ValidTo != nil && now.After(*v.ValidTo):
		return nil, decimal.Zero, apperr.Conflict("VOUCHER_EXPIRED", "This voucher has expired")
	case v.MaxUses != nil && v.UsedCount >= *v.MaxUses:
		return nil, decimal.Zero, apperr.Conflict("VOUCHER_USED_UP", "This voucher has been fully used")
	case v.CustomerID != nil && (customerID == nil || *customerID != *v.CustomerID):
		return nil, decimal.Zero, apperr.Conflict("VOUCHER_CUSTOMER_ONLY", "This voucher belongs to a specific member")
	case base.LessThan(v.MinSpend):
		return nil, decimal.Zero, apperr.Conflict("VOUCHER_MIN_SPEND", "Minimum spend for this voucher is "+v.MinSpend.Round(0).String())
	}
	d := Discount(v, base)
	if !d.IsPositive() {
		return nil, decimal.Zero, apperr.Conflict("VOUCHER_NO_VALUE", "This voucher gives no discount on this cart")
	}
	return &v, d, nil
}

// Claim atomically takes one use of the voucher (call inside the sale transaction).
func Claim(ctx context.Context, q *gen.Queries, id uuid.UUID) error {
	if _, err := q.ClaimVoucher(ctx, id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apperr.Conflict("VOUCHER_USED_UP", "This voucher has been fully used")
		}
		return err
	}
	return nil
}

type Service struct{ tx *tx.Manager }

func NewService(t *tx.Manager) *Service { return &Service{tx: t} }

type Input struct {
	Code        string           `json:"code" validate:"omitempty,min=4,max=40"` // optional, single voucher only
	Count       int              `json:"count" validate:"min=0,max=500"`
	Kind        string           `json:"kind" validate:"required,oneof=fixed percent"`
	Value       decimal.Decimal  `json:"value"`
	MinSpend    decimal.Decimal  `json:"min_spend"`
	MaxDiscount *decimal.Decimal `json:"max_discount"`
	MaxUses     *int32           `json:"max_uses" validate:"omitempty,min=1"`
	CustomerID  *uuid.UUID       `json:"customer_id"`
	ValidFrom   *time.Time       `json:"valid_from"`
	ValidTo     *time.Time       `json:"valid_to"`
	Batch       string           `json:"batch" validate:"max=60"`
	Note        string           `json:"note" validate:"max=200"`
}

func (s *Service) Create(ctx context.Context, in Input) ([]gen.Voucher, error) {
	p := access.MustPrincipal(ctx)
	if err := access.RequireAll(p, Perm); err != nil {
		return nil, err
	}
	if !in.Value.IsPositive() || (in.Kind == "percent" && in.Value.GreaterThan(decimal.NewFromInt(100))) {
		return nil, apperr.Validation(map[string]string{"value": "must be positive (percent: at most 100)"})
	}
	if in.ValidFrom != nil && in.ValidTo != nil && !in.ValidTo.After(*in.ValidFrom) {
		return nil, apperr.Validation(map[string]string{"valid_to": "must be after valid_from"})
	}
	n := in.Count
	if n <= 0 {
		n = 1
	}
	if in.Code != "" && n > 1 {
		return nil, apperr.Validation(map[string]string{"code": "a fixed code can only create one voucher"})
	}
	var out []gen.Voucher
	err := s.tx.Do(ctx, func(q *gen.Queries) error {
		if in.CustomerID != nil {
			if _, err := q.GetCustomer(ctx, gen.GetCustomerParams{ID: *in.CustomerID, CompanyID: p.CompanyID}); err != nil {
				return apperr.Validation(map[string]string{"customer_id": "unknown customer"})
			}
		}
		for i := 0; i < n; i++ {
			code := in.Code
			if code == "" {
				code = genCode()
			}
			id, _ := uuid.NewV7()
			v, err := q.InsertVoucher(ctx, gen.InsertVoucherParams{ID: id, CompanyID: p.CompanyID, Code: code, Kind: in.Kind, Value: in.Value, MinSpend: in.MinSpend, MaxDiscount: in.MaxDiscount,
				MaxUses: in.MaxUses, CustomerID: in.CustomerID, ValidFrom: in.ValidFrom, ValidTo: in.ValidTo, Batch: in.Batch, Note: in.Note, CreatedBy: &p.UserID})
			var pe *pgconn.PgError
			if errors.As(err, &pe) && pe.Code == "23505" {
				return apperr.Conflict("VOUCHER_CODE_TAKEN", "Voucher code already exists")
			}
			if err != nil {
				return err
			}
			out = append(out, v)
		}
		return audit.Record(ctx, q, audit.Entry{Action: "VOUCHERS_CREATED", Entity: "voucher", EntityID: out[0].ID.String(), New: map[string]any{"count": n, "kind": in.Kind, "value": in.Value.String(), "batch": in.Batch}})
	})
	return out, err
}

func (s *Service) List(ctx context.Context, q, status string, pg pagination.Params) ([]gen.ListVouchersRow, pagination.Meta, error) {
	p := access.MustPrincipal(ctx)
	db := s.tx.Queries()
	rows, err := db.ListVouchers(ctx, gen.ListVouchersParams{CompanyID: p.CompanyID, Q: q, Status: status, PageLimit: pg.Limit(), PageOffset: pg.Offset()})
	if err != nil {
		return nil, pagination.Meta{}, err
	}
	total, err := db.CountVouchers(ctx, gen.CountVouchersParams{CompanyID: p.CompanyID, Q: q, Status: status})
	return rows, pagination.NewMeta(pg, total), err
}

func (s *Service) SetStatus(ctx context.Context, id uuid.UUID, status string) (*gen.Voucher, error) {
	p := access.MustPrincipal(ctx)
	if err := access.RequireAll(p, Perm); err != nil {
		return nil, err
	}
	v, err := s.tx.Queries().SetVoucherStatus(ctx, gen.SetVoucherStatusParams{ID: id, CompanyID: p.CompanyID, Status: status})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apperr.NotFound("voucher")
	}
	return &v, err
}

type Handler struct {
	httpx.Base
	svc *Service
}

func NewHandler(b httpx.Base, s *Service) *Handler { return &Handler{Base: b, svc: s} }

func (h *Handler) Routes(r chi.Router, g *middleware.Guard) {
	r.Route("/vouchers", func(r chi.Router) {
		r.With(g.Perm(Perm)).Get("/", h.list)
		r.With(g.Perm(Perm)).Post("/", h.create)
		r.With(g.Perm(Perm)).Patch("/{id}", h.setStatus)
	})
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	out, meta, err := h.svc.List(r.Context(), r.URL.Query().Get("q"), r.URL.Query().Get("status"), pagination.FromRequest(r))
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OKMeta(w, out, meta)
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	var in Input
	if err := httpx.Bind(r, &in); err != nil {
		h.Fail(w, r, err)
		return
	}
	out, err := h.svc.Create(r.Context(), in)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.Created(w, out)
}

type statusRequest struct {
	Status string `json:"status" validate:"required,oneof=active inactive"`
}

func (h *Handler) setStatus(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	var req statusRequest
	if err := httpx.Bind(r, &req); err != nil {
		h.Fail(w, r, err)
		return
	}
	out, err := h.svc.SetStatus(r.Context(), id, req.Status)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, out)
}
