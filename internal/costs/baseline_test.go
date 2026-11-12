package costs

import (
	"math"
	"testing"
	"time"
)

// series builds daily Periods for consecutive days starting at start, one
// value per day for each named group.
func series(start string, groups map[string][]float64) []Period {
	t0, _ := time.Parse(dateLayout, start)
	n := 0
	for _, v := range groups {
		n = max(n, len(v))
	}
	days := make([]Period, n)
	for i := range days {
		d := t0.AddDate(0, 0, i)
		days[i] = Period{
			Start:   d.Format(dateLayout),
			End:     d.AddDate(0, 0, 1).Format(dateLayout),
			ByGroup: map[string]float64{},
		}
		for name, vals := range groups {
			if i < len(vals) {
				days[i].ByGroup[name] = vals[i]
			}
		}
	}
	return days
}

func repeat(v float64, n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = v
	}
	return out
}

func TestBaselineFlagsSpikeAndScoresSigma(t *testing.T) {
	// 14 days of CloudWatch around $3 (alternating 2/4), EC2 steady at $100.
	cw := make([]float64, 14)
	for i := range cw {
		cw[i] = 2 + float64(i%2)*2
	}
	hist := series("2026-07-20", map[string][]float64{"Amazon CloudWatch": cw, "Amazon EC2": repeat(100, 14)})
	in := Input{
		GroupBy:   "SERVICE",
		Currency:  "USD",
		Yesterday: Period{Start: "2026-08-03", End: "2026-08-04", ByGroup: map[string]float64{"Amazon CloudWatch": 30, "Amazon EC2": 100.5}},
		DayBefore: hist[len(hist)-1],
		History:   hist,
	}
	r := Build(in, Options{BaselineDays: 14, Sigma: 2.5})

	if r.Baseline == nil || r.Baseline.Days != 14 || r.Baseline.Sigma != 2.5 {
		t.Fatalf("baseline info = %+v", r.Baseline)
	}
	if len(r.Anomalies) != 1 || r.Anomalies[0].Name != "Amazon CloudWatch" {
		t.Fatalf("want CloudWatch only, got %+v", r.Anomalies)
	}
	a := r.Anomalies[0]
	// Aug 3 2026 is a Monday: weekday days only (10 of the 14), still 2/4 alternating.
	if math.Abs(a.Baseline-3) > 0.3 {
		t.Errorf("baseline mean = %v, want about 3", a.Baseline)
	}
	if a.Score < 10 {
		t.Errorf("score = %v, want a large sigma", a.Score)
	}
	if len(a.Recent) != 7 || a.Recent[6] != 30 {
		t.Errorf("recent = %v, want 7 days ending with 30", a.Recent)
	}
	// EC2 moved $0.50 off a flat line: huge sigma, but under the $1 floor.
	for _, row := range r.Rows {
		if row.Name == "Amazon EC2" && row.Anomaly {
			t.Errorf("EC2 +$0.50 must stay quiet under min delta")
		}
	}
}

func TestBaselineKeepsWeekendsApart(t *testing.T) {
	// Batch jobs run on weekends: $50 Sat/Sun, $5 on weekdays. 2026-07-06 is a Monday.
	var vals []float64
	for i := 0; i < 28; i++ {
		wd := time.Date(2026, 7, 6+i, 0, 0, 0, 0, time.UTC).Weekday()
		if wd == time.Saturday || wd == time.Sunday {
			vals = append(vals, 50)
		} else {
			vals = append(vals, 5)
		}
	}
	hist := series("2026-07-06", map[string][]float64{"AWS Glue": vals})

	// Saturday 2026-08-08 at the usual $50: not an anomaly.
	sat := Input{Yesterday: Period{Start: "2026-08-08", End: "2026-08-09", ByGroup: map[string]float64{"AWS Glue": 50}}, History: hist}
	if r := Build(sat, Options{BaselineDays: 28, Sigma: 2.5}); len(r.Anomalies) != 0 {
		t.Fatalf("weekend at its weekend level flagged: %+v", r.Anomalies)
	}
	// Wednesday 2026-08-05 at $50: way above the weekday baseline.
	wed := Input{Yesterday: Period{Start: "2026-08-05", End: "2026-08-06", ByGroup: map[string]float64{"AWS Glue": 50}}, History: hist}
	r := Build(wed, Options{BaselineDays: 28, Sigma: 2.5})
	if len(r.Anomalies) != 1 || r.Anomalies[0].Baseline != 5 {
		t.Fatalf("weekday at weekend level should be flagged against $5: %+v", r.Anomalies)
	}
}

func TestBaselineFallsBackWhenFewSeasonalDays(t *testing.T) {
	// Five weekdays only (Mon Jul 27 .. Fri Jul 31); scoring a Saturday must
	// use all of them instead of an empty weekend set.
	hist := series("2026-07-27", map[string][]float64{"Amazon S3": {10, 10, 10, 10, 10}})
	mean, _ := baseline("Amazon S3", hist, true)
	if mean != 10 {
		t.Fatalf("fallback mean = %v, want 10", mean)
	}
}

func TestBaselineDaysTrimsLongHistory(t *testing.T) {
	// 20 days at $100 then 7 days at $10; a 7-day baseline only sees the $10s.
	vals := append(repeat(100, 20), repeat(10, 7)...)
	hist := series("2026-07-07", map[string][]float64{"Amazon EC2": vals})
	in := Input{Yesterday: Period{Start: "2026-08-03", End: "2026-08-04", ByGroup: map[string]float64{"Amazon EC2": 40}}, History: hist}
	r := Build(in, Options{BaselineDays: 7, Sigma: 2.5})
	if r.Baseline.Days != 7 || len(r.Anomalies) != 1 || r.Anomalies[0].Baseline != 10 {
		t.Fatalf("want a 7-day $10 baseline and one anomaly, got %+v / %+v", r.Baseline, r.Anomalies)
	}
}

func TestNoHistoryFallsBackToDoD(t *testing.T) {
	r := Build(sampleInput(), Options{ThresholdPct: 100, BaselineDays: 30, Sigma: 2.5})
	if r.Baseline != nil {
		t.Fatal("no history: report must not claim baseline mode")
	}
	if len(r.Anomalies) != 1 || r.Anomalies[0].Name != "Amazon CloudWatch" {
		t.Fatalf("DoD detection expected, got %+v", r.Anomalies)
	}
}

func TestMinDeltaOption(t *testing.T) {
	// CloudWatch 3 -> 30 is +$27; a $50 floor silences it.
	if r := Build(sampleInput(), Options{ThresholdPct: 100, MinDelta: 50}); len(r.Anomalies) != 0 {
		t.Fatalf("min delta 50 should silence a $27 spike: %+v", r.Anomalies)
	}
}
