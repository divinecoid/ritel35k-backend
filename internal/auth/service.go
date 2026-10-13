// Package auth handles login, refresh-token rotation, passwords, PINs and supervisor approvals.
package auth

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ritel35k/backend/db/gen"
	"github.com/ritel35k/backend/internal/access"
	"github.com/ritel35k/backend/internal/audit"
	"github.com/ritel35k/backend/internal/auth/token"
	"github.com/ritel35k/backend/internal/platform/reqmeta"
	"github.com/ritel35k/backend/internal/platform/tx"
	"github.com/ritel35k/backend/pkg/apperr"
)

var pinRe = regexp.MustCompile(`^[0-9]{4,8}$`)

type Service struct {
	tx       *tx.Manager
	tokens   *token.Manager
	refTTL   time.Duration
	throttle *throttle
	cache    *principalCache
	now      func() time.Time
}

func NewService(t *tx.Manager, tm *token.Manager, refreshTTL time.Duration) *Service {
	return &Service{tx: t, tokens: tm, refTTL: refreshTTL, throttle: newThrottle(5, 15*time.Minute),
		cache: newPrincipalCache(10 * time.Second), now: time.Now}
}

// InvalidatePrincipal drops cached grants for a user (call after changing their roles/scopes).
func (s *Service) InvalidatePrincipal(userID uuid.UUID) { s.cache.delete(userID) }

// ---- principal loading ------------------------------------------------------------------

func (s *Service) LoadPrincipal(ctx context.Context, userID, companyID uuid.UUID) (*access.Principal, error) {
	if p := s.cache.get(userID); p != nil && p.CompanyID == companyID {
		return p, nil
	}
	q := s.tx.Queries()
	u, err := q.GetUser(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (u.Status != "active" || u.CompanyID != companyID)) {
		return nil, apperr.Unauthenticated("account unavailable")
	}
	if err != nil {
		return nil, err
	}
	rows, err := q.ListUserGrants(ctx, userID)
	if err != nil {
		return nil, err
	}
	g := access.NewGrants()
	for _, r := range rows {
		if r.ScopePath == "" {
			g.Add(r.PermissionCode, nil)
		} else {
			sp := r.ScopePath
			g.Add(r.PermissionCode, &sp)
		}
	}
	p := &access.Principal{UserID: u.ID, CompanyID: u.CompanyID, Email: u.Email, Name: u.Name, Grants: g}
	s.cache.put(p)
	return p, nil
}

// ---- login / refresh / logout ------------------------------------------------------------

type Tokens struct {
	AccessToken      string    `json:"access_token"`
	AccessExpiresAt  time.Time `json:"access_expires_at"`
	RefreshToken     string    `json:"refresh_token"`
	RefreshExpiresAt time.Time `json:"refresh_expires_at"`
}

type LoginResult struct {
	Tokens
	User UserInfo `json:"user"`
}

type UserInfo struct {
	ID        uuid.UUID `json:"id"`
	CompanyID uuid.UUID `json:"company_id"`
	Email     string    `json:"email"`
	Name      string    `json:"name"`
}

func (s *Service) Login(ctx context.Context, email, password string) (*LoginResult, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	meta := reqmeta.From(ctx)
	key := email + "|" + meta.IP
	if wait := s.throttle.blockedFor(key); wait > 0 {
		return nil, apperr.New(429, "LOGIN_THROTTLED", "Too many failed attempts, try again later")
	}
	q := s.tx.Queries()
	u, err := q.GetUserByEmail(ctx, email)
	if errors.Is(err, pgx.ErrNoRows) {
		VerifySecret(password, dummyHash) // equalize timing
		s.throttle.fail(key)
		return nil, apperr.Unauthenticated("invalid email or password")
	}
	if err != nil {
		return nil, err
	}
	if !VerifySecret(password, u.PasswordHash) || u.Status != "active" {
		s.throttle.fail(key)
		_ = audit.Record(ctx, q, audit.Entry{CompanyID: u.CompanyID, UserID: &u.ID, Action: audit.LoginFailed, Entity: "user", EntityID: u.ID.String()})
		return nil, apperr.Unauthenticated("invalid email or password")
	}
	s.throttle.reset(key)

	var res LoginResult
	err = s.tx.Do(ctx, func(q *gen.Queries) error {
		tk, err := s.issue(ctx, q, u.ID, u.CompanyID, uuid.New())
		if err != nil {
			return err
		}
		res = LoginResult{Tokens: *tk, User: UserInfo{ID: u.ID, CompanyID: u.CompanyID, Email: u.Email, Name: u.Name}}
		if err := q.TouchUserLogin(ctx, u.ID); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{CompanyID: u.CompanyID, UserID: &u.ID, Action: audit.LoginSucceeded, Entity: "user", EntityID: u.ID.String()})
	})
	if err != nil {
		return nil, err
	}
	return &res, nil
}

