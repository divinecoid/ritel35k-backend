// Package organization owns the company/region/store tree and store provisioning.
package organization

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/ritel35k/backend/db/gen"
	"github.com/ritel35k/backend/internal/access"
	"github.com/ritel35k/backend/internal/audit"
	"github.com/ritel35k/backend/internal/platform/outbox"
	"github.com/ritel35k/backend/internal/platform/tx"
	"github.com/ritel35k/backend/internal/settings"
	"github.com/ritel35k/backend/pkg/apperr"
)

const EventStoreProvisioned = "StoreProvisioned"

type Service struct{ tx *tx.Manager }

func NewService(t *tx.Manager) *Service { return &Service{tx: t} }

// ---- read side --------------------------------------------------------------------------

type Unit struct {
	ID          uuid.UUID  `json:"id"`
	ParentID    *uuid.UUID `json:"parent_id"`
	Type        string     `json:"type"`
	Code        string     `json:"code"`
	Name        string     `json:"name"`
	Path        string     `json:"path"`
	Status      string     `json:"status"`
	Accessible  bool       `json:"accessible"`
	StoreID     *uuid.UUID `json:"store_id,omitempty"`
	StoreStatus *string    `json:"store_status,omitempty"`
}

// ScopeTree returns the org units the caller can see (their scope plus ancestors for context).
// The frontend builds the Company/Region/Store filter from this — there is no static store list anywhere.
func (s *Service) ScopeTree(ctx context.Context) ([]Unit, error) {
	p := access.MustPrincipal(ctx)
	all, paths := p.Grants.Scope(access.OrganizationView).SQL()
	rows, err := s.tx.Queries().ListOrgUnitsVisible(ctx, gen.ListOrgUnitsVisibleParams{CompanyID: p.CompanyID, AllScope: all, ScopePaths: paths})
	if err != nil {
		return nil, err
	}
	out := make([]Unit, len(rows))
	for i, r := range rows {
		out[i] = Unit{ID: r.ID, ParentID: r.ParentID, Type: r.Type, Code: r.Code, Name: r.Name, Path: r.Path, Status: r.Status,
			Accessible: r.Accessible, StoreID: r.StoreID, StoreStatus: r.StoreStatus}
	}
	return out, nil
}

type Store struct {
	ID             uuid.UUID       `json:"id"`
	OrgUnitID      uuid.UUID       `json:"org_unit_id"`
	ParentUnitID   *uuid.UUID      `json:"parent_org_unit_id"`
	Code           string          `json:"code"`
	Name           string          `json:"name"`
	Path           string          `json:"path"`
	Address        json.RawMessage `json:"address"`
	Timezone       string          `json:"timezone"`
	OperatingHours json.RawMessage `json:"operating_hours"`
	Status         string          `json:"status"`
	CreatedAt      time.Time       `json:"created_at"`
}

type ListStoresFilter struct {
	Status    string
	UnderPath string
}

func (s *Service) ListStores(ctx context.Context, f ListStoresFilter) ([]Store, error) {
	p := access.MustPrincipal(ctx)
	all, paths := p.Grants.Scope(access.OrganizationView).SQL()
	rows, err := s.tx.Queries().ListStores(ctx, gen.ListStoresParams{
		CompanyID: p.CompanyID, AllScope: all, ScopePaths: paths, Status: f.Status, UnderPath: f.UnderPath,
	})
	if err != nil {
		return nil, err
	}
	out := make([]Store, len(rows))
	for i, r := range rows {
		out[i] = Store{ID: r.ID, OrgUnitID: r.OrgUnitID, ParentUnitID: r.ParentOrgUnitID, Code: r.Code, Name: r.Name, Path: r.Path,
			Address: r.Address, Timezone: r.Timezone, OperatingHours: r.OperatingHours, Status: r.Status, CreatedAt: r.CreatedAt}
	}
	return out, nil
}

func (s *Service) GetStore(ctx context.Context, id uuid.UUID) (*Store, error) {
	p := access.MustPrincipal(ctx)
	r, err := s.tx.Queries().GetStore(ctx, gen.GetStoreParams{ID: id, CompanyID: p.CompanyID})
	if err != nil {
		return nil, notFoundOr(err, "store")
	}
	if !p.Grants.Can(access.OrganizationView, r.Path) {
		return nil, apperr.NotFound("store") // don't reveal stores outside scope
	}
	return &Store{ID: r.ID, OrgUnitID: r.OrgUnitID, ParentUnitID: r.ParentOrgUnitID, Code: r.Code, Name: r.Name, Path: r.Path,
		Address: r.Address, Timezone: r.Timezone, OperatingHours: r.OperatingHours, Status: r.Status, CreatedAt: r.CreatedAt}, nil
}

// ---- org units ---------------------------------------------------------------------------

type CreateUnitInput struct {
	ParentID uuid.UUID
	Type     string // business_unit | region
	Code     string
	Name     string
}

