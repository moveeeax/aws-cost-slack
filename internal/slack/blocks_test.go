package slack

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/moveeeax/aws-cost-slack/internal/costs"
)

func report() costs.Report {
	in := costs.Input{
		GroupBy:  "SERVICE",
		Currency: "USD",
		Yesterday: costs.Period{Start: "2026-08-02", ByGroup: map[string]float64{
			"Amazon EC2": 120, "Amazon CloudWatch": 30, "Amazon S3": 15,
		}},
		DayBefore: costs.Period{Start: "2026-08-01", ByGroup: map[string]float64{
			"Amazon EC2": 118, "Amazon CloudWatch": 3, "Amazon S3": 16,
		}},
		MTD: costs.Period{ByGroup: map[string]float64{"Amazon EC2": 238}},
	}
	return costs.BuildReport(in, 5, 100)
}

func TestRenderStructure(t *testing.T) {
	msg := Render(report())

	if msg.Text == "" {
		t.Error("fallback text must be set for notifications")
	}
	if msg.Blocks[0]["type"] != "header" {
		t.Errorf("first block should be a header, got %v", msg.Blocks[0]["type"])
	}

	// Round-trip through JSON to prove it is valid Block Kit shape.
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(raw)
	for _, want := range []string{"Amazon EC2", "Amazon CloudWatch", "$120.00", "Month to date", "Anomaly"} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered message missing %q\n%s", want, s)
		}
	}
}

func TestRenderNoAnomalies(t *testing.T) {
	in := costs.Input{
		GroupBy:   "SERVICE",
		Currency:  "USD",
		Yesterday: costs.Period{Start: "2026-08-02", ByGroup: map[string]float64{"Amazon EC2": 10}},
		DayBefore: costs.Period{Start: "2026-08-01", ByGroup: map[string]float64{"Amazon EC2": 10}},
		MTD:       costs.Period{ByGroup: map[string]float64{"Amazon EC2": 20}},
	}
	msg := Render(costs.BuildReport(in, 5, 100))
	raw, _ := json.Marshal(msg)
	if strings.Contains(string(raw), "Anomaly") {
		t.Error("no anomaly block should appear when nothing spikes")
	}
}

func TestPostSuccessAndFailure(t *testing.T) {
	// success
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("missing json content-type")
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte("ok"))
	}))
	defer ok.Close()
	if err := Post(context.Background(), ok.Client(), ok.URL, Render(report())); err != nil {
		t.Fatalf("expected success, got %v", err)
	}

	// failure surfaces status + body
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte("invalid_payload"))
	}))
	defer bad.Close()
	err := Post(context.Background(), bad.Client(), bad.URL, Render(report()))
	if err == nil || !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "invalid_payload") {
		t.Fatalf("expected loud 400 error, got %v", err)
	}
}
