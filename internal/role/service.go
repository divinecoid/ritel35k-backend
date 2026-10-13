// Package role manages roles and their permission sets (company-wide concern).
package role

import (
	"context"
	"errors"
	"regexp"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ritel35k/backend/db/gen"
	"github.com/ritel35k/backend/internal/access"
	"github.com/ritel35k/backend/internal/audit"
	"github.com/ritel35k/backend/internal/platform/tx"
	"github.com/ritel35k/backend/pkg/apperr"
)

var codeRe = regexp.MustCompile(`^[a-z][a-z0-9_]{1,39}$`)

type Service struct {
	tx  *tx.Manager
	inv access.Invalidator
}

func NewService(t *tx.Manager, inv access.Invalidator) *Service { return &Service{tx: t, inv: inv} }

type Role struct {
	ID          uuid.UUID `json:"id"`
	Code        string    `json:"code"`
	Name        string    `json:"name"`
	IsSystem    bool      `json:"is_system"`
	Permissions []string  `json:"permissions"`
}

func (s *Service) List(ctx context.Context) ([]Role, error) {
	p := access.MustPrincipal(ctx)
	q := s.tx.Queries()
	rows, err := q.ListRoles(ctx, p.CompanyID)
	if err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	perms, err := q.ListRolePermissions(ctx, ids)
	if err != nil {
		return nil, err
	}
	by := map[uuid.UUID][]string{}
	for _, rp := range perms {
		by[rp.RoleID] = append(by[rp.RoleID], rp.PermissionCode)
	}
	out := make([]Role, len(rows))
	for i, r := range rows {
		ps := by[r.ID]
		if ps == nil {
			ps = []string{}
		}
		out[i] = Role{ID: r.ID, Code: r.Code, Name: r.Name, IsSystem: r.IsSystem, Permissions: ps}
	}
	return out, nil
}

func (s *Service) Permissions(ctx context.Context) ([]gen.Permission, error) {
	return s.tx.Queries().ListPermissions(ctx)
}

func validatePerms(perms []string) error {
	for _, c := range perms {
		if !access.IsKnown(c) {
			return apperr.Validation(map[string]string{"permissions": "unknown permission " + c})
		}
	}
	return nil
}

func (s *Service) Create(ctx context.Context, code, name string, perms []string) (*Role, error) {
	p := access.MustPrincipal(ctx)
	if err := access.RequireAll(p, access.RoleManage); err != nil {
		return nil, err
	}
	if !codeRe.MatchString(code) {
		return nil, apperr.Validation(map[string]string{"code": "lowercase letters, digits, _ (2-40)"})
	}
	if err := validatePerms(perms); err != nil {
		return nil, err
	}
	var out *Role
	err := s.tx.Do(ctx, func(q *gen.Queries) error {
		id, _ := uuid.NewV7()
		r, err := q.CreateRole(ctx, gen.CreateRoleParams{ID: id, CompanyID: p.CompanyID, Code: code, Name: name})
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "23505" {
			return apperr.Conflict("ROLE_CODE_TAKEN", "Role code already exists")
		}
		if err != nil {
			return err
		}
		for _, c := range perms {
			if err := q.AddRolePermission(ctx, gen.AddRolePermissionParams{RoleID: r.ID, PermissionCode: c}); err != nil {
				return err
			}
		}
		out = &Role{ID: r.ID, Code: r.Code, Name: r.Name, Permissions: append([]string{}, perms...)}
		return audit.Record(ctx, q, audit.Entry{Action: audit.RoleCreated, Entity: "role", EntityID: r.ID.String(), New: out})
	})
	return out, err
}

// SetPermissions replaces the permission set of a custom role. System roles are immutable
// (clone them into a custom role instead) so the shipped baseline stays predictable.
func (s *Service) SetPermissions(ctx context.Context, id uuid.UUID, name *string, perms []string) error {
	p := access.MustPrincipal(ctx)
	if err := access.RequireAll(p, access.RoleManage); err != nil {
		return err
	}
	if err := validatePerms(perms); err != nil {
		return err
	}
	err := s.tx.Do(ctx, func(q *gen.Queries) error {
		r, err := q.GetRole(ctx, gen.GetRoleParams{ID: id, CompanyID: p.CompanyID})
		if errors.Is(err, pgx.ErrNoRows) {
			return apperr.NotFound("role")
		}
		if err != nil {
			return err
		}
		if r.IsSystem {
			return apperr.Conflict("ROLE_IMMUTABLE", "System roles cannot be edited; create a custom role instead")
		}
		if name != nil {
			if _, err := q.UpdateRole(ctx, gen.UpdateRoleParams{ID: id, CompanyID: p.CompanyID, Name: *name}); err != nil {
				return err
			}
		}
		if err := q.ClearRolePermissions(ctx, id); err != nil {
			return err
		}
		for _, c := range perms {
			if err := q.AddRolePermission(ctx, gen.AddRolePermissionParams{RoleID: id, PermissionCode: c}); err != nil {
				return err
			}
		}
		return audit.Record(ctx, q, audit.Entry{Action: audit.RolePermissionChanged, Entity: "role", EntityID: id.String(), New: perms})
	})
	if err == nil {
		s.inv.InvalidateAll()
	}
	return err
}
