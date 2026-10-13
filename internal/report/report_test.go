package report

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ritel35k/backend/internal/access"
)

func TestCSVCellDefusesFormulas(t *testing.T) {
	cases := []struct {
		in, typ, want string
	}{
		{"=HYPERLINK(1)", TText, "'=HYPERLINK(1)"},
		{"+62812", TText, "'+62812"},
		{"@cmd", TText, "'@cmd"},
		{"-5000", TMoney, "-5000"}, // numbers keep their sign
		{"Toko Jakarta", TText, "Toko Jakarta"},
		{"", TText, ""},
	}
	for _, c := range cases {
		if got := cell(c.in, c.typ); got != c.want {
			t.Errorf("cell(%q,%s)=%q want %q", c.in, c.typ, got, c.want)
		}
	}
	if cell(nil, TText) != "" {
		t.Error("nil cell")
	}
}

func TestParseFilter(t *testing.T) {
	now := time.Date(2026, 10, 12, 15, 0, 0, 0, time.UTC)
	parse := func(q string) (Filter, error) {
		return ParseFilter(httptest.NewRequest("GET", "/x?"+q, nil), now)
	}
	f, err := parse("")
	if err != nil || f.To.Format("2006-01-02") != "2026-10-12" || f.From.Format("2006-01-02") != "2026-10-06" || f.Days() != 7 {
		t.Fatalf("defaults: %+v %v", f, err)
	}
	pf, pt := f.Previous()
	if pf.Format("2006-01-02") != "2026-09-29" || pt.Format("2006-01-02") != "2026-10-05" {
		t.Fatalf("previous period %v..%v", pf, pt)
	}
	for _, bad := range []string{"from=2026-10-12&to=2026-10-01", "from=x", "to=2026-02-30", "under_path=a.b%3Bdrop", "under_path=a..b", "store_id=nope", "from=2024-01-01&to=2026-10-12"} {
		if _, err := parse(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
	if f, err := parse("under_path=acme.jkt.store_1"); err != nil || f.UnderPath != "acme.jkt.store_1" {
		t.Fatalf("path: %+v %v", f, err)
	}
}

func principal(grants map[string][]string) *access.Principal {
	g := access.NewGrants()
	for perm, paths := range grants {
		if len(paths) == 0 {
			g.Add(perm, nil)
		}
		for _, p := range paths {
			p := p
			g.Add(perm, &p)
		}
	}
	return &access.Principal{Grants: g}
}

func TestShowProfitNeverWidensScope(t *testing.T) {
	// profit only on store A while the dashboard shows A and B → hidden for the region view, allowed for store A
	p := principal(map[string][]string{"dashboard.view": {"co.r.a", "co.r.b"}, "report.profit.view": {"co.r.a"}})
	if showProfit(p, "dashboard.view", Filter{}) {
		t.Error("profit must be hidden when the sales scope is wider than the profit scope")
	}
	if !showProfit(p, "dashboard.view", Filter{UnderPath: "co.r.a"}) {
		t.Error("profit should show when the selected node is inside the profit scope")
	}
	if showProfit(p, "dashboard.view", Filter{UnderPath: "co.r.b"}) {
		t.Error("profit must stay hidden for a node outside the profit scope")
	}
	// company-wide profit covers everything
	if !showProfit(principal(map[string][]string{"dashboard.view": {"co.r.a"}, "report.profit.view": nil}), "dashboard.view", Filter{}) {
		t.Error("company-wide profit grant")
	}
	if showProfit(principal(map[string][]string{"dashboard.view": nil}), "dashboard.view", Filter{}) {
		t.Error("no profit permission")
	}
}

func TestPercentHelper(t *testing.T) {
	if pct(d100, d100).String() != "100" || !pct(d100, d100.Sub(d100)).IsZero() {
		t.Error("pct")
	}
}
