package promotion

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shopspring/decimal"

	"github.com/ritel35k/backend/db/gen"
	"github.com/ritel35k/backend/internal/access"
	"github.com/ritel35k/backend/internal/audit"
	"github.com/ritel35k/backend/internal/platform/tx"
	"github.com/ritel35k/backend/pkg/apperr"
)

const Perm = "promotion.manage"

type Service struct{ tx *tx.Manager }

func NewService(t *tx.Manager) *Service { return &Service{tx: t} }

type ScopeInput struct {
	OrgUnitIDs  []uuid.UUID `json:"org_unit_ids"`
	VariantIDs  []uuid.UUID `json:"variant_ids"`
	CategoryIDs []uuid.UUID `json:"category_ids"`
	BrandIDs    []uuid.UUID `json:"brand_ids"`
	LevelIDs    []uuid.UUID `json:"level_ids"`
}

type Input struct {
	Code        string          `json:"code" validate:"required,min=2,max=40"`
	Name        string          `json:"name" validate:"required,max=120"`
	Description string          `json:"description" validate:"max=300"`
	Type        string          `json:"type" validate:"required,oneof=percent fixed bxgy bundle threshold"`
	Config      json.RawMessage `json:"config" validate:"required"`
	Priority    int32           `json:"priority"`
	Stackable   *bool           `json:"stackable"` // default true
	Exclusive   bool            `json:"exclusive"`
	StartsAt    *time.Time      `json:"starts_at"`
	EndsAt      *time.Time      `json:"ends_at"`
	DaysOfWeek  []int16         `json:"days_of_week"`
	TimeFrom    *string         `json:"time_from" validate:"omitempty,len=5"`
	TimeTo      *string         `json:"time_to" validate:"omitempty,len=5"`
	Status      string          `json:"status" validate:"omitempty,oneof=active inactive"`
	Scopes      ScopeInput      `json:"scopes"`
}

type Detail struct {
	gen.Promotion
	Scopes ScopeInput `json:"scopes"`
}

// ValidateConfig checks the type-specific settings (so a bad promotion can never reach the POS).
func ValidateConfig(t string, raw json.RawMessage) (Config, error) {
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, apperr.Validation(map[string]string{"config": "invalid JSON: " + err.Error()})
	}
	bad := func(m string) (Config, error) { return c, apperr.Validation(map[string]string{"config": m}) }
	hundred := decimal.NewFromInt(100)
	switch Type(t) {
	case Percent:
		if !c.Percent.IsPositive() || c.Percent.GreaterThan(hundred) {
			return bad("percent must be between 0 and 100")
		}
	case Fixed:
		if !c.Amount.IsPositive() {
			return bad("amount must be positive")
		}
		if c.Per != "" && c.Per != "unit" && c.Per != "order" {
			return bad("per must be unit or order")
		}
	case BxGy:
		if c.BuyQty < 1 || c.GetQty < 1 {
			return bad("buy_qty and get_qty must be at least 1")
		}
		if c.GetDiscountPercent.IsNegative() || c.GetDiscountPercent.GreaterThan(hundred) {
			return bad("get_discount_percent must be between 0 and 100 (default 100 = free)")
		}
	case Bundle:
		if c.Qty < 2 || !c.Price.IsPositive() {
			return bad("qty must be at least 2 and price positive")
		}
	case Threshold:
		hasAmt, hasPct := c.Amount.IsPositive(), c.Percent.IsPositive()
		if hasAmt == hasPct || c.MinSpend.IsNegative() {
			return bad("set exactly one of amount or percent, and min_spend >= 0")
		}
		if hasPct && c.Percent.GreaterThan(hundred) {
			return bad("percent must be at most 100")
		}
	}
	return c, nil
}

func (s *Service) List(ctx context.Context, status, q string) ([]gen.Promotion, error) {
	p := access.MustPrincipal(ctx)
	return s.tx.Queries().ListPromotions(ctx, gen.ListPromotionsParams{CompanyID: p.CompanyID, Status: status, Q: q})
}

