// Package pricing manages selling prices: named price lists, inheritance (company → region → store)
// and volume tiers (min_qty).
package pricing

import (
	"context"
	"errors"
	"net/http"
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
)

type Resolved struct {
	Price        decimal.Decimal `json:"price"`
	MinQty       decimal.Decimal `json:"min_qty"`
	ListID       uuid.UUID       `json:"price_list_id"`
	SourceUnitID uuid.UUID       `json:"source_org_unit_id"`
	SourceType   string          `json:"source_type"`
	SourceName   string          `json:"source_name"`
}

// Resolve returns the effective price for one price list. ok=false: that list has no price for this item here.
func Resolve(ctx context.Context, q *gen.Queries, companyID, storeOrgUnitID, variantID, listID uuid.UUID, qty decimal.Decimal, at time.Time) (Resolved, bool, error) {
	r, err := q.ResolvePrice(ctx, gen.ResolvePriceParams{CompanyID: companyID, VariantID: variantID, PriceListID: listID, StoreOrgUnitID: storeOrgUnitID, Qty: qty, At: at})
	if errors.Is(err, pgx.ErrNoRows) {
		return Resolved{}, false, nil
	}
	if err != nil {
		return Resolved{}, false, err
	}
	return Resolved{Price: r.Price, MinQty: r.MinQty, ListID: listID, SourceUnitID: r.SourceOrgUnitID, SourceType: r.SourceType, SourceName: r.SourceName}, true, nil
}

// ResolveBest looks at every price list that applies to the customer (the default list, plus any list bound
// to their membership level) and returns the LOWEST price: a member price can never be worse than the normal one.
func ResolveBest(ctx context.Context, q *gen.Queries, companyID, storeOrgUnitID, variantID uuid.UUID, qty decimal.Decimal, levelID *uuid.UUID, at time.Time) (Resolved, bool, error) {
	ids, err := q.ListApplicablePriceLists(ctx, gen.ListApplicablePriceListsParams{CompanyID: companyID, LevelID: levelID})
	if err != nil {
		return Resolved{}, false, err
	}
	var best Resolved
	found := false
	for _, id := range ids {
		r, ok, err := Resolve(ctx, q, companyID, storeOrgUnitID, variantID, id, qty, at)
		if err != nil {
			return Resolved{}, false, err
		}
		if ok && (!found || r.Price.LessThan(best.Price)) {
			best, found = r, true
		}
	}
	return best, found, nil
}

type Service struct{ tx *tx.Manager }

func NewService(t *tx.Manager) *Service { return &Service{tx: t} }

// SetAt sets a price at an org unit for a price list (nil = the default list) and volume tier. It closes the
// previous open price for the same (unit, item, list, tier), keeping history. Callers authorize the unit.
func SetAt(ctx context.Context, q *gen.Queries, companyID, orgUnitID, variantID uuid.UUID, listID *uuid.UUID, minQty, price decimal.Decimal, from time.Time, by *uuid.UUID) error {
	if price.IsNegative() {
		return apperr.Validation(map[string]string{"price": "must not be negative"})
	}
	if !minQty.IsPositive() {
		minQty = decimal.NewFromInt(1)
	}
	var lid uuid.UUID
	if listID != nil {
		pl, err := q.GetPriceList(ctx, gen.GetPriceListParams{ID: *listID, CompanyID: companyID})
		if errors.Is(err, pgx.ErrNoRows) {
			return apperr.Validation(map[string]string{"price_list_id": "unknown price list"})
		}
		if err != nil {
			return err
		}
		lid = pl.ID
	} else {
		pl, err := q.GetDefaultPriceList(ctx, companyID)
		if err != nil {
			return err
		}
		lid = pl.ID
	}
	if err := q.ClosePrices(ctx, gen.ClosePricesParams{CloseAt: &from, OrgUnitID: orgUnitID, VariantID: variantID, CompanyID: companyID, PriceListID: lid, MinQty: minQty}); err != nil {
		return err
	}
	id, _ := uuid.NewV7()
	_, err := q.InsertPrice(ctx, gen.InsertPriceParams{ID: id, CompanyID: companyID, OrgUnitID: orgUnitID, VariantID: variantID, PriceListID: lid, Price: price, MinQty: minQty, EffectiveFrom: from, CreatedBy: by})
	return err
}

