package rfid

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestNormalizeEPC(t *testing.T) {
	ok := map[string]string{
		"e2801160600002054a1b2c3d":      "E2801160600002054A1B2C3D",
		" 0xE280 1160 6000 0205 4A1B ":  "E2801160600002054A1B",
		"e2:80:11:60:60:00:02:05:4a:1b": "E2801160600002054A1B",
		"AABBCCDD":                      "AABBCCDD",
	}
	for in, want := range ok {
		got, err := NormalizeEPC(in)
		if err != nil || got != want {
			t.Errorf("NormalizeEPC(%q)=%q,%v want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "xyz", "ABC", "E28011606000020G", string(make([]byte, 80))} {
		if _, err := NormalizeEPC(bad); err == nil {
			t.Errorf("NormalizeEPC(%q) should fail", bad)
		}
	}
	if !LooksLikeEPC("e2801160600002054a1b2c3d") || LooksLikeEPC("8993000000019") || LooksLikeEPC("ABC123") {
		t.Error("LooksLikeEPC")
	}
}

func TestStatusMachine(t *testing.T) {
	cases := []struct {
		from, to string
		want     bool
	}{
		{InStock, OnShelf, true}, {OnShelf, Reserved, true}, {Lost, OnShelf, true}, {Returned, InStock, true},
		{Sold, OnShelf, false}, {Sold, Lost, false}, {InStock, Sold, false}, {OnShelf, Sold, false}, // SOLD only by checkout
		{Damaged, InStock, false}, {Damaged, Lost, false}, {InStock, InStock, false}, {Registered, Reserved, false},
	}
	for _, c := range cases {
		if got := CanChange(c.from, c.to); got != c.want {
			t.Errorf("CanChange(%s→%s)=%v want %v", c.from, c.to, got, c.want)
		}
	}
	for _, s := range []string{InStock, OnShelf, Returned} {
		if !Sellable(s) {
			t.Errorf("%s should be sellable", s)
		}
	}
	for _, s := range []string{Registered, Reserved, Sold, Damaged, Lost} {
		if Sellable(s) {
			t.Errorf("%s must not be sellable", s)
		}
	}
}

type gateFixture struct {
	Grace int `json:"grace_seconds"`
	Cases []struct {
		Name string `json:"name"`
		Tag  *struct {
			Status        string `json:"status"`
			Store         string `json:"store"`
			SoldOffsetSec *int   `json:"sold_offset_seconds"`
		} `json:"tag"`
		Direction     string `json:"direction"`
		ReportUnknown bool   `json:"report_unknown"`
		Result        string `json:"result"`
		Type          string `json:"type"`
	} `json:"cases"`
}

// TestDecideGate runs the contract shared with the edge agent (testdata/gate_cases.json).
func TestDecideGate(t *testing.T) {
	raw, err := os.ReadFile("testdata/gate_cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx gateFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	store, other := uuid.New(), uuid.New()
	now := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	for _, c := range fx.Cases {
		var facts *TagFacts
		if c.Tag != nil {
			facts = &TagFacts{Status: c.Tag.Status}
			if c.Tag.Store == "other" {
				facts.StoreID = &other
			} else {
				facts.StoreID = &store
			}
			if c.Tag.SoldOffsetSec != nil {
				at := now.Add(time.Duration(*c.Tag.SoldOffsetSec) * time.Second)
				facts.SoldAt = &at
			}
		}
		d := DecideGate(facts, store, c.Direction, now, Rules{SoldGrace: time.Duration(fx.Grace) * time.Second, ReportUnknown: c.ReportUnknown})
		if d.Result != c.Result || d.Type != c.Type {
			t.Errorf("%s: got result=%s type=%s, want result=%s type=%s", c.Name, d.Result, d.Type, c.Result, c.Type)
		}
		if d.Record != (c.Result == Alarm) {
			t.Errorf("%s: Record=%v for result %s", c.Name, d.Record, d.Result)
		}
	}
}
