package state

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/moveeeax/aws-cost-slack/internal/costs"
)

func TestLoadMissingIsEmpty(t *testing.T) {
	st, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if st.Open == nil || len(st.Open) != 0 {
		t.Fatalf("want empty open map, got %+v", st)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	in := costs.AlertState{Open: map[string]costs.OpenAlert{
		"SERVICE/Amazon CloudWatch": {GroupBy: "SERVICE", Name: "Amazon CloudWatch", Since: "2026-08-02", Amount: 30},
	}}
	if err := Save(path, in); err != nil {
		t.Fatalf("save: %v", err)
	}
	out, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := out.Open["SERVICE/Amazon CloudWatch"]; got != in.Open["SERVICE/Amazon CloudWatch"] {
		t.Fatalf("round trip lost data: %+v", out)
	}

	// No temp files left behind.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("want only state.json in dir, got %d entries", len(entries))
	}
}

func TestLoadRejectsGarbage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("want parse error for a corrupt state file")
	}
}