type SetInput struct {
	OrgUnitID     uuid.UUID
	VariantID     uuid.UUID
	PriceListID   *uuid.UUID
	MinQty        decimal.Decimal
	Price         decimal.Decimal
	EffectiveFrom *time.Time
}

func (s *Service) authorizeUnit(ctx context.Context, q *gen.Queries, p *access.Principal, orgUnitID uuid.UUID) error {
	u, err := q.GetOrgUnit(ctx, gen.GetOrgUnitParams{ID: orgUnitID, CompanyID: p.CompanyID})
	if errors.Is(err, pgx.ErrNoRows) {
		return apperr.NotFound("org unit")
	}
	if err != nil {
		return err
	}
	if !p.Grants.Can(access.PricingManage, u.Path) {
		return apperr.Forbidden("not allowed to set prices here")
	}
	return nil
}

func (s *Service) Set(ctx context.Context, in SetInput) error {
	p := access.MustPrincipal(ctx)
	at := time.Now()
	if in.EffectiveFrom != nil {
		at = *in.EffectiveFrom
	}
	return s.tx.Do(ctx, func(q *gen.Queries) error {
		if err := s.authorizeUnit(ctx, q, p, in.OrgUnitID); err != nil {
			return err
		}
		if _, err := q.GetVariant(ctx, gen.GetVariantParams{ID: in.VariantID, CompanyID: p.CompanyID}); err != nil {
			return apperr.Validation(map[string]string{"variant_id": "unknown variant"})
		}
		if err := SetAt(ctx, q, p.CompanyID, in.OrgUnitID, in.VariantID, in.PriceListID, in.MinQty, in.Price, at, &p.UserID); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{Action: "PRICE_CHANGED", Entity: "price", EntityID: in.VariantID.String() + "@" + in.OrgUnitID.String(),
			New: map[string]any{"price": in.Price.String(), "min_qty": in.MinQty.String(), "price_list_id": in.PriceListID, "org_unit_id": in.OrgUnitID, "effective_from": at}})
	})
}

// Unset removes the override at this unit/list/tier so the price falls back to the inherited one.
func (s *Service) Unset(ctx context.Context, orgUnitID, variantID uuid.UUID, listID *uuid.UUID, minQty decimal.Decimal) error {
	p := access.MustPrincipal(ctx)
	if !minQty.IsPositive() {
		minQty = decimal.NewFromInt(1)
	}
	return s.tx.Do(ctx, func(q *gen.Queries) error {
		if err := s.authorizeUnit(ctx, q, p, orgUnitID); err != nil {
			return err
		}
		var lid uuid.UUID
		if listID != nil {
			lid = *listID
		} else {
			pl, err := q.GetDefaultPriceList(ctx, p.CompanyID)
			if err != nil {
				return err
			}
			lid = pl.ID
		}
		now := time.Now()
		if err := q.ClosePrices(ctx, gen.ClosePricesParams{CloseAt: &now, OrgUnitID: orgUnitID, VariantID: variantID, CompanyID: p.CompanyID, PriceListID: lid, MinQty: minQty}); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{Action: "PRICE_REVERTED", Entity: "price", EntityID: variantID.String() + "@" + orgUnitID.String()})
	})
}

func (s *Service) ResolveFor(ctx context.Context, storeID, variantID uuid.UUID, qty decimal.Decimal, levelID *uuid.UUID) (*Resolved, error) {
	p := access.MustPrincipal(ctx)
	q := s.tx.Queries()
	st, err := q.GetStore(ctx, gen.GetStoreParams{ID: storeID, CompanyID: p.CompanyID})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !p.Grants.Can(access.PricingView, st.Path)) {
		return nil, apperr.NotFound("store")
	}
	if err != nil {
		return nil, err
	}
	if !qty.IsPositive() {
		qty = decimal.NewFromInt(1)
	}
	r, ok, err := ResolveBest(ctx, q, p.CompanyID, st.OrgUnitID, variantID, qty, levelID, time.Now())
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, apperr.NotFound("price")
	}
	return &r, nil
}