func (s *Service) issue(ctx context.Context, q *gen.Queries, userID, companyID, family uuid.UUID) (*Tokens, error) {
	now := s.now()
	at, aexp, err := s.tokens.IssueAccess(userID, companyID, now)
	if err != nil {
		return nil, err
	}
	rt, hash := token.NewRefresh()
	rexp := now.Add(s.refTTL)
	m := reqmeta.From(ctx)
	if err := q.CreateRefreshToken(ctx, gen.CreateRefreshTokenParams{
		ID: uuid.New(), UserID: userID, FamilyID: family, TokenHash: hash, ExpiresAt: rexp, UserAgent: m.UserAgent, Ip: m.IP,
	}); err != nil {
		return nil, err
	}
	return &Tokens{AccessToken: at, AccessExpiresAt: aexp, RefreshToken: rt, RefreshExpiresAt: rexp}, nil
}

// Refresh rotates the refresh token. Presenting an already-rotated token revokes the whole family
// (token theft detection).
func (s *Service) Refresh(ctx context.Context, refreshToken string) (*Tokens, error) {
	var out *Tokens
	var reuse *apperr.Error
	err := s.tx.Do(ctx, func(q *gen.Queries) error {
		rt, err := q.GetRefreshTokenForUpdate(ctx, token.HashRefresh(refreshToken))
		if errors.Is(err, pgx.ErrNoRows) {
			return apperr.Unauthenticated("invalid refresh token")
		}
		if err != nil {
			return err
		}
		if rt.RevokedAt != nil {
			if err := q.RevokeRefreshFamily(ctx, rt.FamilyID); err != nil {
				return err
			}
			_ = audit.Record(ctx, q, audit.Entry{UserID: &rt.UserID, Action: audit.RefreshReuse, Entity: "user", EntityID: rt.UserID.String()})
			reuse = apperr.Unauthenticated("refresh token reuse detected")
			return nil // commit the revocation, then fail
		}
		if s.now().After(rt.ExpiresAt) {
			return apperr.Unauthenticated("refresh token expired")
		}
		u, err := q.GetUser(ctx, rt.UserID)
		if err != nil || u.Status != "active" {
			return apperr.Unauthenticated("account unavailable")
		}
		tk, err := s.issue(ctx, q, u.ID, u.CompanyID, rt.FamilyID)
		if err != nil {
			return err
		}
		out = tk
		// find new row id to link: rotate marks old row revoked (replaced_by informational)
		return q.RotateRefreshToken(ctx, gen.RotateRefreshTokenParams{ID: rt.ID, ReplacedBy: nil})
	})
	if err != nil {
		return nil, err
	}
	if reuse != nil {
		return nil, reuse
	}
	return out, nil
}

func (s *Service) Logout(ctx context.Context, refreshToken string) error {
	return s.tx.Do(ctx, func(q *gen.Queries) error {
		rt, err := q.GetRefreshTokenForUpdate(ctx, token.HashRefresh(refreshToken))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		return q.RevokeRefreshFamily(ctx, rt.FamilyID)
	})
}

// ---- self-service -----------------------------------------------------------------------

type Me struct {
	User   UserInfo              `json:"user"`
	Grants map[string]GrantScope `json:"grants"`
	Roles  []RoleScope           `json:"roles"`
}

