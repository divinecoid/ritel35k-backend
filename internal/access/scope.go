package access

import (
	"context"

	"github.com/google/uuid"
)

// within reports whether ltree `path` equals scope or is a descendant of it (the ltree `<@` operator).
func within(path, scope string) bool {
	return path == scope || (len(path) > len(scope) && path[:len(scope)] == scope && path[len(scope)] == '.')
}

// ScopeFilter narrows queries to the part of the org tree a caller may touch.
// Every list/report query takes one; there is intentionally no "unfiltered" path.
type ScopeFilter struct {
	All   bool
	Paths []string
}

// SQL returns the (all, paths) pair expected by the sqlc scope predicates.
func (s ScopeFilter) SQL() (bool, []string) {
	if s.Paths == nil {
		return s.All, []string{}
	}
	return s.All, s.Paths
}

func (s ScopeFilter) Empty() bool { return !s.All && len(s.Paths) == 0 }

// Allows reports whether an org path is covered by the scope.
func (s ScopeFilter) Allows(path string) bool {
	if s.All {
		return true
	}
	for _, p := range s.Paths {
		if within(path, p) {
			return true
		}
	}
	return false
}

// Covers reports whether every org node visible through other is also visible through s
// (used to decide whether a narrower permission, e.g. profit, may be shown next to a broader one).
func (s ScopeFilter) Covers(other ScopeFilter) bool {
	if s.All {
		return true
	}
	if other.All {
		return false
	}
	for _, op := range other.Paths {
		ok := false
		for _, sp := range s.Paths {
			if within(op, sp) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

type grant struct {
	all   bool
	paths []string
}

// Grants is a caller's permission → scope map, loaded once per request.
type Grants struct{ m map[string]*grant }

func NewGrants() *Grants { return &Grants{m: map[string]*grant{}} }

// Add records one (permission, scope) pair; scopePath nil means company-wide.
func (g *Grants) Add(perm string, scopePath *string) {
	gr := g.m[perm]
	if gr == nil {
		gr = &grant{}
		g.m[perm] = gr
	}
	if scopePath == nil {
		gr.all = true
		return
	}
	for _, p := range gr.paths {
		if p == *scopePath {
			return
		}
	}
	gr.paths = append(gr.paths, *scopePath)
}

func (g *Grants) Has(perm string) bool { _, ok := g.m[perm]; return ok }

// Scope returns where perm applies. Missing permission → empty filter.
func (g *Grants) Scope(perm string) ScopeFilter {
	gr := g.m[perm]
	if gr == nil {
		return ScopeFilter{}
	}
	if gr.all {
		return ScopeFilter{All: true}
	}
	return ScopeFilter{Paths: append([]string(nil), gr.paths...)}
}

// Can reports whether perm applies to the org node at orgPath (e.g. a store's path).
func (g *Grants) Can(perm, orgPath string) bool { return g.Scope(perm).Allows(orgPath) }

// Permissions lists granted permission codes.
func (g *Grants) Permissions() []string {
	out := make([]string, 0, len(g.m))
	for k := range g.m {
		out = append(out, k)
	}
	return out
}

type Principal struct {
	UserID    uuid.UUID
	CompanyID uuid.UUID
	Email     string
	Name      string
	Grants    *Grants
}

type ctxKey struct{}

func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

func PrincipalFrom(ctx context.Context) (*Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(*Principal)
	return p, ok
}

// MustPrincipal is for code paths behind auth middleware.
func MustPrincipal(ctx context.Context) *Principal {
	p, ok := PrincipalFrom(ctx)
	if !ok {
		panic("access: no principal in context (route missing auth middleware)")
	}
	return p
}
