// Package user manages user accounts and their (role, org scope) assignments.
package user

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ritel35k/backend/db/gen"
	"github.com/ritel35k/backend/internal/access"
	"github.com/ritel35k/backend/internal/audit"
	"github.com/ritel35k/backend/internal/auth"
	"github.com/ritel35k/backend/internal/platform/tx"
	"github.com/ritel35k/backend/pkg/apperr"
	"github.com/ritel35k/backend/pkg/pagination"
)

type Service struct {
	tx  *tx.Manager
	inv access.Invalidator
}

func NewService(t *tx.Manager, inv access.Invalidator) *Service { return &Service{tx: t, inv: inv} }

// Assignment grants a role over an org subtree. OrgUnitID nil = whole company.
type Assignment struct {
	RoleID    uuid.UUID  `json:"role_id" validate:"required"`
	OrgUnitID *uuid.UUID `json:"org_unit_id"`
}

type AssignmentView struct {
	RoleID      uuid.UUID  `json:"role_id"`
	RoleCode    string     `json:"role_code"`
	RoleName    string     `json:"role_name"`
	OrgUnitID   *uuid.UUID `json:"org_unit_id"`
	OrgUnitName *string    `json:"org_unit_name"`
	OrgUnitType *string    `json:"org_unit_type"`
}

type User struct {
	ID          uuid.UUID        `json:"id"`
	Email       string           `json:"email"`
	Name        string           `json:"name"`
	Status      string           `json:"status"`
	HasPIN      bool             `json:"has_pin"`
	LastLoginAt *time.Time       `json:"last_login_at"`
	CreatedAt   time.Time        `json:"created_at"`
	Assignments []AssignmentView `json:"assignments"`
}

func toUser(u gen.User) User {
	return User{ID: u.ID, Email: u.Email, Name: u.Name, Status: u.Status, HasPIN: u.PinHash != nil, LastLoginAt: u.LastLoginAt,
		CreatedAt: u.CreatedAt, Assignments: []AssignmentView{}}
}

func (s *Service) attach(ctx context.Context, q *gen.Queries, users []User) error {
	if len(users) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, len(users))
	idx := map[uuid.UUID]int{}
	for i, u := range users {
		ids[i] = u.ID
		idx[u.ID] = i
	}
	rows, err := q.ListUserRoleScopes(ctx, ids)
	if err != nil {
		return err
	}
	for _, r := range rows {
		i := idx[r.UserID]
		users[i].Assignments = append(users[i].Assignments, AssignmentView{RoleID: r.RoleID, RoleCode: r.RoleCode, RoleName: r.RoleName,
			OrgUnitID: r.OrgUnitID, OrgUnitName: r.OrgUnitName, OrgUnitType: r.OrgUnitType})
	}
	return nil
}

func (s *Service) List(ctx context.Context, qstr string, pg pagination.Params) ([]User, pagination.Meta, error) {
	p := access.MustPrincipal(ctx)
	all, paths := p.Grants.Scope(access.UserView).SQL()
	q := s.tx.Queries()
	rows, err := q.ListUsers(ctx, gen.ListUsersParams{CompanyID: p.CompanyID, Q: qstr, AllScope: all, ScopePaths: paths, PageLimit: pg.Limit(), PageOffset: pg.Offset()})
	if err != nil {
		return nil, pagination.Meta{}, err
	}
	total, err := q.CountUsers(ctx, gen.CountUsersParams{CompanyID: p.CompanyID, Q: qstr, AllScope: all, ScopePaths: paths})
	if err != nil {
		return nil, pagination.Meta{}, err
	}
	out := make([]User, len(rows))
	for i, r := range rows {
		out[i] = toUser(r)
	}
	if err := s.attach(ctx, q, out); err != nil {
		return nil, pagination.Meta{}, err
	}
	return out, pagination.NewMeta(pg, total), nil
}

// canManage: caller manages the target if every scope the target holds lies within the caller's
// user.manage scope. A target with no assignments is only manageable company-wide.
func canManage(ctx context.Context, q *gen.Queries, p *access.Principal, targetID uuid.UUID) (bool, error) {
	sc := p.Grants.Scope(access.UserManage)
	if sc.All {
		return true, nil
	}
	paths, err := q.ListUserScopePaths(ctx, targetID)
	if err != nil {
		return false, err
	}
	if len(paths) == 0 {
		return false, nil
	}
	for _, sp := range paths {
		if sp == "" || !sc.Allows(sp) {
			return false, nil
		}
	}
	return true, nil
}