type GrantScope struct {
	All   bool     `json:"all"`
	Paths []string `json:"paths"`
}

type RoleScope struct {
	RoleCode    string     `json:"role_code"`
	RoleName    string     `json:"role_name"`
	OrgUnitID   *uuid.UUID `json:"org_unit_id"`
	OrgUnitName *string    `json:"org_unit_name"`
}

func (s *Service) Me(ctx context.Context) (*Me, error) {
	p := access.MustPrincipal(ctx)
	grants := map[string]GrantScope{}
	for _, perm := range p.Grants.Permissions() {
		sc := p.Grants.Scope(perm)
		paths := sc.Paths
		if paths == nil {
			paths = []string{}
		}
		grants[perm] = GrantScope{All: sc.All, Paths: paths}
	}
	rows, err := s.tx.Queries().ListUserRoleScopes(ctx, []uuid.UUID{p.UserID})
	if err != nil {
		return nil, err
	}
	roles := make([]RoleScope, len(rows))
	for i, r := range rows {
		roles[i] = RoleScope{RoleCode: r.RoleCode, RoleName: r.RoleName, OrgUnitID: r.OrgUnitID, OrgUnitName: r.OrgUnitName}
	}
	return &Me{User: UserInfo{ID: p.UserID, CompanyID: p.CompanyID, Email: p.Email, Name: p.Name}, Grants: grants, Roles: roles}, nil
}

func validatePassword(pw string) error {
	if len(pw) < 8 {
		return apperr.Validation(map[string]string{"new_password": "min=8"})
	}
	return nil
}

func (s *Service) ChangePassword(ctx context.Context, oldPw, newPw string) error {
	p := access.MustPrincipal(ctx)
	if err := validatePassword(newPw); err != nil {
		return err
	}
	return s.tx.Do(ctx, func(q *gen.Queries) error {
		u, err := q.GetUser(ctx, p.UserID)
		if err != nil {
			return err
		}
		if !VerifySecret(oldPw, u.PasswordHash) {
			return apperr.New(422, "WRONG_PASSWORD", "Current password is incorrect")
		}
		h, err := HashSecret(newPw)
		if err != nil {
			return err
		}
		if err := q.UpdateUserPassword(ctx, gen.UpdateUserPasswordParams{ID: u.ID, PasswordHash: h}); err != nil {
			return err
		}
		if err := q.RevokeUserRefreshTokens(ctx, u.ID); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{Action: audit.PasswordChanged, Entity: "user", EntityID: u.ID.String()})
	})
}

func (s *Service) SetPIN(ctx context.Context, password, pin string) error {
	p := access.MustPrincipal(ctx)
	if !pinRe.MatchString(pin) {
		return apperr.Validation(map[string]string{"pin": "4-8 digits"})
	}
	return s.tx.Do(ctx, func(q *gen.Queries) error {
		u, err := q.GetUser(ctx, p.UserID)
		if err != nil {
			return err
		}
		if !VerifySecret(password, u.PasswordHash) {
			return apperr.New(422, "WRONG_PASSWORD", "Password is incorrect")
		}
		h, err := HashSecret(pin)
		if err != nil {
			return err
		}
		if err := q.UpdateUserPIN(ctx, gen.UpdateUserPINParams{ID: u.ID, PinHash: &h}); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{Action: audit.PINChanged, Entity: "user", EntityID: u.ID.String()})
	})
}

// Approval is the result of a successful supervisor PIN check.
type Approval struct {
	ApproverID   uuid.UUID `json:"approver_id"`
	ApproverName string    `json:"approver_name"`
	Permission   string    `json:"permission"`
	StoreID      uuid.UUID `json:"store_id"`
}

