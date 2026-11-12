package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// TestAlertsOnlyPostsOnce runs the CLI twice against the baseline fixture
// with a state file: the first run posts the CloudWatch spike, the second
// finds it already open and posts nothing.
func TestAlertsOnlyPostsOnce(t *testing.T) {
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		posts.Add(1)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	statePath := filepath.Join(t.TempDir(), "state.json")
	args := []string{
		"--input", "../../examples/baseline_input.json",
		"--baseline-days", "28",
		"--state", statePath,
		"--alerts-only",
		"--webhook", srv.URL,
	}
	for i := 0; i < 2; i++ {
		if err := run(args); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
	}
	if got := posts.Load(); got != 1 {
		t.Fatalf("want exactly 1 post over two runs, got %d", got)
	}
	b, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("state not written: %v", err)
	}
	if !strings.Contains(string(b), `"SERVICE/Amazon CloudWatch"`) {
		t.Fatalf("state should hold the open CloudWatch alert:\n%s", b)
	}
}

func TestValidate(t *testing.T) {
	bad := []config{
		{Budget: 100, BudgetName: "team"},
		{BudgetName: "team", InputFile: "x.json"},
		{Budget: -1},
		{BaselineDays: 7, Sigma: 0},
		{Backtest: -1},
	}
	for _, c := range bad {
		if err := c.validate(); err == nil {
			t.Errorf("config %+v should be rejected", c)
		}
	}
	if err := (config{BaselineDays: 28, Sigma: 2.5, Budget: 5000}).validate(); err != nil {
		t.Errorf("valid config rejected: %v", err)
	}
}