// checkDelegation enforces "you cannot grant more than you have": for each permission of the role,
// the caller must hold it over the requested scope.
func (s *Service) checkDelegation(ctx context.Context, q *gen.Queries, p *access.Principal, a Assignment) error {
	var scopePath string // '' = company
	if a.OrgUnitID != nil {
		u, err := q.GetOrgUnit(ctx, gen.GetOrgUnitParams{ID: *a.OrgUnitID, CompanyID: p.CompanyID})
		if errors.Is(err, pgx.ErrNoRows) {
			return apperr.Validation(map[string]string{"org_unit_id": "unknown org unit"})
		}
		if err != nil {
			return err
		}
		scopePath = u.Path
	}
	if _, err := q.GetRole(ctx, gen.GetRoleParams{ID: a.RoleID, CompanyID: p.CompanyID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apperr.Validation(map[string]string{"role_id": "unknown role"})
		}
		return err
	}
	perms, err := q.ListRolePermissions(ctx, []uuid.UUID{a.RoleID})
	if err != nil {
		return err
	}
	if um := p.Grants.Scope(access.UserManage); scopePath == "" && !um.All || scopePath != "" && !um.Allows(scopePath) {
		return apperr.Forbidden("not allowed to assign users in this scope")
	}
	for _, rp := range perms {
		sc := p.Grants.Scope(rp.PermissionCode)
		if scopePath == "" && !sc.All || scopePath != "" && !sc.Allows(scopePath) {
			return apperr.Forbidden("cannot grant permissions you do not hold").WithDetails(map[string]string{"permission": rp.PermissionCode})
		}
	}
	return nil
}

type CreateInput struct {
	Email       string
	Name        string
	Password    string
	PIN         string
	Assignments []Assignment
}

