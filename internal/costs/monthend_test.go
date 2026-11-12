package costs

import (
	"math"
	"testing"
)

func TestMonthEndFromForecast(t *testing.T) {
	in := sampleInput() // MTD 269.50 through Aug 2
	in.Forecast = &Forecast{Start: "2026-08-03", End: "2026-09-01", Amount: 5730.50}
	in.LastMonth = &Period{Start: "2026-07-01", End: "2026-08-01", ByGroup: map[string]float64{"Amazon EC2": 5000}}

	r := Build(in, Options{})
	if r.MonthEnd == nil {
		t.Fatal("month end missing")
	}
	if r.MonthEnd.Total != 6000 || r.MonthEnd.Final {
		t.Fatalf("month end = %+v, want 6000 projected", *r.MonthEnd)
	}
	if !r.MonthEnd.HasLastMonth || r.MonthEnd.VsLastMonthPct != 20 {
		t.Fatalf("vs last month = %+v, want +20%%", *r.MonthEnd)
	}
}

func TestMonthEndFinalOnLastDay(t *testing.T) {
	in := sampleInput()
	in.Yesterday.Start, in.Yesterday.End = "2026-08-31", "2026-09-01"
	in.Forecast = &Forecast{Amount: 999} // ignored: the month is over
	r := Build(in, Options{})
	if r.MonthEnd == nil || !r.MonthEnd.Final || r.MonthEnd.Total != r.MTDTotal {
		t.Fatalf("last day should report the final MTD total: %+v", r.MonthEnd)
	}
	if r.MonthEnd.HasLastMonth {
		t.Error("no last month in input")
	}
}

func TestMonthEndAbsentWithoutForecast(t *testing.T) {
	if r := Build(sampleInput(), Options{}); r.MonthEnd != nil {
		t.Fatalf("mid-month without forecast: want nil, got %+v", r.MonthEnd)
	}
}

func TestBudgetLevels(t *testing.T) {
	cases := []struct {
		budget float64
		level  string
	}{
		{1000, BudgetOK},    // 27%
		{330, BudgetWarn},   // 81.7%
		{269.5, BudgetOver}, // exactly 100%
		{100, BudgetOver},
	}
	for _, c := range cases {
		r := Build(sampleInput(), Options{Budget: c.budget, BudgetName: "team"})
		if r.Budget == nil || r.Budget.Level != c.level {
			t.Errorf("budget %v: got %+v, want level %s", c.budget, r.Budget, c.level)
		}
	}
}

func TestBudgetWithForecast(t *testing.T) {
	in := sampleInput()
	in.Forecast = &Forecast{Amount: 5730.50} // month end 6000
	r := Build(in, Options{Budget: 5000})
	b := r.Budget
	if b == nil || !b.HasForecast || b.ForecastPct != 120 {
		t.Fatalf("forecast pct = %+v, want 120", b)
	}
	if b.Spent != 269.5 || math.Abs(b.UsedPct-5.39) > 1e-9 || b.Level != BudgetOK {
		t.Fatalf("used = %+v", b)
	}
	if r := Build(sampleInput(), Options{}); r.Budget != nil {
		t.Fatal("no budget configured: want nil")
	}
}
