package costs

import "testing"

func TestReconcileNewOngoingResolved(t *testing.T) {
	// Day 1: CloudWatch spikes -> new alert.
	r1 := Build(sampleInput(), Options{ThresholdPct: 100})
	st := r1.Reconcile(AlertState{})
	if r1.NewAnomalies() != 1 || !r1.HasAlerts() {
		t.Fatalf("first run: want 1 new anomaly, got %+v", r1.Anomalies)
	}
	if st.Open["SERVICE/Amazon CloudWatch"].Since != "2026-08-02" {
		t.Fatalf("state should open the alert: %+v", st)
	}

	// Same data again (an hourly rerun): ongoing, nothing to alert.
	r2 := Build(sampleInput(), Options{ThresholdPct: 100})
	st = r2.Reconcile(st)
	if r2.NewAnomalies() != 0 || r2.HasAlerts() {
		t.Fatalf("rerun must not alert again: %+v", r2.Anomalies)
	}
	if !r2.Anomalies[0].Ongoing || r2.Anomalies[0].Since != "2026-08-02" {
		t.Fatalf("anomaly should be ongoing since day 1: %+v", r2.Anomalies[0])
	}
	for _, row := range r2.Rows {
		if row.Name == "Amazon CloudWatch" && !row.Ongoing {
			t.Error("top-N row should carry the ongoing flag too")
		}
	}

	// Next day CloudWatch is back to $3 -> resolved and dropped from state.
	in := sampleInput()
	in.Yesterday.Start = "2026-08-03"
	in.DayBefore.ByGroup["Amazon CloudWatch"] = 3
	in.Yesterday.ByGroup["Amazon CloudWatch"] = 3
	r3 := Build(in, Options{ThresholdPct: 100})
	st = r3.Reconcile(st)
	if len(r3.Resolved) != 1 || r3.Resolved[0].Name != "Amazon CloudWatch" || r3.Resolved[0].Amount != 3 || r3.Resolved[0].Since != "2026-08-02" {
		t.Fatalf("want CloudWatch resolved: %+v", r3.Resolved)
	}
	if !r3.HasAlerts() {
		t.Error("a resolution is worth posting")
	}
	if len(st.Open) != 0 {
		t.Fatalf("resolved alert should leave the state: %+v", st)
	}
}

func TestReconcileKeepsOtherDimensions(t *testing.T) {
	prev := AlertState{Open: map[string]OpenAlert{
		"LINKED_ACCOUNT/123456789012": {GroupBy: "LINKED_ACCOUNT", Name: "123456789012", Since: "2026-08-01"},
	}}
	r := Build(sampleInput(), Options{ThresholdPct: 100})
	st := r.Reconcile(prev)
	if len(r.Resolved) != 0 {
		t.Fatalf("a SERVICE run must not resolve account alerts: %+v", r.Resolved)
	}
	if _, ok := st.Open["LINKED_ACCOUNT/123456789012"]; !ok {
		t.Fatal("account alert dropped from state")
	}
}

func TestBacktestFiresOnceAndResolves(t *testing.T) {
	// 10 quiet days at $5, a 3-day spike at $40, then quiet again.
	vals := append(append(repeat(5, 10), 40, 40, 40), repeat(5, 3)...)
	days := series("2026-07-20", map[string][]float64{"Amazon EC2": repeat(100, len(vals)), "AWS Glue": vals})

	events := Backtest("SERVICE", "USD", days, Options{BaselineDays: 7, Sigma: 2.5})
	if len(events) != 2 {
		t.Fatalf("want one fire and one resolve, got %+v", events)
	}
	if e := events[0]; e.Kind != EventFire || e.Row.Name != "AWS Glue" || e.Date != "2026-07-30" {
		t.Errorf("fire = %+v", e)
	}
	// Days 2 and 3 of the spike score under Sigma because the baseline now
	// holds $40 days, but they are still over resolveSigma, so the alert stays
	// open until spend is back to $5.
	if e := events[1]; e.Kind != EventResolve || e.Date != "2026-08-02" || e.Since != "2026-07-30" || e.Row.Amount != 5 {
		t.Errorf("resolve = %+v", e)
	}
}
