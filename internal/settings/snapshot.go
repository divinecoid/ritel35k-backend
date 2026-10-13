package settings

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/ritel35k/backend/db/gen"
)

// Snapshot is every setting resolved for one org unit in a single query — use it on hot paths
// (POS checkout) instead of many Get calls.
type Snapshot map[string]json.RawMessage

func LoadSnapshot(ctx context.Context, q *gen.Queries, companyID, orgUnitID uuid.UUID) (Snapshot, error) {
	rows, err := q.ResolveAllSettings(ctx, gen.ResolveAllSettingsParams{CompanyID: companyID, OrgUnitID: orgUnitID})
	if err != nil {
		return nil, err
	}
	s := make(Snapshot, len(Registry))
	for _, d := range Registry {
		raw, _ := json.Marshal(d.Default)
		s[d.Key] = raw
	}
	for _, r := range rows {
		s[r.Key] = r.Value
	}
	return s, nil
}

func (s Snapshot) Decimal(key string) decimal.Decimal {
	var f float64
	_ = json.Unmarshal(s[key], &f)
	return decimal.NewFromFloat(f)
}

func (s Snapshot) Bool(key string) bool {
	var b bool
	_ = json.Unmarshal(s[key], &b)
	return b
}

func (s Snapshot) String(key string) string {
	var v string
	_ = json.Unmarshal(s[key], &v)
	return v
}

func (s Snapshot) Strings(key string) []string {
	var v []string
	_ = json.Unmarshal(s[key], &v)
	return v
}