// ApproveWithPIN lets a supervisor authorize a sensitive action on someone else's terminal.
// The approver must hold approval.pin.approve AND the requested permission within the store's scope.
func (s *Service) ApproveWithPIN(ctx context.Context, approverEmail, pin, permission string, storeID uuid.UUID) (*Approval, error) {
	caller := access.MustPrincipal(ctx)
	q := s.tx.Queries()
	st, err := q.GetStore(ctx, gen.GetStoreParams{ID: storeID, CompanyID: caller.CompanyID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apperr.NotFound("store")
	}
	if err != nil {
		return nil, err
	}
	if !caller.Grants.Can(access.OrganizationView, st.Path) {
		return nil, apperr.NotFound("store")
	}
	email := strings.ToLower(strings.TrimSpace(approverEmail))
	key := "pin|" + caller.Email + "|" + email
	if s.throttle.blockedFor(key) > 0 {
		return nil, apperr.New(429, "LOGIN_THROTTLED", "Too many failed attempts, try again later")
	}
	fail := func() (*Approval, error) {
		s.throttle.fail(key)
		return nil, apperr.New(403, "APPROVAL_DENIED", "Approval denied")
	}
	ap, err := q.GetUserByEmail(ctx, email)
	if errors.Is(err, pgx.ErrNoRows) {
		VerifySecret(pin, dummyHash)
		return fail()
	}
	if err != nil {
		return nil, err
	}
	if ap.CompanyID != caller.CompanyID || ap.Status != "active" || ap.PinHash == nil || !VerifySecret(pin, *ap.PinHash) {
		return fail()
	}
	apPrin, err := s.LoadPrincipal(ctx, ap.ID, ap.CompanyID)
	if err != nil {
		return nil, err
	}
	if !apPrin.Grants.Can(access.ApprovalPIN, st.Path) || !apPrin.Grants.Can(permission, st.Path) {
		return fail()
	}
	s.throttle.reset(key)
	if err := s.tx.Do(ctx, func(q *gen.Queries) error {
		return audit.Record(ctx, q, audit.Entry{StoreID: &storeID, Action: audit.PINApproval, Entity: "approval", EntityID: ap.ID.String(),
			New: map[string]any{"permission": permission, "approver_id": ap.ID, "requested_by": caller.UserID}})
	}); err != nil {
		return nil, err
	}
	return &Approval{ApproverID: ap.ID, ApproverName: ap.Name, Permission: permission, StoreID: storeID}, nil
}

// ---- helpers ----------------------------------------------------------------------------

type throttle struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	m      map[string]*attempt
}

type attempt struct {
	n     int
	until time.Time
	last  time.Time
}

func newThrottle(max int, window time.Duration) *throttle {
	return &throttle{max: max, window: window, m: map[string]*attempt{}}
}

func (t *throttle) blockedFor(k string) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	a := t.m[k]
	if a == nil || time.Now().After(a.until) {
		return 0
	}
	return time.Until(a.until)
}

func (t *throttle) fail(k string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	a := t.m[k]
	if a == nil || time.Since(a.last) > t.window {
		a = &attempt{}
		t.m[k] = a
	}
	a.n++
	a.last = time.Now()
	if a.n >= t.max {
		a.until = time.Now().Add(t.window)
		a.n = 0
	}
}

func (t *throttle) reset(k string) {
	t.mu.Lock()
	delete(t.m, k)
	t.mu.Unlock()
}

type principalCache struct {
	mu  sync.RWMutex
	ttl time.Duration
	m   map[uuid.UUID]cached
}

type cached struct {
	p   *access.Principal
	exp time.Time
}

func newPrincipalCache(ttl time.Duration) *principalCache {
	return &principalCache{ttl: ttl, m: map[uuid.UUID]cached{}}
}

func (c *principalCache) get(id uuid.UUID) *access.Principal {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if e, ok := c.m[id]; ok && time.Now().Before(e.exp) {
		return e.p
	}
	return nil
}

func (c *principalCache) put(p *access.Principal) {
	c.mu.Lock()
	c.m[p.UserID] = cached{p: p, exp: time.Now().Add(c.ttl)}
	c.mu.Unlock()
}

func (c *principalCache) delete(id uuid.UUID) {
	c.mu.Lock()
	delete(c.m, id)
	c.mu.Unlock()
}

// InvalidateAll drops every cached principal (use after role permission changes).
func (s *Service) InvalidateAll() {
	s.cache.mu.Lock()
	s.cache.m = map[uuid.UUID]cached{}
	s.cache.mu.Unlock()
}