func (s *Service) Create(ctx context.Context, in CreateInput) (*User, error) {
	p := access.MustPrincipal(ctx)
	if len(in.Password) < 8 {
		return nil, apperr.Validation(map[string]string{"password": "min=8"})
	}
	if len(in.Assignments) == 0 && !p.Grants.Scope(access.UserManage).All {
		return nil, apperr.Validation(map[string]string{"assignments": "at least one assignment is required"})
	}
	ph, err := auth.HashSecret(in.Password)
	if err != nil {
		return nil, err
	}
	var pinHash *string
	if in.PIN != "" {
		if len(in.PIN) < 4 || len(in.PIN) > 8 {
			return nil, apperr.Validation(map[string]string{"pin": "4-8 digits"})
		}
		h, err := auth.HashSecret(in.PIN)
		if err != nil {
			return nil, err
		}
		pinHash = &h
	}
	var out User
	err = s.tx.Do(ctx, func(q *gen.Queries) error {
		for _, a := range in.Assignments {
			if err := s.checkDelegation(ctx, q, p, a); err != nil {
				return err
			}
		}
		id, _ := uuid.NewV7()
		u, err := q.CreateUser(ctx, gen.CreateUserParams{ID: id, CompanyID: p.CompanyID, Email: strings.ToLower(strings.TrimSpace(in.Email)),
			Name: in.Name, PasswordHash: ph, PinHash: pinHash, CreatedBy: &p.UserID})
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "23505" {
			return apperr.Conflict("EMAIL_TAKEN", "Email already registered")
		}
		if err != nil {
			return err
		}
		for _, a := range in.Assignments {
			sid, _ := uuid.NewV7()
			if err := q.AddUserRoleScope(ctx, gen.AddUserRoleScopeParams{ID: sid, UserID: u.ID, RoleID: a.RoleID, OrgUnitID: a.OrgUnitID, CreatedBy: &p.UserID}); err != nil {
				return err
			}
		}
		list := []User{toUser(u)}
		if err := s.attach(ctx, q, list); err != nil {
			return err
		}
		out = list[0]
		return audit.Record(ctx, q, audit.Entry{Action: audit.UserCreated, Entity: "user", EntityID: u.ID.String(), New: map[string]any{"email": u.Email, "assignments": in.Assignments}})
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *Service) Get(ctx context.Context, id uuid.UUID) (*User, error) {
	p := access.MustPrincipal(ctx)
	q := s.tx.Queries()
	u, err := q.GetUser(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && u.CompanyID != p.CompanyID) {
		return nil, apperr.NotFound("user")
	}
	if err != nil {
		return nil, err
	}
	paths, err := q.ListUserScopePaths(ctx, id)
	if err != nil {
		return nil, err
	}
	sc := p.Grants.Scope(access.UserView)
	visible := sc.All
	if !visible {
		for _, sp := range paths {
			if sp != "" && sc.Allows(sp) {
				visible = true
			}
		}
	}
	if !visible && id != p.UserID {
		return nil, apperr.NotFound("user")
	}
	out := []User{toUser(u)}
	if err := s.attach(ctx, q, out); err != nil {
		return nil, err
	}
	return &out[0], nil
}

func (s *Service) Update(ctx context.Context, id uuid.UUID, name, status *string) (*User, error) {
	p := access.MustPrincipal(ctx)
	if id == p.UserID && status != nil && *status != "active" {
		return nil, apperr.Conflict("SELF_DEACTIVATE", "You cannot deactivate yourself")
	}
	var out User
	err := s.tx.Do(ctx, func(q *gen.Queries) error {
		cur, err := q.GetUser(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && cur.CompanyID != p.CompanyID) {
			return apperr.NotFound("user")
		}
		if err != nil {
			return err
		}
		ok, err := canManage(ctx, q, p, id)
		if err != nil {
			return err
		}
		if !ok {
			return apperr.NotFound("user")
		}
		u, err := q.UpdateUser(ctx, gen.UpdateUserParams{ID: id, CompanyID: p.CompanyID, Name: name, Status: status})
		if err != nil {
			return err
		}
		if status != nil && *status != "active" {
			if err := q.RevokeUserRefreshTokens(ctx, id); err != nil {
				return err
			}
		}
		out = toUser(u)
		list := []User{out}
		if err := s.attach(ctx, q, list); err != nil {
			return err
		}
		out = list[0]
		return audit.Record(ctx, q, audit.Entry{Action: audit.UserUpdated, Entity: "user", EntityID: id.String(), Old: cur.Status, New: map[string]any{"name": name, "status": status}})
	})
	if err != nil {
		return nil, err
	}
	s.inv.InvalidatePrincipal(id)
	return &out, nil
}

// SetAssignments replaces all of a user's role/scope assignments.
func (s *Service) SetAssignments(ctx context.Context, id uuid.UUID, as []Assignment) (*User, error) {
	p := access.MustPrincipal(ctx)
	var out User
	err := s.tx.Do(ctx, func(q *gen.Queries) error {
		cur, err := q.GetUser(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && cur.CompanyID != p.CompanyID) {
			return apperr.NotFound("user")
		}
		if err != nil {
			return err
		}
		ok, err := canManage(ctx, q, p, id)
		if err != nil {
			return err
		}
		if !ok {
			return apperr.NotFound("user")
		}
		if id == p.UserID {
			return apperr.Conflict("SELF_SCOPE_CHANGE", "You cannot change your own assignments")
		}
		for _, a := range as {
			if err := s.checkDelegation(ctx, q, p, a); err != nil {
				return err
			}
		}
		if err := q.ClearUserRoleScopes(ctx, id); err != nil {
			return err
		}
		for _, a := range as {
			sid, _ := uuid.NewV7()
			if err := q.AddUserRoleScope(ctx, gen.AddUserRoleScopeParams{ID: sid, UserID: id, RoleID: a.RoleID, OrgUnitID: a.OrgUnitID, CreatedBy: &p.UserID}); err != nil {
				return err
			}
		}
		list := []User{toUser(cur)}
		if err := s.attach(ctx, q, list); err != nil {
			return err
		}
		out = list[0]
		return audit.Record(ctx, q, audit.Entry{Action: audit.UserPermissionChanged, Entity: "user", EntityID: id.String(), New: as})
	})
	if err != nil {
		return nil, err
	}
	s.inv.InvalidatePrincipal(id)
	return &out, nil
}
