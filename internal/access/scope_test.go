package access

import "testing"

func TestScope(t *testing.T) {
	g := NewGrants()
	jabo := "acme.jabo"
	g.Add("report.sales.view", &jabo)
	if !g.Can("report.sales.view", "acme.jabo.jkt_barat") {
		t.Fatal("descendant must be allowed")
	}
	if !g.Can("report.sales.view", "acme.jabo") {
		t.Fatal("self must be allowed")
	}
	if g.Can("report.sales.view", "acme.jabodetabek") {
		t.Fatal("label prefix must not match a sibling")
	}
	if g.Can("report.sales.view", "acme.jabar.bandung") {
		t.Fatal("other region must be denied")
	}
	if g.Can("pos.refund.create", "acme.jabo.jkt_barat") {
		t.Fatal("ungranted permission must be denied")
	}
	g.Add("dashboard.view", nil)
	if !g.Scope("dashboard.view").All {
		t.Fatal("nil scope means all")
	}
}

func TestExpand(t *testing.T) {
	all := Expand([]string{"*"})
	if len(all) != len(Catalog) {
		t.Fatalf("star expands to whole catalog: %d vs %d", len(all), len(Catalog))
	}
	pos := Expand([]string{"pos.*"})
	for _, c := range pos {
		if c[:4] != "pos." {
			t.Fatalf("unexpected %s", c)
		}
	}
	if len(pos) == 0 {
		t.Fatal("pos.* must match")
	}
}
