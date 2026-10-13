package access

import (
	"github.com/google/uuid"

	"github.com/ritel35k/backend/pkg/apperr"
)

// RequireAll demands the permission be held company-wide. Used for global master data
// (products, suppliers, roles) which is shared by every store.
func RequireAll(p *Principal, perm string) error {
	if !p.Grants.Scope(perm).All {
		return apperr.Forbidden("company-wide permission required").WithDetails(map[string]string{"permission": perm})
	}
	return nil
}

// Invalidator is implemented by auth.Service so user/role changes take effect immediately.
type Invalidator interface {
	InvalidatePrincipal(id uuid.UUID)
	InvalidateAll()
}
