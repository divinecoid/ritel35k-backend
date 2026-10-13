package numbering

import (
	"testing"
	"time"
)

func TestRender(t *testing.T) {
	at := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	got, err := Render("SO/{STORE}/{YYYY}/{MM}/{SEQ:6}", "JKT", at, 42)
	if err != nil || got != "SO/JKT/2026/10/000042" {
		t.Fatalf("got %q err %v", got, err)
	}
	if Period("SO/{STORE}/{YYYY}/{MM}/{SEQ:6}", at) != "202610" {
		t.Fatal("monthly period")
	}
	if Period("X-{SEQ:4}", at) != "ALL" {
		t.Fatal("never resets")
	}
	if _, err := Render("NO-SEQ", "A", at, 1); err == nil {
		t.Fatal("must require SEQ")
	}
}
