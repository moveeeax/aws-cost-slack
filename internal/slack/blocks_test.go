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

func allText(msg Message) string {
	raw, _ := json.Marshal(msg)
	var back struct {
		Blocks []struct {
			Text struct {
				Text string `json:"text"`
			} `json:"text"`
		} `json:"blocks"`
	}
	_ = json.Unmarshal(raw, &back)
	var b strings.Builder
	for _, blk := range back.Blocks {
		b.WriteString(blk.Text.Text + "\n")
	}
	return b.String()
}

func TestRenderForecastAndBudget(t *testing.T) {
	r := report()
	r.MonthEnd = &costs.MonthEnd{Total: 6000, HasLastMonth: true, LastMonth: 5000, VsLastMonthPct: 20}
	r.Budget = &costs.BudgetStatus{Name: "team", Amount: 5000, Spent: 4100, UsedPct: 82, ForecastPct: 120, HasForecast: true, Level: costs.BudgetWarn}
	s := allText(Render(r))

	for _, want := range []string{
		"*Month-end forecast:* $6000.00  (+20.0% vs last month)",
		":warning: *Budget (team):* $4100.00 of $5000.00  `▓▓▓▓▓▓▓▓░░` 82%",
		"Forecast: 120% of budget :warning:",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in\n%s", want, s)
		}
	}
}

func TestRenderFinalMonthAndOverBudget(t *testing.T) {
	r := report()
	r.MonthEnd = &costs.MonthEnd{Total: 5200, Final: true}
	r.Budget = &costs.BudgetStatus{Amount: 5000, Spent: 5200, UsedPct: 104, ForecastPct: 104, HasForecast: true, Level: costs.BudgetOver}
	s := allText(Render(r))
	for _, want := range []string{
		"*Month total:* $5200.00",
		":rotating_light: *Budget:* $5200.00 of $5000.00  `▓▓▓▓▓▓▓▓▓▓` 104% — over budget",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in\n%s", want, s)
		}
	}
	if strings.Contains(s, "vs last month") {
		t.Error("no last month figure: comparison must be omitted")
	}
	if strings.Contains(s, "120% of budget :warning:") {
		t.Error("over budget already says so; no extra forecast warning")
	}
}

func TestRenderBaselineAnomalyOngoingAndResolved(t *testing.T) {
	r := report()
	r.Baseline = &costs.BaselineInfo{Days: 28, Sigma: 2.5}
	r.Anomalies = []costs.Row{{
		Name: "Amazon CloudWatch", Amount: 30, Baseline: 3.1, Score: 8.7,
		Recent: []float64{3, 3, 3, 3, 3, 3, 30}, Ongoing: true, Since: "2026-08-01",
	}}
	r.Resolved = []costs.Resolved{{Name: "AWS Glue", Amount: 9, Baseline: 8.5, Since: "2026-07-30"}}
	s := allText(Render(r))
	for _, want := range []string{
		"*Anomaly (≥ 2.5σ over 28-day baseline):*",
		"• *Amazon CloudWatch* $30.00 vs baseline $3.10  (+$26.90, 8.7σ)  `▁▁▁▁▁▁█`  _ongoing since 2026-08-01_",
		":white_check_mark: *Resolved:*\n• *AWS Glue* back to $9.00 (baseline $8.50), alerted since 2026-07-30",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in\n%s", want, s)
		}
	}
}

func TestSparklineAndProgressBar(t *testing.T) {
	if got := Sparkline([]float64{0, 1, 2, 3, 4, 5, 6, 7}); got != "▁▂▃▄▅▆▇█" {
		t.Errorf("sparkline ramp = %q", got)
	}
	if got := Sparkline([]float64{5, 5, 5}); got != "▅▅▅" {
		t.Errorf("flat sparkline = %q", got)
	}
	if got := progressBar(-5); got != "░░░░░░░░░░" {
		t.Errorf("negative pct bar = %q", got)
	}
	if got := progressBar(250); got != "▓▓▓▓▓▓▓▓▓▓" {
		t.Errorf("clamped bar = %q", got)
	}
}
