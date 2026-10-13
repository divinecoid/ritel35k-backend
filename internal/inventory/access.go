package inventory

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/ritel35k/backend/db/gen"
	"github.com/ritel35k/backend/internal/access"
	"github.com/ritel35k/backend/pkg/apperr"
)

// WarehouseFor loads a warehouse and checks the caller holds perm over it: store warehouses are
// checked against the store's org path, central warehouses (no store) need the permission company-wide.
// It returns the store's org path ("" for central).
func WarehouseFor(ctx context.Context, q *gen.Queries, p *access.Principal, warehouseID uuid.UUID, perm string) (gen.Warehouse, string, error) {
	wh, err := q.GetWarehouse(ctx, gen.GetWarehouseParams{ID: warehouseID, CompanyID: p.CompanyID})
	if errors.Is(err, pgx.ErrNoRows) {
		return wh, "", apperr.Validation(map[string]string{"warehouse_id": "unknown warehouse"})
	}
	if err != nil {
		return wh, "", err
	}
	if wh.Status != "active" {
		return wh, "", apperr.Conflict("WAREHOUSE_INACTIVE", "Warehouse is inactive")
	}
	if wh.StoreID == nil {
		if !p.Grants.Scope(perm).All {
			return wh, "", apperr.Forbidden("company-wide permission required for central warehouses").WithDetails(map[string]string{"permission": perm})
		}
		return wh, "", nil
	}
	st, err := q.GetStore(ctx, gen.GetStoreParams{ID: *wh.StoreID, CompanyID: p.CompanyID})
	if err != nil {
		return wh, "", err
	}
	if !p.Grants.Can(perm, st.Path) {
		return wh, "", apperr.Forbidden("not allowed for this warehouse").WithDetails(map[string]string{"permission": perm})
	}
	return wh, st.Path, nil
}

// CanAt reports whether perm applies at a path ("" = central/company-wide).
func CanAt(p *access.Principal, perm, path string) bool {
	if path == "" {
		return p.Grants.Scope(perm).All
	}
	return p.Grants.Can(perm, path)
}

// InTransit adjusts the in-transit quantity of a balance (goods shipped to this warehouse, not yet received).
func InTransit(ctx context.Context, q *gen.Queries, companyID uuid.UUID, storeID *uuid.UUID, warehouseID, variantID uuid.UUID, delta decimal.Decimal) error {
	return q.AddInTransit(ctx, gen.AddInTransitParams{CompanyID: companyID, StoreID: storeID, WarehouseID: warehouseID, VariantID: variantID, Delta: delta})
}

const (
	PurchaseReturn = "PURCHASE_RETURN"
	TransferOut    = "TRANSFER_OUT"
	TransferIn     = "TRANSFER_IN"
	Opname         = "OPNAME"
)