func (s *Service) CreateUnit(ctx context.Context, in CreateUnitInput) (*gen.CreateOrgUnitRow, error) {
	p := access.MustPrincipal(ctx)
	if in.Type != "business_unit" && in.Type != "region" {
		return nil, apperr.Validation(map[string]string{"type": "must be business_unit or region"})
	}
	if !ValidCode(in.Code) {
		return nil, apperr.Validation(map[string]string{"code": "2-32 chars: letters, digits, _ or -"})
	}
	var out gen.CreateOrgUnitRow
	err := s.tx.Do(ctx, func(q *gen.Queries) error {
		parent, err := q.GetOrgUnit(ctx, gen.GetOrgUnitParams{ID: in.ParentID, CompanyID: p.CompanyID})
		if err != nil {
			return notFoundOr(err, "parent org unit")
		}
		if !p.Grants.Can(access.OrganizationUnitManage, parent.Path) {
			return apperr.Forbidden("not allowed under this parent")
		}
		if parent.Type == "store" || (parent.Type == "region" && in.Type == "business_unit") {
			return apperr.Validation(map[string]string{"parent_id": "invalid parent for this unit type"})
		}
		id, _ := uuid.NewV7()
		out, err = q.CreateOrgUnit(ctx, gen.CreateOrgUnitParams{
			ID: id, CompanyID: p.CompanyID, ParentID: &parent.ID, Type: in.Type, Code: in.Code, Name: in.Name,
			Path: parent.Path + "." + PathLabel(in.Code),
		})
		if isUniqueViolation(err) {
			return apperr.Conflict("CODE_TAKEN", "Code already used in this company")
		}
		if err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{Action: audit.OrgUnitCreated, Entity: "org_unit", EntityID: id.String(), New: out})
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ---- store provisioning ------------------------------------------------------------------

type ProvisionInput struct {
	Code           string
	Name           string
	ParentUnitID   *uuid.UUID // nil → company root
	Address        json.RawMessage
	Timezone       string
	OperatingHours json.RawMessage
	// Optional per-store setting overrides (everything else is inherited).
	Settings map[string]json.RawMessage
}

type Provisioned struct {
	Store     Store      `json:"store"`
	Warehouse WarehouseV `json:"default_warehouse"`
	Terminal  string     `json:"default_terminal"`
}

type WarehouseV struct {
	ID   uuid.UUID `json:"id"`
	Code string    `json:"code"`
	Name string    `json:"name"`
}

// ProvisionStore creates a fully usable store in ONE transaction: org node, store, default warehouse,
// optional overrides, audit and the StoreProvisioned event. Everything else (prices, tax, payment methods,
// numbering formats, POS rules) is inherited from the parent chain, so no per-store setup is needed.
func (s *Service) ProvisionStore(ctx context.Context, in ProvisionInput) (*Provisioned, error) {
	p := access.MustPrincipal(ctx)
	if !ValidCode(in.Code) {
		return nil, apperr.Validation(map[string]string{"code": "2-32 chars: letters, digits, _ or -"})
	}
	if in.Timezone == "" {
		in.Timezone = "Asia/Jakarta"
	}
	if _, err := time.LoadLocation(in.Timezone); err != nil {
		return nil, apperr.Validation(map[string]string{"timezone": "unknown IANA timezone"})
	}
	if len(in.Address) == 0 {
		in.Address = json.RawMessage(`{}`)
	}
	if len(in.OperatingHours) == 0 {
		in.OperatingHours = json.RawMessage(`{}`)
	}
	norm := make(map[string]json.RawMessage, len(in.Settings))
	for k, v := range in.Settings {
		def, ok := settings.Lookup(k)
		if !ok {
			return nil, apperr.Validation(map[string]string{"settings." + k: "unknown setting"})
		}
		nv, err := settings.Validate(def, v)
		if err != nil {
			return nil, apperr.Validation(map[string]string{"settings." + k: err.Error()})
		}
		norm[k] = nv
	}

	var out Provisioned
	err := s.tx.Do(ctx, func(q *gen.Queries) error {
		var parent gen.GetOrgUnitRow
		var err error
		if in.ParentUnitID != nil {
			parent, err = q.GetOrgUnit(ctx, gen.GetOrgUnitParams{ID: *in.ParentUnitID, CompanyID: p.CompanyID})
		} else {
			var root gen.GetCompanyRootOrgUnitRow
			root, err = q.GetCompanyRootOrgUnit(ctx, p.CompanyID)
			parent = gen.GetOrgUnitRow(root)
		}
		if err != nil {
			return notFoundOr(err, "parent org unit")
		}
		if parent.Type == "store" {
			return apperr.Validation(map[string]string{"parent_org_unit_id": "a store cannot contain a store"})
		}
		if !p.Grants.Can(access.OrganizationStoreCreate, parent.Path) {
			return apperr.Forbidden("not allowed to add stores under this parent")
		}
		if len(norm) > 0 && !p.Grants.Can(access.SettingsManage, parent.Path) {
			return apperr.Forbidden("settings.manage required to set overrides")
		}

		unitID, _ := uuid.NewV7()
		unit, err := q.CreateOrgUnit(ctx, gen.CreateOrgUnitParams{
			ID: unitID, CompanyID: p.CompanyID, ParentID: &parent.ID, Type: "store", Code: in.Code, Name: in.Name,
			Path: parent.Path + "." + PathLabel(in.Code),
		})
		if isUniqueViolation(err) {
			return apperr.Conflict("STORE_CODE_TAKEN", "Store code already in use")
		}
		if err != nil {
			return err
		}
		storeID, _ := uuid.NewV7()
		st, err := q.CreateStore(ctx, gen.CreateStoreParams{
			ID: storeID, CompanyID: p.CompanyID, OrgUnitID: unit.ID, Code: in.Code, Name: in.Name, Address: in.Address,
			Timezone: in.Timezone, OperatingHours: in.OperatingHours, CreatedBy: &p.UserID,
		})
		if isUniqueViolation(err) {
			return apperr.Conflict("STORE_CODE_TAKEN", "Store code already in use")
		}
		if err != nil {
			return err
		}

		for k, v := range norm {
			if err := settings.Apply(ctx, q, p.CompanyID, unit.ID, k, v, &p.UserID); err != nil {
				return err
			}
		}

		whName, err := settings.GetString(ctx, q, p.CompanyID, unit.ID, "inventory.default_warehouse_name")
		if err != nil {
			return err
		}
		whID, _ := uuid.NewV7()
		wh, err := q.CreateWarehouse(ctx, gen.CreateWarehouseParams{
			ID: whID, CompanyID: p.CompanyID, StoreID: &st.ID, Code: "WH-" + in.Code, Name: fmt.Sprintf("%s %s", whName, in.Name),
			Type: "store", IsDefault: true, CreatedBy: &p.UserID,
		})
		if isUniqueViolation(err) {
			return apperr.Conflict("WAREHOUSE_CODE_TAKEN", "Warehouse code already in use")
		}
		if err != nil {
			return err
		}

		tid, _ := uuid.NewV7()
		if _, err := q.CreateTerminal(ctx, gen.CreateTerminalParams{ID: tid, CompanyID: p.CompanyID, StoreID: st.ID, Code: "POS-01", Name: "Kasir 1", CreatedBy: &p.UserID}); err != nil {
			return err
		}

		out = Provisioned{
			Store: Store{ID: st.ID, OrgUnitID: unit.ID, ParentUnitID: unit.ParentID, Code: st.Code, Name: st.Name, Path: unit.Path,
				Address: st.Address, Timezone: st.Timezone, OperatingHours: st.OperatingHours, Status: st.Status, CreatedAt: st.CreatedAt},
			Warehouse: WarehouseV{ID: wh.ID, Code: wh.Code, Name: wh.Name}, Terminal: "POS-01",
		}
		if err := audit.Record(ctx, q, audit.Entry{StoreID: &st.ID, Action: audit.StoreCreated, Entity: "store", EntityID: st.ID.String(), New: out}); err != nil {
			return err
		}
		return outbox.Publish(ctx, q, p.CompanyID, EventStoreProvisioned, "store", st.ID,
			map[string]any{"store_id": st.ID, "org_unit_id": unit.ID, "code": st.Code, "path": unit.Path})
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

type UpdateStoreInput struct {
	Name           *string
	Address        json.RawMessage
	Timezone       *string
	OperatingHours json.RawMessage
	Status         *string
}

func (s *Service) UpdateStore(ctx context.Context, id uuid.UUID, in UpdateStoreInput) (*Store, error) {
	p := access.MustPrincipal(ctx)
	if in.Timezone != nil {
		if _, err := time.LoadLocation(*in.Timezone); err != nil {
			return nil, apperr.Validation(map[string]string{"timezone": "unknown IANA timezone"})
		}
	}
	var out *Store
	err := s.tx.Do(ctx, func(q *gen.Queries) error {
		cur, err := q.GetStore(ctx, gen.GetStoreParams{ID: id, CompanyID: p.CompanyID})
		if err != nil {
			return notFoundOr(err, "store")
		}
		if !p.Grants.Can(access.OrganizationStoreUpdate, cur.Path) {
			return apperr.Forbidden("not allowed to edit this store")
		}
		upd, err := q.UpdateStore(ctx, gen.UpdateStoreParams{
			ID: id, CompanyID: p.CompanyID, Name: in.Name, Address: in.Address, Timezone: in.Timezone,
			OperatingHours: in.OperatingHours, Status: in.Status,
		})
		if err != nil {
			return err
		}
		out = &Store{ID: upd.ID, OrgUnitID: upd.OrgUnitID, ParentUnitID: cur.ParentOrgUnitID, Code: upd.Code, Name: upd.Name, Path: cur.Path,
			Address: upd.Address, Timezone: upd.Timezone, OperatingHours: upd.OperatingHours, Status: upd.Status, CreatedAt: upd.CreatedAt}
		return audit.Record(ctx, q, audit.Entry{StoreID: &id, Action: audit.StoreUpdated, Entity: "store", EntityID: id.String(), Old: cur.Name, New: out})
	})
	return out, err
}
