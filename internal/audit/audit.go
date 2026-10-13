// Package audit writes append-only audit records inside the caller's transaction.
package audit

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"

	"github.com/ritel35k/backend/db/gen"
	"github.com/ritel35k/backend/internal/access"
	"github.com/ritel35k/backend/internal/platform/reqmeta"
)

type Entry struct {
	CompanyID uuid.UUID // optional when a Principal is in ctx
	UserID    *uuid.UUID
	StoreID   *uuid.UUID
	Action    string
	Entity    string
	EntityID  string
	Old, New  any
}

// Standard actions.
const (
	StoreCreated          = "STORE_CREATED"
	StoreUpdated          = "STORE_UPDATED"
	OrgUnitCreated        = "ORG_UNIT_CREATED"
	SettingChanged        = "SETTING_CHANGED"
	SettingReverted       = "SETTING_REVERTED"
	UserCreated           = "USER_CREATED"
	UserUpdated           = "USER_UPDATED"
	UserPermissionChanged = "USER_PERMISSION_CHANGED"
	RoleCreated           = "ROLE_CREATED"
	RolePermissionChanged = "ROLE_PERMISSION_CHANGED"
	PasswordChanged       = "PASSWORD_CHANGED"
	PINChanged            = "PIN_CHANGED"
	PINApproval           = "PIN_APPROVAL"
	LoginSucceeded        = "LOGIN_SUCCEEDED"
	LoginFailed           = "LOGIN_FAILED"
	RefreshReuse          = "REFRESH_TOKEN_REUSE"
	ProductCreated        = "PRODUCT_CREATED"
	ProductUpdated        = "PRODUCT_UPDATED"
	SupplierCreated       = "SUPPLIER_CREATED"
	SupplierUpdated       = "SUPPLIER_UPDATED"
	WarehouseCreated      = "WAREHOUSE_CREATED"
	WarehouseUpdated      = "WAREHOUSE_UPDATED"
	CatalogChanged        = "CATALOG_CHANGED"
)

func Record(ctx context.Context, q *gen.Queries, e Entry) error {
	if p, ok := access.PrincipalFrom(ctx); ok {
		if e.CompanyID == uuid.Nil {
			e.CompanyID = p.CompanyID
		}
		if e.UserID == nil {
			uid := p.UserID
			e.UserID = &uid
		}
	}
	m := reqmeta.From(ctx)
	id, err := uuid.NewV7()
	if err != nil {
		return err
	}
	return q.InsertAuditLog(ctx, gen.InsertAuditLogParams{
		ID: id, CompanyID: e.CompanyID, StoreID: e.StoreID, UserID: e.UserID, Action: e.Action, Entity: e.Entity,
		EntityID: e.EntityID, OldValue: marshal(e.Old), NewValue: marshal(e.New),
		IpAddress: m.IP, UserAgent: m.UserAgent, DeviceID: m.DeviceID, RequestID: m.RequestID,
	})
}

func marshal(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}
