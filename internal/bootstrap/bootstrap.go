// Package bootstrap seeds permissions and creates a company with system roles and an owner.
package bootstrap

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/ritel35k/backend/db/gen"
	"github.com/ritel35k/backend/internal/access"
	"github.com/ritel35k/backend/internal/auth"
	"github.com/ritel35k/backend/internal/organization"
	"github.com/ritel35k/backend/internal/platform/tx"
)

// SyncPermissions upserts the permission catalog and additively tops up every company's system roles
// to the code-defined baseline, so new permissions reach existing companies. Safe to run on every start.
func SyncPermissions(ctx context.Context, pool *pgxpool.Pool) error {
	return tx.NewManager(pool).Do(ctx, func(q *gen.Queries) error {
		for _, d := range access.Catalog {
			if err := q.UpsertPermission(ctx, gen.UpsertPermissionParams{Code: d.Code, Description: d.Description}); err != nil {
				return err
			}
		}
		ids, err := q.ListCompanyIDs(ctx)
		if err != nil {
			return err
		}
		for _, cid := range ids {
			for _, dr := range access.DefaultRoles {
				r, err := q.GetRoleByCode(ctx, gen.GetRoleByCodeParams{CompanyID: cid, Code: dr.Code})
				if errors.Is(err, pgx.ErrNoRows) {
					continue
				}
				if err != nil {
					return err
				}
				if !r.IsSystem {
					continue
				}
				// System roles only ever gain permissions here (additive), so new features reach existing companies.
				for _, perm := range access.Expand(dr.Patterns) {
					if err := q.AddRolePermission(ctx, gen.AddRolePermissionParams{RoleID: r.ID, PermissionCode: perm}); err != nil {
						return err
					}
				}
			}
		}
		return nil
	})
}

type Input struct {
	CompanyCode   string
	CompanyName   string
	OwnerEmail    string
	OwnerName     string
	OwnerPassword string
}

type Result struct {
	CompanyID  uuid.UUID
	RootUnitID uuid.UUID
	OwnerID    uuid.UUID
}

// CreateCompany creates the company, its root org unit, the system roles, default UOMs and an Owner
// with company-wide scope, atomically.
func CreateCompany(ctx context.Context, pool *pgxpool.Pool, in Input) (*Result, error) {
	if !organization.ValidCode(in.CompanyCode) {
		return nil, fmt.Errorf("invalid company code %q", in.CompanyCode)
	}
	if len(in.OwnerPassword) < 8 {
		return nil, errors.New("owner password must be at least 8 characters")
	}
	if err := SyncPermissions(ctx, pool); err != nil {
		return nil, err
	}
	ph, err := auth.HashSecret(in.OwnerPassword)
	if err != nil {
		return nil, err
	}
	var res Result
	err = tx.NewManager(pool).Do(ctx, func(q *gen.Queries) error {
		cid, _ := uuid.NewV7()
		if _, err := q.CreateCompany(ctx, gen.CreateCompanyParams{ID: cid, Code: in.CompanyCode, Name: in.CompanyName}); err != nil {
			return err
		}
		rid, _ := uuid.NewV7()
		if _, err := q.CreateOrgUnit(ctx, gen.CreateOrgUnitParams{ID: rid, CompanyID: cid, Type: "company", Code: in.CompanyCode, Name: in.CompanyName,
			Path: organization.PathLabel(in.CompanyCode)}); err != nil {
			return err
		}
		var ownerRole uuid.UUID
		for _, dr := range access.DefaultRoles {
			id, _ := uuid.NewV7()
			if _, err := q.CreateRole(ctx, gen.CreateRoleParams{ID: id, CompanyID: cid, Code: dr.Code, Name: dr.Name, IsSystem: true}); err != nil {
				return err
			}
			for _, perm := range access.Expand(dr.Patterns) {
				if err := q.AddRolePermission(ctx, gen.AddRolePermissionParams{RoleID: id, PermissionCode: perm}); err != nil {
					return err
				}
			}
			if dr.Code == "owner" {
				ownerRole = id
			}
		}
		plID, _ := uuid.NewV7()
		if _, err := q.CreateDefaultPriceList(ctx, gen.CreateDefaultPriceListParams{ID: plID, CompanyID: cid}); err != nil {
			return err
		}
		for _, l := range []struct {
			code, name string
			rank       int32
			spend      int64
			mult       string
		}{{"REGULAR", "Regular", 0, 0, "1.00"}, {"SILVER", "Silver", 1, 5000000, "1.25"}, {"GOLD", "Gold", 2, 20000000, "1.50"}, {"PLATINUM", "Platinum", 3, 50000000, "2.00"}} {
			lid, _ := uuid.NewV7()
			if _, err := q.CreateLevel(ctx, gen.CreateLevelParams{ID: lid, CompanyID: cid, Code: l.code, Name: l.name, Rank: l.rank, MinSpend: decimal.NewFromInt(l.spend),
				EarnMultiplier: decimal.RequireFromString(l.mult), IsDefault: l.rank == 0}); err != nil {
				return err
			}
		}
		uid, _ := uuid.NewV7()
		if _, err := q.CreateUser(ctx, gen.CreateUserParams{ID: uid, CompanyID: cid, Email: in.OwnerEmail, Name: in.OwnerName, PasswordHash: ph}); err != nil {
			return err
		}
		sid, _ := uuid.NewV7()
		if err := q.AddUserRoleScope(ctx, gen.AddUserRoleScopeParams{ID: sid, UserID: uid, RoleID: ownerRole, OrgUnitID: nil, CreatedBy: &uid}); err != nil {
			return err
		}
		for _, u := range []struct {
			code, name string
			dec        bool
		}{{"PCS", "Pieces", false}, {"BOX", "Box", false}, {"PACK", "Pack", false}, {"KG", "Kilogram", true}, {"GR", "Gram", true}, {"LTR", "Liter", true}} {
			id, _ := uuid.NewV7()
			if _, err := q.CreateUOM(ctx, gen.CreateUOMParams{ID: id, CompanyID: cid, Code: u.code, Name: u.name, AllowsDecimal: u.dec, CreatedBy: &uid}); err != nil {
				return err
			}
		}
		res = Result{CompanyID: cid, RootUnitID: rid, OwnerID: uid}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &res, nil
}
