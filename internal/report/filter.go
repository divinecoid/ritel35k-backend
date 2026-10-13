// Package report serves the dashboard and the report catalogue from the summary tables (never from raw sales).
package report

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ritel35k/backend/db/gen"
	"github.com/ritel35k/backend/internal/access"
	"github.com/ritel35k/backend/internal/httpx"
	"github.com/ritel35k/backend/pkg/apperr"
)

const maxRangeDays = 400

var pathRe = regexp.MustCompile(`^[A-Za-z0-9_]+(\.[A-Za-z0-9_]+)*$`)

// Filter is the standard global filter: company ▸ region ▸ store (as an ltree path or store id) + date range.
// It is always intersected with the caller's scope in SQL; there is no way to widen it.
type Filter struct {
	From, To  time.Time
	UnderPath string
	StoreID   *uuid.UUID
}

func (f Filter) Days() int { return int(f.To.Sub(f.From).Hours()/24) + 1 }

// Previous is the period of equal length immediately before f.
func (f Filter) Previous() (time.Time, time.Time) {
	d := f.Days()
	return f.From.AddDate(0, 0, -d), f.From.AddDate(0, 0, -1)
}

func ParseFilter(r *http.Request, now time.Time) (Filter, error) {
	q := r.URL.Query()
	var f Filter
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	var err error
	if s := q.Get("to"); s != "" {
		if f.To, err = time.Parse("2006-01-02", s); err != nil {
			return f, apperr.BadRequest("to must be YYYY-MM-DD")
		}
	} else {
		f.To = today
	}
	if s := q.Get("from"); s != "" {
		if f.From, err = time.Parse("2006-01-02", s); err != nil {
			return f, apperr.BadRequest("from must be YYYY-MM-DD")
		}
	} else {
		f.From = f.To.AddDate(0, 0, -6)
	}
	if f.From.After(f.To) {
		return f, apperr.BadRequest("from must not be after to")
	}
	if f.Days() > maxRangeDays {
		return f, apperr.BadRequest("date range is limited to 400 days")
	}
	if f.UnderPath = q.Get("under_path"); f.UnderPath != "" && !pathRe.MatchString(f.UnderPath) {
		return f, apperr.BadRequest("invalid under_path")
	}
	if f.StoreID, err = httpx.QueryUUID(r, "store_id"); err != nil {
		return f, err
	}
	return f, nil
}

// resolveStorePath turns a store filter into its org path so profit gating and breakdowns can reason about the node.
func resolveStorePath(ctx context.Context, q *gen.Queries, p *access.Principal, f *Filter) error {
	if f.StoreID == nil {
		return nil
	}
	st, err := q.GetStore(ctx, gen.GetStoreParams{ID: *f.StoreID, CompanyID: p.CompanyID})
	if errors.Is(err, pgx.ErrNoRows) {
		return apperr.NotFound("store")
	}
	if err != nil {
		return err
	}
	f.UnderPath = st.Path
	return nil
}

// showProfit decides whether cost/profit figures may be shown for this filter.
// They need report.profit.view over everything the sales scope shows, or over the selected node.
func showProfit(p *access.Principal, salesPerm string, f Filter) bool {
	profit := p.Grants.Scope("report.profit.view")
	if profit.Empty() {
		return false
	}
	if f.UnderPath != "" && profit.Allows(f.UnderPath) {
		return true
	}
	return profit.Covers(p.Grants.Scope(salesPerm))
}

type scopeArgs struct {
	All   bool
	Paths []string
}

func scopeOf(p *access.Principal, perm string) (scopeArgs, bool) {
	sc := p.Grants.Scope(perm)
	all, paths := sc.SQL()
	return scopeArgs{all, paths}, !sc.Empty()
}

var errNoScope = apperr.Forbidden("No access to this report in any part of the organization")
