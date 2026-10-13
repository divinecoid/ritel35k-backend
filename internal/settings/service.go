package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ritel35k/backend/db/gen"
	"github.com/ritel35k/backend/internal/access"
	"github.com/ritel35k/backend/internal/audit"
	"github.com/ritel35k/backend/internal/platform/tx"
	"github.com/ritel35k/backend/pkg/apperr"
)

type Service struct{ tx *tx.Manager }

func NewService(t *tx.Manager) *Service { return &Service{tx: t} }

// Get resolves one key for an org unit: nearest ancestor override, else registry default.
func Get(ctx context.Context, q *gen.Queries, companyID, orgUnitID uuid.UUID, key string) (json.RawMessage, error) {
	def, ok := Lookup(key)
	if !ok {
		return nil, fmt.Errorf("unknown setting %q", key)
	}
	raw, err := q.ResolveSetting(ctx, gen.ResolveSettingParams{CompanyID: companyID, OrgUnitID: orgUnitID, Key: key})
	if errors.Is(err, pgx.ErrNoRows) {
		return json.Marshal(def.Default)
	}
	return raw, err
}

func GetString(ctx context.Context, q *gen.Queries, companyID, orgUnitID uuid.UUID, key string) (string, error) {
	raw, err := Get(ctx, q, companyID, orgUnitID, key)
	if err != nil {
		return "", err
	}
	var s string
	return s, json.Unmarshal(raw, &s)
}

func GetBool(ctx context.Context, q *gen.Queries, companyID, orgUnitID uuid.UUID, key string) (bool, error) {
	raw, err := Get(ctx, q, companyID, orgUnitID, key)
	if err != nil {
		return false, err
	}
	var b bool
	return b, json.Unmarshal(raw, &b)
}

type EffectiveSetting struct {
	Def
	Value        json.RawMessage `json:"value"`
	Source       string          `json:"source"` // default | company | business_unit | region | store
	SourceUnitID *uuid.UUID      `json:"source_org_unit_id,omitempty"`
	SourceName   string          `json:"source_name,omitempty"`
	Inherited    bool            `json:"inherited"` // true when value does not come from this exact unit
}

// Effective returns every known key with its resolved value and where it came from.
func (s *Service) Effective(ctx context.Context, orgUnitID uuid.UUID) ([]EffectiveSetting, error) {
	p := access.MustPrincipal(ctx)
	q := s.tx.Queries()
	if err := s.authorizeUnit(ctx, q, p, access.SettingsView, orgUnitID); err != nil {
		return nil, err
	}
	rows, err := q.ResolveAllSettings(ctx, gen.ResolveAllSettingsParams{CompanyID: p.CompanyID, OrgUnitID: orgUnitID})
	if err != nil {
		return nil, err
	}
	over := make(map[string]gen.ResolveAllSettingsRow, len(rows))
	for _, r := range rows {
		over[r.Key] = r
	}
	out := make([]EffectiveSetting, 0, len(Registry))
	for _, d := range Registry {
		e := EffectiveSetting{Def: d, Source: "default", Inherited: true}
		if r, ok := over[d.Key]; ok {
			id := r.SourceOrgUnitID
			e.Value, e.Source, e.SourceUnitID, e.SourceName = r.Value, r.SourceType, &id, r.SourceName
			e.Inherited = id != orgUnitID
		} else {
			e.Value, _ = json.Marshal(d.Default)
		}
		out = append(out, e)
	}
	return out, nil
}

func (s *Service) Set(ctx context.Context, orgUnitID uuid.UUID, key string, value json.RawMessage) error {
	p := access.MustPrincipal(ctx)
	def, ok := Lookup(key)
	if !ok {
		return apperr.Validation(map[string]string{"key": "unknown setting"})
	}
	norm, err := Validate(def, value)
	if err != nil {
		return apperr.Validation(map[string]string{"value": err.Error()})
	}
	return s.tx.Do(ctx, func(q *gen.Queries) error {
		if err := s.authorizeUnit(ctx, q, p, access.SettingsManage, orgUnitID); err != nil {
			return err
		}
		return Apply(ctx, q, p.CompanyID, orgUnitID, key, norm, &p.UserID)
	})
}

// Apply upserts an override and audits it. Callers are responsible for authorization.
func Apply(ctx context.Context, q *gen.Queries, companyID, orgUnitID uuid.UUID, key string, value json.RawMessage, by *uuid.UUID) error {
	id, err := uuid.NewV7()
	if err != nil {
		return err
	}
	if _, err := q.UpsertSetting(ctx, gen.UpsertSettingParams{
		ID: id, CompanyID: companyID, OrgUnitID: orgUnitID, Key: key, Value: value, UpdatedBy: by,
	}); err != nil {
		return err
	}
	return audit.Record(ctx, q, audit.Entry{CompanyID: companyID, UserID: by, Action: audit.SettingChanged, Entity: "setting",
		EntityID: orgUnitID.String() + ":" + key, New: map[string]any{"key": key, "value": value, "org_unit_id": orgUnitID}})
}

// Unset removes the override at this unit so it falls back to the inherited value.
func (s *Service) Unset(ctx context.Context, orgUnitID uuid.UUID, key string) error {
	p := access.MustPrincipal(ctx)
	if _, ok := Lookup(key); !ok {
		return apperr.Validation(map[string]string{"key": "unknown setting"})
	}
	return s.tx.Do(ctx, func(q *gen.Queries) error {
		if err := s.authorizeUnit(ctx, q, p, access.SettingsManage, orgUnitID); err != nil {
			return err
		}
		n, err := q.DeleteSetting(ctx, gen.DeleteSettingParams{OrgUnitID: orgUnitID, Key: key, CompanyID: p.CompanyID})
		if err != nil {
			return err
		}
		if n == 0 {
			return apperr.NotFound("override")
		}
		return audit.Record(ctx, q, audit.Entry{Action: audit.SettingReverted, Entity: "setting", EntityID: orgUnitID.String() + ":" + key})
	})
}

func (s *Service) authorizeUnit(ctx context.Context, q *gen.Queries, p *access.Principal, perm string, orgUnitID uuid.UUID) error {
	u, err := q.GetOrgUnit(ctx, gen.GetOrgUnitParams{ID: orgUnitID, CompanyID: p.CompanyID})
	if errors.Is(err, pgx.ErrNoRows) {
		return apperr.NotFound("org unit")
	}
	if err != nil {
		return err
	}
	if !p.Grants.Can(perm, u.Path) {
		return apperr.Forbidden("not allowed for this part of the organization")
	}
	return nil
}
