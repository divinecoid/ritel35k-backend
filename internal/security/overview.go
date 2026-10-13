package security

import (
	"context"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"github.com/ritel35k/backend/db/gen"
	"github.com/ritel35k/backend/internal/access"
	"github.com/ritel35k/backend/internal/platform/tx"
)

// ReaderOnlineWindow is how recent a heartbeat must be for a reader to count as online.
const ReaderOnlineWindow = 5 * time.Minute

type Overviewer struct {
	tx  *tx.Manager
	now func() time.Time
}

func NewOverviewer(t *tx.Manager) *Overviewer { return &Overviewer{tx: t, now: time.Now} }

type TypeCount struct {
	Type     string `json:"type"`
	Total    int64  `json:"total"`
	Active   int64  `json:"active"`
	Critical int64  `json:"critical"`
}

type DayCount struct {
	Day      string `json:"day"`
	Total    int64  `json:"total"`
	Critical int64  `json:"critical"`
}

type StoreCount struct {
	StoreID   uuid.UUID `json:"store_id"`
	StoreName string    `json:"store_name"`
	Total     int64     `json:"total"`
	Active    int64     `json:"active"`
	Critical  int64     `json:"critical"`
}

type ReaderHealth struct {
	Total   int `json:"total"`
	Online  int `json:"online"`
	Offline int `json:"offline"`
	Never   int `json:"never_seen"`
}

type Overview struct {
	From           string           `json:"from"`
	To             string           `json:"to"`
	Active         int64            `json:"active"`          // open + acknowledged
	ActiveCritical int64            `json:"active_critical"` // of which critical
	Total          int64            `json:"total"`
	ByType         []TypeCount      `json:"by_type"`
	ByDay          []DayCount       `json:"by_day"`
	ByStore        []StoreCount     `json:"by_store"`
	Readers        *ReaderHealth    `json:"readers,omitempty"`
	Tags           map[string]int64 `json:"tags,omitempty"`
	StockMismatch  *int             `json:"stock_mismatch,omitempty"` // SKU/store pairs where tag count != system stock
}

func (o *Overviewer) Overview(ctx context.Context, f Filter) (*Overview, error) {
	p := access.MustPrincipal(ctx)
	sc := p.Grants.Scope(PermView)
	all, paths := sc.SQL()
	q := o.tx.Queries()
	from, to := f.window()
	out := &Overview{From: f.From.Format("2006-01-02"), To: f.To.Format("2006-01-02"), ByType: []TypeCount{}, ByDay: []DayCount{}, ByStore: []StoreCount{}}

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(4)
	g.Go(func() error {
		rows, err := q.SecuritySummary(gctx, gen.SecuritySummaryParams{CompanyID: p.CompanyID, AllScope: all, ScopePaths: paths, StoreID: f.StoreID, UnderPath: f.UnderPath, FromTime: from, ToTime: to})
		if err != nil {
			return err
		}
		byType := map[string]*TypeCount{}
		for _, r := range rows {
			tc := byType[r.Type]
			if tc == nil {
				tc = &TypeCount{Type: r.Type}
				byType[r.Type] = tc
			}
			tc.Total += r.N
			out.Total += r.N
			if r.Status == "open" || r.Status == "acknowledged" {
				tc.Active += r.N
				out.Active += r.N
				if r.Severity == "critical" {
					tc.Critical += r.N
					out.ActiveCritical += r.N
				}
			}
		}
		for _, t := range Types { // stable order, zero rows for types with no events
			if tc := byType[t]; tc != nil {
				out.ByType = append(out.ByType, *tc)
			} else {
				out.ByType = append(out.ByType, TypeCount{Type: t})
			}
		}
		return nil
	})
	g.Go(func() error {
		rows, err := q.SecurityByDay(gctx, gen.SecurityByDayParams{CompanyID: p.CompanyID, AllScope: all, ScopePaths: paths, StoreID: f.StoreID, UnderPath: f.UnderPath, FromTime: from, ToTime: to})
		if err != nil {
			return err
		}
		by := map[string]gen.SecurityByDayRow{}
		for _, r := range rows {
			by[r.Day.Format("2006-01-02")] = r
		}
		for d := f.From; !d.After(f.To); d = d.AddDate(0, 0, 1) {
			k := d.Format("2006-01-02")
			out.ByDay = append(out.ByDay, DayCount{Day: k, Total: by[k].N, Critical: by[k].Critical})
		}
		return nil
	})
	g.Go(func() error {
		rows, err := q.SecurityByStore(gctx, gen.SecurityByStoreParams{CompanyID: p.CompanyID, AllScope: all, ScopePaths: paths, StoreID: f.StoreID, UnderPath: f.UnderPath, FromTime: from, ToTime: to})
		if err != nil {
			return err
		}
		for _, r := range rows {
			out.ByStore = append(out.ByStore, StoreCount{StoreID: r.StoreID, StoreName: r.StoreName, Total: r.N, Active: r.Active, Critical: r.Critical})
		}
		return nil
	})
	if rsc := p.Grants.Scope("rfid.view"); !rsc.Empty() {
		rall, rpaths := rsc.SQL()
		g.Go(func() error {
			rows, err := q.ListReaders(gctx, gen.ListReadersParams{CompanyID: p.CompanyID, AllScope: rall, ScopePaths: rpaths, StoreID: f.StoreID})
			if err != nil {
				return err
			}
			h := &ReaderHealth{}
			cutoff := o.now().Add(-ReaderOnlineWindow)
			for _, r := range rows {
				if r.Status != "active" {
					continue
				}
				h.Total++
				switch {
				case r.LastSeenAt == nil:
					h.Never++
					h.Offline++
				case r.LastSeenAt.Before(cutoff):
					h.Offline++
				default:
					h.Online++
				}
			}
			out.Readers = h
			return nil
		})
		g.Go(func() error {
			rows, err := q.TagStatusCounts(gctx, gen.TagStatusCountsParams{CompanyID: p.CompanyID, AllScope: rall, ScopePaths: rpaths, UnderPath: f.UnderPath, StoreID: f.StoreID})
			if err != nil {
				return err
			}
			out.Tags = map[string]int64{}
			for _, r := range rows {
				out.Tags[r.Status] = r.N
			}
			return nil
		})
		g.Go(func() error {
			rows, err := q.RFIDStockVariance(gctx, gen.RFIDStockVarianceParams{CompanyID: p.CompanyID, AllScope: rall, ScopePaths: rpaths, UnderPath: f.UnderPath, StoreID: f.StoreID, OnlyDiff: true, RowLimit: 1000})
			if err != nil {
				return err
			}
			n := len(rows)
			out.StockMismatch = &n
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return out, nil
}