// List returns the price history of a variant at every org unit the caller may view.
func (s *Service) List(ctx context.Context, variantID uuid.UUID) ([]gen.ListVariantPricesRow, error) {
	p := access.MustPrincipal(ctx)
	rows, err := s.tx.Queries().ListVariantPrices(ctx, gen.ListVariantPricesParams{CompanyID: p.CompanyID, VariantID: variantID})
	if err != nil {
		return nil, err
	}
	sc := p.Grants.Scope(access.PricingView)
	out := rows[:0]
	for _, r := range rows {
		// a unit's prices are visible if it lies inside the caller's scope, or is an ancestor of it (inherited context)
		if sc.Allows(r.OrgUnitPath) || ancestorOf(r.OrgUnitPath, sc) {
			out = append(out, r)
		}
	}
	return out, nil
}

func ancestorOf(path string, sc access.ScopeFilter) bool {
	for _, p := range sc.Paths {
		if len(p) > len(path) && p[:len(path)] == path && p[len(path)] == '.' {
			return true
		}
	}
	return false
}

func (s *Service) Lists(ctx context.Context) ([]gen.ListPriceListsRow, error) {
	return s.tx.Queries().ListPriceLists(ctx, access.MustPrincipal(ctx).CompanyID)
}

func (s *Service) CreateList(ctx context.Context, code, name string, levelID *uuid.UUID) (*gen.PriceList, error) {
	p := access.MustPrincipal(ctx)
	if err := access.RequireAll(p, access.PricingManage); err != nil {
		return nil, err
	}
	var out gen.PriceList
	err := s.tx.Do(ctx, func(q *gen.Queries) error {
		if levelID != nil {
			if _, err := q.GetLevel(ctx, gen.GetLevelParams{ID: *levelID, CompanyID: p.CompanyID}); err != nil {
				return apperr.Validation(map[string]string{"membership_level_id": "unknown level"})
			}
		}
		id, _ := uuid.NewV7()
		pl, err := q.CreatePriceList(ctx, gen.CreatePriceListParams{ID: id, CompanyID: p.CompanyID, Code: code, Name: name, MembershipLevelID: levelID})
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "23505" {
			return apperr.Conflict("PRICE_LIST_CODE_TAKEN", "Price list code already exists")
		}
		if err != nil {
			return err
		}
		out = pl
		return audit.Record(ctx, q, audit.Entry{Action: "PRICE_LIST_CREATED", Entity: "price_list", EntityID: id.String(), New: pl})
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *Service) UpdateList(ctx context.Context, id uuid.UUID, name, status *string) (*gen.PriceList, error) {
	p := access.MustPrincipal(ctx)
	if err := access.RequireAll(p, access.PricingManage); err != nil {
		return nil, err
	}
	pl, err := s.tx.Queries().UpdatePriceList(ctx, gen.UpdatePriceListParams{ID: id, CompanyID: p.CompanyID, Name: name, Status: status})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apperr.NotFound("price list")
	}
	if err == nil && pl.IsDefault && pl.Status != "active" {
		return nil, apperr.Conflict("DEFAULT_LIST_ACTIVE", "The default price list cannot be deactivated")
	}
	return &pl, err
}

type Handler struct {
	httpx.Base
	svc *Service
}

func NewHandler(b httpx.Base, s *Service) *Handler { return &Handler{Base: b, svc: s} }

func (h *Handler) Routes(r chi.Router, g *middleware.Guard) {
	r.With(g.Perm(access.PricingManage)).Put("/prices", h.set)
	r.With(g.Perm(access.PricingManage)).Delete("/prices", h.unset)
	r.With(g.Perm(access.PricingView)).Get("/prices/resolve", h.resolve)
	r.With(g.Perm(access.PricingView)).Get("/variants/{id}/prices", h.list)
	r.With(g.AnyPerm(access.PricingView, access.PricingManage, "promotion.manage")).Get("/price-lists", h.lists)
	r.With(g.Perm(access.PricingManage)).Post("/price-lists", h.createList)
	r.With(g.Perm(access.PricingManage)).Patch("/price-lists/{id}", h.updateList)
}

type setRequest struct {
	OrgUnitID     uuid.UUID       `json:"org_unit_id" validate:"required"`
	VariantID     uuid.UUID       `json:"variant_id" validate:"required"`
	PriceListID   *uuid.UUID      `json:"price_list_id"`
	MinQty        decimal.Decimal `json:"min_qty"`
	Price         decimal.Decimal `json:"price"`
	EffectiveFrom *time.Time      `json:"effective_from"`
}

func (h *Handler) set(w http.ResponseWriter, r *http.Request) {
	var req setRequest
	if err := httpx.Bind(r, &req); err != nil {
		h.Fail(w, r, err)
		return
	}
	if err := h.svc.Set(r.Context(), SetInput(req)); err != nil {
		h.Fail(w, r, err)
		return
	}
	response.NoContent(w)
}

type unsetRequest struct {
	OrgUnitID   uuid.UUID       `json:"org_unit_id" validate:"required"`
	VariantID   uuid.UUID       `json:"variant_id" validate:"required"`
	PriceListID *uuid.UUID      `json:"price_list_id"`
	MinQty      decimal.Decimal `json:"min_qty"`
}

func (h *Handler) unset(w http.ResponseWriter, r *http.Request) {
	var req unsetRequest
	if err := httpx.Bind(r, &req); err != nil {
		h.Fail(w, r, err)
		return
	}
	if err := h.svc.Unset(r.Context(), req.OrgUnitID, req.VariantID, req.PriceListID, req.MinQty); err != nil {
		h.Fail(w, r, err)
		return
	}
	response.NoContent(w)
}

func (h *Handler) resolve(w http.ResponseWriter, r *http.Request) {
	sid, e1 := httpx.QueryUUID(r, "store_id")
	vid, e2 := httpx.QueryUUID(r, "variant_id")
	lvl, e3 := httpx.QueryUUID(r, "level_id")
	if e1 != nil || e2 != nil || e3 != nil || sid == nil || vid == nil {
		h.Fail(w, r, apperr.BadRequest("store_id and variant_id are required"))
		return
	}
	qty, _ := decimal.NewFromString(r.URL.Query().Get("qty"))
	out, err := h.svc.ResolveFor(r.Context(), *sid, *vid, qty, lvl)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, out)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	out, err := h.svc.List(r.Context(), id)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, out)
}

func (h *Handler) lists(w http.ResponseWriter, r *http.Request) {
	out, err := h.svc.Lists(r.Context())
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, out)
}

type listRequest struct {
	Code              string     `json:"code" validate:"required,min=2,max=30"`
	Name              string     `json:"name" validate:"required,max=80"`
	MembershipLevelID *uuid.UUID `json:"membership_level_id"`
}

func (h *Handler) createList(w http.ResponseWriter, r *http.Request) {
	var req listRequest
	if err := httpx.Bind(r, &req); err != nil {
		h.Fail(w, r, err)
		return
	}
	out, err := h.svc.CreateList(r.Context(), req.Code, req.Name, req.MembershipLevelID)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.Created(w, out)
}

type listUpdate struct {
	Name   *string `json:"name" validate:"omitempty,max=80"`
	Status *string `json:"status" validate:"omitempty,oneof=active inactive"`
}

func (h *Handler) updateList(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	var req listUpdate
	if err := httpx.Bind(r, &req); err != nil {
		h.Fail(w, r, err)
		return
	}
	out, err := h.svc.UpdateList(r.Context(), id, req.Name, req.Status)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, out)
}