func (s *Service) Get(ctx context.Context, id uuid.UUID) (*Detail, error) {
	p := access.MustPrincipal(ctx)
	q := s.tx.Queries()
	pr, err := q.GetPromotion(ctx, gen.GetPromotionParams{ID: id, CompanyID: p.CompanyID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apperr.NotFound("promotion")
	}
	if err != nil {
		return nil, err
	}
	sc, err := q.ListPromotionScopes(ctx, []uuid.UUID{id})
	if err != nil {
		return nil, err
	}
	d := &Detail{Promotion: pr}
	for _, r := range sc {
		switch r.ScopeType {
		case "org_unit":
			d.Scopes.OrgUnitIDs = append(d.Scopes.OrgUnitIDs, r.ScopeID)
		case "product":
			d.Scopes.VariantIDs = append(d.Scopes.VariantIDs, r.ScopeID)
		case "category":
			d.Scopes.CategoryIDs = append(d.Scopes.CategoryIDs, r.ScopeID)
		case "brand":
			d.Scopes.BrandIDs = append(d.Scopes.BrandIDs, r.ScopeID)
		case "level":
			d.Scopes.LevelIDs = append(d.Scopes.LevelIDs, r.ScopeID)
		}
	}
	return d, nil
}

func writeScopes(ctx context.Context, q *gen.Queries, id uuid.UUID, sc ScopeInput) error {
	if err := q.DeletePromotionScopes(ctx, id); err != nil {
		return err
	}
	add := func(kind string, ids []uuid.UUID) error {
		for _, x := range ids {
			if err := q.InsertPromotionScope(ctx, gen.InsertPromotionScopeParams{PromotionID: id, ScopeType: kind, ScopeID: x}); err != nil {
				return err
			}
		}
		return nil
	}
	for kind, ids := range map[string][]uuid.UUID{"org_unit": sc.OrgUnitIDs, "product": sc.VariantIDs, "category": sc.CategoryIDs, "brand": sc.BrandIDs, "level": sc.LevelIDs} {
		if err := add(kind, ids); err != nil {
			return err
		}
	}
	return nil
}

func normalize(in *Input) error {
	if (in.TimeFrom == nil) != (in.TimeTo == nil) {
		return apperr.Validation(map[string]string{"time_from": "set both time_from and time_to, or neither"})
	}
	if in.StartsAt != nil && in.EndsAt != nil && !in.EndsAt.After(*in.StartsAt) {
		return apperr.Validation(map[string]string{"ends_at": "must be after starts_at"})
	}
	for _, d := range in.DaysOfWeek {
		if d < 0 || d > 6 {
			return apperr.Validation(map[string]string{"days_of_week": "0 (Sunday) to 6"})
		}
	}
	if in.Status == "" {
		in.Status = "active"
	}
	if in.Stackable == nil {
		t := true
		in.Stackable = &t
	}
	if in.Priority == 0 {
		in.Priority = 100
	}
	return nil
}

func (s *Service) Create(ctx context.Context, in Input) (*Detail, error) {
	p := access.MustPrincipal(ctx)
	if err := access.RequireAll(p, Perm); err != nil {
		return nil, err
	}
	if err := normalize(&in); err != nil {
		return nil, err
	}
	if _, err := ValidateConfig(in.Type, in.Config); err != nil {
		return nil, err
	}
	var id uuid.UUID
	err := s.tx.Do(ctx, func(q *gen.Queries) error {
		id, _ = uuid.NewV7()
		pr, err := q.InsertPromotion(ctx, gen.InsertPromotionParams{ID: id, CompanyID: p.CompanyID, Code: in.Code, Name: in.Name, Description: in.Description, Type: in.Type,
			Config: in.Config, Priority: in.Priority, Stackable: *in.Stackable, Exclusive: in.Exclusive, StartsAt: in.StartsAt, EndsAt: in.EndsAt, DaysOfWeek: in.DaysOfWeek,
			TimeFrom: in.TimeFrom, TimeTo: in.TimeTo, CreatedBy: &p.UserID})
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "23505" {
			return apperr.Conflict("PROMOTION_CODE_TAKEN", "Promotion code already exists")
		}
		if err != nil {
			return err
		}
		if in.Status != "active" {
			if _, err := q.UpdatePromotion(ctx, gen.UpdatePromotionParams{ID: id, CompanyID: p.CompanyID, Name: pr.Name, Description: pr.Description, Config: pr.Config, Priority: pr.Priority,
				Stackable: pr.Stackable, Exclusive: pr.Exclusive, StartsAt: pr.StartsAt, EndsAt: pr.EndsAt, DaysOfWeek: pr.DaysOfWeek, TimeFrom: pr.TimeFrom, TimeTo: pr.TimeTo, Status: in.Status}); err != nil {
				return err
			}
		}
		if err := writeScopes(ctx, q, id, in.Scopes); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{Action: "PROMOTION_CREATED", Entity: "promotion", EntityID: id.String(), New: in})
	})
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

// Update replaces everything except the code and type (changing the type changes what the promotion IS; create a new one).
func (s *Service) Update(ctx context.Context, id uuid.UUID, in Input) (*Detail, error) {
	p := access.MustPrincipal(ctx)
	if err := access.RequireAll(p, Perm); err != nil {
		return nil, err
	}
	if err := normalize(&in); err != nil {
		return nil, err
	}
	err := s.tx.Do(ctx, func(q *gen.Queries) error {
		cur, err := q.GetPromotion(ctx, gen.GetPromotionParams{ID: id, CompanyID: p.CompanyID})
		if errors.Is(err, pgx.ErrNoRows) {
			return apperr.NotFound("promotion")
		}
		if err != nil {
			return err
		}
		if _, err := ValidateConfig(cur.Type, in.Config); err != nil {
			return err
		}
		if _, err := q.UpdatePromotion(ctx, gen.UpdatePromotionParams{ID: id, CompanyID: p.CompanyID, Name: in.Name, Description: in.Description, Config: in.Config, Priority: in.Priority,
			Stackable: *in.Stackable, Exclusive: in.Exclusive, StartsAt: in.StartsAt, EndsAt: in.EndsAt, DaysOfWeek: in.DaysOfWeek, TimeFrom: in.TimeFrom, TimeTo: in.TimeTo, Status: in.Status}); err != nil {
			return err
		}
		if err := writeScopes(ctx, q, id, in.Scopes); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{Action: "PROMOTION_UPDATED", Entity: "promotion", EntityID: id.String(), New: in})
	})
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

// Load returns the promotions that can apply in a store right now, in engine form. Used by the POS.
func Load(ctx context.Context, q *gen.Queries, companyID, storeOrgUnitID uuid.UUID, at time.Time) ([]Promotion, error) {
	return LoadFor(ctx, q, companyID, storeOrgUnitID, at, false)
}

// LoadFor is Load; with includeFuture it also returns promotions that start after `at` (the offline snapshot is
// used for hours or days, so what begins tomorrow must already be in it; the engine re-checks the window per sale).
func LoadFor(ctx context.Context, q *gen.Queries, companyID, storeOrgUnitID uuid.UUID, at time.Time, includeFuture bool) ([]Promotion, error) {
	var rows []gen.Promotion
	var err error
	if includeFuture {
		rows, err = q.SnapshotPromotions(ctx, gen.SnapshotPromotionsParams{CompanyID: companyID, StoreOrgUnitID: storeOrgUnitID, At: at})
	} else {
		rows, err = q.ListActivePromotions(ctx, gen.ListActivePromotionsParams{CompanyID: companyID, StoreOrgUnitID: storeOrgUnitID, At: at})
	}
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	ids := make([]uuid.UUID, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	scopes, err := q.ListPromotionScopes(ctx, ids)
	if err != nil {
		return nil, err
	}
	byPromo := map[uuid.UUID]*Scopes{}
	for _, sc := range scopes {
		s := byPromo[sc.PromotionID]
		if s == nil {
			s = &Scopes{Products: map[uuid.UUID]bool{}, Categories: map[uuid.UUID]bool{}, Brands: map[uuid.UUID]bool{}, Levels: map[uuid.UUID]bool{}}
			byPromo[sc.PromotionID] = s
		}
		switch sc.ScopeType {
		case "product":
			s.Products[sc.ScopeID] = true
		case "category":
			s.Categories[sc.ScopeID] = true
		case "brand":
			s.Brands[sc.ScopeID] = true
		case "level":
			s.Levels[sc.ScopeID] = true
		}
	}
	out := make([]Promotion, 0, len(rows))
	for _, r := range rows {
		var cfg Config
		if err := json.Unmarshal(r.Config, &cfg); err != nil {
			continue // a corrupt promotion must never block selling
		}
		p := Promotion{ID: r.ID, Code: r.Code, Name: r.Name, Type: Type(r.Type), Config: cfg, Priority: int(r.Priority), Stackable: r.Stackable, Exclusive: r.Exclusive,
			StartsAt: r.StartsAt, EndsAt: r.EndsAt}
		for _, d := range r.DaysOfWeek {
			p.DaysOfWeek = append(p.DaysOfWeek, int(d))
		}
		if r.TimeFrom != nil && r.TimeTo != nil {
			p.TimeFrom, p.TimeTo = *r.TimeFrom, *r.TimeTo
		}
		if s := byPromo[r.ID]; s != nil {
			p.Scopes = *s
		}
		out = append(out, p)
	}
	return out, nil
}
