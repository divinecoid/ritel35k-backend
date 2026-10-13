package report

import (
	"context"
	"encoding/csv"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/ritel35k/backend/db/gen"
	"github.com/ritel35k/backend/internal/audit"
	"github.com/ritel35k/backend/internal/httpx"
	"github.com/ritel35k/backend/internal/httpx/middleware"
	"github.com/ritel35k/backend/internal/httpx/response"
)

type Handler struct {
	httpx.Base
	svc *Service
}

func NewHandler(b httpx.Base, s *Service) *Handler { return &Handler{Base: b, svc: s} }

func (h *Handler) Routes(r chi.Router, g *middleware.Guard) {
	r.With(g.Perm(PermDashboard)).Get("/dashboard/overview", h.overview)
	r.Route("/reports", func(r chi.Router) {
		r.Use(g.AnyPerm(PermSales, PermInventory))
		r.Get("/", h.catalog)
		r.Get("/{key}", h.run)
	})
}

func (h *Handler) overview(w http.ResponseWriter, r *http.Request) {
	f, err := ParseFilter(r, h.svc.now())
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	out, err := h.svc.Overview(r.Context(), f)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, out)
}

func (h *Handler) catalog(w http.ResponseWriter, r *http.Request) {
	response.OK(w, h.svc.Catalog(r.Context()))
}

func (h *Handler) run(w http.ResponseWriter, r *http.Request) {
	f, err := ParseFilter(r, h.svc.now())
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	q := r.URL.Query()
	o := Options{Order: q.Get("order"), OnlyVariance: q.Get("only_variance") == "true"}
	if o.Limit, _ = strconv.Atoi(q.Get("limit")); o.Limit < 0 {
		o.Limit = 0
	}
	key := chi.URLParam(r, "key")
	csvOut := q.Get("format") == "csv"
	if csvOut {
		o.Limit = MaxLimit // an export is the whole report, up to the hard cap
	}
	t, err := h.svc.Run(r.Context(), key, f, o)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	if !csvOut {
		response.OK(w, t)
		return
	}
	if err := h.svc.recordExport(r.Context(), key, f, len(t.Rows)); err != nil {
		h.Fail(w, r, err)
		return
	}
	writeCSV(w, t)
}

func (s *Service) recordExport(ctx context.Context, key string, f Filter, rows int) error {
	return s.tx.Do(ctx, func(q *gen.Queries) error {
		return audit.Record(ctx, q, audit.Entry{Action: "REPORT_EXPORTED", Entity: "report", EntityID: key,
			New: map[string]any{"from": f.From.Format("2006-01-02"), "to": f.To.Format("2006-01-02"), "under_path": f.UnderPath, "store_id": f.StoreID, "rows": rows}})
	})
}

// writeCSV streams the table as UTF-8 CSV with a BOM so Excel opens Indonesian text correctly.
func writeCSV(w http.ResponseWriter, t *Table) {
	name := t.Report
	if t.From != "" {
		name += "_" + t.From + "_" + t.To
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.csv"`, name))
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte("\xEF\xBB\xBF"))
	cw := csv.NewWriter(w)
	head := make([]string, len(t.Columns))
	for i, c := range t.Columns {
		head[i] = c.Label
	}
	_ = cw.Write(head)
	write := func(cells []any) {
		rec := make([]string, len(t.Columns))
		for i := range t.Columns {
			if i < len(cells) {
				rec[i] = cell(cells[i], t.Columns[i].Type)
			}
		}
		_ = cw.Write(rec)
	}
	for _, row := range t.Rows {
		write(row)
	}
	if t.Totals != nil {
		write(t.Totals)
	}
	cw.Flush()
}

// cell renders one value. Text cells are defused against spreadsheet formula injection (=, +, -, @ at the start).
func cell(v any, typ string) string {
	if v == nil {
		return ""
	}
	s := fmt.Sprint(v)
	if typ == TText && s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}
