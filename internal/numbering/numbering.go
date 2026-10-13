// Package numbering produces race-free, per-store document numbers (SO/PO/GRN/TRF/...).
package numbering

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ritel35k/backend/db/gen"
	"github.com/ritel35k/backend/internal/settings"
)

var seqRe = regexp.MustCompile(`\{SEQ:(\d+)\}`)

// Period decides when the counter resets, derived from the tokens used in the format.
func Period(format string, t time.Time) string {
	switch {
	case strings.Contains(format, "{DD}"):
		return t.Format("20060102")
	case strings.Contains(format, "{MM}"):
		return t.Format("200601")
	case strings.Contains(format, "{YYYY}") || strings.Contains(format, "{YY}"):
		return t.Format("2006")
	}
	return "ALL"
}

// Render fills a format given the sequence number. Exported for tests and previews.
func Render(format, storeCode string, t time.Time, seq int64) (string, error) {
	m := seqRe.FindStringSubmatch(format)
	if m == nil {
		return "", errors.New("format must contain {SEQ:n}")
	}
	width, _ := strconv.Atoi(m[1])
	out := seqRe.ReplaceAllString(format, fmt.Sprintf("%0*d", width, seq))
	out = strings.NewReplacer("{STORE}", storeCode, "{YYYY}", t.Format("2006"), "{YY}", t.Format("06"), "{MM}", t.Format("01"), "{DD}", t.Format("02")).Replace(out)
	return out, nil
}

// Next allocates the next number for (store, docType) inside the caller's transaction.
// The counter row is locked by the upsert, so concurrent callers serialize and never collide.
// A rolled-back transaction releases its number (gaps are avoided; duplicates are impossible).
func Next(ctx context.Context, q *gen.Queries, companyID, storeID uuid.UUID, docType string, now time.Time) (string, error) {
	st, err := q.GetStore(ctx, gen.GetStoreParams{ID: storeID, CompanyID: companyID})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errors.New("numbering: unknown store")
	}
	if err != nil {
		return "", err
	}
	return NextAt(ctx, q, Owner{CompanyID: companyID, ID: storeID, Code: st.Code, OrgUnitID: st.OrgUnitID, Timezone: st.Timezone}, docType, now)
}

// Owner is whatever the counter belongs to: a store, or a central warehouse (no store).
type Owner struct {
	CompanyID uuid.UUID
	ID        uuid.UUID
	Code      string
	OrgUnitID uuid.UUID // where the format setting is resolved (store unit, or company root for central warehouses)
	Timezone  string
}

func NextAt(ctx context.Context, q *gen.Queries, o Owner, docType string, now time.Time) (string, error) {
	format, err := settings.GetString(ctx, q, o.CompanyID, o.OrgUnitID, "numbering.format."+docType)
	if err != nil {
		return "", err
	}
	loc, err := time.LoadLocation(o.Timezone)
	if err != nil {
		loc = time.UTC
	}
	local := now.In(loc)
	seq, err := q.NextDocumentNo(ctx, gen.NextDocumentNoParams{CompanyID: o.CompanyID, StoreID: o.ID, DocType: docType, Period: Period(format, local)})
	if err != nil {
		return "", err
	}
	return Render(format, o.Code, local, seq)
}

// ForWarehouse numbers a document for a warehouse: the store's counter for store warehouses,
// the warehouse's own counter for central ones.
func ForWarehouse(ctx context.Context, q *gen.Queries, wh gen.Warehouse, docType string, now time.Time) (string, error) {
	if wh.StoreID != nil {
		return Next(ctx, q, wh.CompanyID, *wh.StoreID, docType, now)
	}
	root, err := q.GetCompanyRootOrgUnit(ctx, wh.CompanyID)
	if err != nil {
		return "", err
	}
	return NextAt(ctx, q, Owner{CompanyID: wh.CompanyID, ID: wh.ID, Code: wh.Code, OrgUnitID: root.ID, Timezone: "Asia/Jakarta"}, docType, now)
}
