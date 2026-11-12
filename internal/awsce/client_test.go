package awsce

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ce "github.com/aws/aws-sdk-go-v2/service/costexplorer"
	cetypes "github.com/aws/aws-sdk-go-v2/service/costexplorer/types"
)

// fakeAPI returns a canned response per query start date and records the
// windows it was asked for so we can assert the date math.
type fakeAPI struct {
	byStart map[string]*ce.GetCostAndUsageOutput
	seen    []string

	forecast    *ce.GetCostForecastOutput
	forecastErr error
	forecastIn  []*ce.GetCostForecastInput
}

func (f *fakeAPI) GetCostForecast(_ context.Context, in *ce.GetCostForecastInput, _ ...func(*ce.Options)) (*ce.GetCostForecastOutput, error) {
	f.forecastIn = append(f.forecastIn, in)
	if f.forecastErr != nil {
		return nil, f.forecastErr
	}
	if f.forecast != nil {
		return f.forecast, nil
	}
	return &ce.GetCostForecastOutput{}, nil
}

func (f *fakeAPI) GetCostAndUsage(_ context.Context, in *ce.GetCostAndUsageInput, _ ...func(*ce.Options)) (*ce.GetCostAndUsageOutput, error) {
	start := *in.TimePeriod.Start
	f.seen = append(f.seen, start+".."+*in.TimePeriod.End)
	if out, ok := f.byStart[start]; ok {
		return out, nil
	}
	return &ce.GetCostAndUsageOutput{}, nil
}

func grp(name, amount string) cetypes.Group {
	return cetypes.Group{
		Keys: []string{name},
		Metrics: map[string]cetypes.MetricValue{
			"UnblendedCost": {Amount: aws.String(amount), Unit: aws.String("USD")},
		},
	}
}

func out(groups ...cetypes.Group) *ce.GetCostAndUsageOutput {
	return &ce.GetCostAndUsageOutput{
		ResultsByTime: []cetypes.ResultByTime{{Groups: groups}},
	}
}

func TestFetchNormalizesWindows(t *testing.T) {
	now := time.Date(2026, 8, 3, 6, 0, 0, 0, time.UTC) // report on Aug 2
	api := &fakeAPI{byStart: map[string]*ce.GetCostAndUsageOutput{
		"2026-08-02": out(grp("Amazon EC2", "120.0"), grp("Amazon S3", "15.5")), // yesterday
		"2026-08-01": out(grp("Amazon EC2", "118.0")),                           // day before / month start
	}}

	f := New(api, "SERVICE", "UnblendedCost")
	in, err := f.Fetch(context.Background(), now)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}

	if in.Currency != "USD" {
		t.Errorf("currency = %q", in.Currency)
	}
	if in.Yesterday.ByGroup["Amazon EC2"] != 120.0 || in.Yesterday.ByGroup["Amazon S3"] != 15.5 {
		t.Errorf("yesterday parsed wrong: %+v", in.Yesterday.ByGroup)
	}
	if in.DayBefore.ByGroup["Amazon EC2"] != 118.0 {
		t.Errorf("day-before parsed wrong: %+v", in.DayBefore.ByGroup)
	}

	// windows requested: yesterday [08-02,08-03), day-before [07-31,08-02)?? no:
	// yStart=08-02, dbStart=07-31? day before is [yEnd-2, yStart) = [07-31,08-02)
	wantWindows := map[string]bool{
		"2026-08-02..2026-08-03": true, // yesterday [Aug 2, Aug 3)
		"2026-08-01..2026-08-02": true, // day-before [Aug 1, Aug 2)
		"2026-08-01..2026-08-03": true, // MTD [month start, Aug 3)
	}
	for _, w := range api.seen {
		delete(wantWindows, w)
	}
	if len(wantWindows) != 0 {
		t.Fatalf("missing/incorrect windows, still expected: %v (got %v)", wantWindows, api.seen)
	}
}

func TestFetchPagesTokens(t *testing.T) {
	// A window whose first page carries a NextPageToken must be fully drained.
	page2 := out(grp("Amazon S3", "5.0"))
	page1 := out(grp("Amazon S3", "10.0"))
	page1.NextPageToken = aws.String("more")

	calls := 0
	api := paging(func() *ce.GetCostAndUsageOutput {
		calls++
		if calls == 1 {
			return page1
		}
		return page2
	})

	f := New(api, "SERVICE", "")
	p, _, err := f.window(context.Background(), time.Now().UTC(), time.Now().UTC())
	if err != nil {
		t.Fatalf("window: %v", err)
	}
	if p.ByGroup["Amazon S3"] != 15.0 {
		t.Fatalf("paging did not accumulate both pages: %+v", p.ByGroup)
	}
}

type pagingAPI struct {
	next func() *ce.GetCostAndUsageOutput
}

func paging(next func() *ce.GetCostAndUsageOutput) *pagingAPI { return &pagingAPI{next: next} }

func (p *pagingAPI) GetCostAndUsage(_ context.Context, _ *ce.GetCostAndUsageInput, _ ...func(*ce.Options)) (*ce.GetCostAndUsageOutput, error) {
	return p.next(), nil
}

func (p *pagingAPI) GetCostForecast(_ context.Context, _ *ce.GetCostForecastInput, _ ...func(*ce.Options)) (*ce.GetCostForecastOutput, error) {
	return nil, errors.New("not used")
}

func TestFetchForecastAndLastMonth(t *testing.T) {
	now := time.Date(2026, 8, 3, 6, 0, 0, 0, time.UTC) // report on Aug 2
	api := &fakeAPI{
		byStart: map[string]*ce.GetCostAndUsageOutput{
			"2026-07-01": out(grp("Amazon EC2", "3000.0")), // last month
		},
		forecast: &ce.GetCostForecastOutput{
			Total: &cetypes.MetricValue{Amount: aws.String("3400.5"), Unit: aws.String("USD")},
		},
	}
	f := New(api, "SERVICE", "UnblendedCost")
	f.Forecast = true
	in, err := f.Fetch(context.Background(), now)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}

	if in.LastMonth == nil || in.LastMonth.Start != "2026-07-01" || in.LastMonth.End != "2026-08-01" {
		t.Fatalf("last month window wrong: %+v", in.LastMonth)
	}
	if in.LastMonth.Total() != 3000 {
		t.Errorf("last month total = %v", in.LastMonth.Total())
	}
	if in.Forecast == nil || in.Forecast.Amount != 3400.5 {
		t.Fatalf("forecast not parsed: %+v", in.Forecast)
	}
	if len(api.forecastIn) != 1 {
		t.Fatalf("want 1 forecast call, got %d", len(api.forecastIn))
	}
	q := api.forecastIn[0]
	// Rest of month: today (Aug 3) through Sep 1, exclusive end.
	if *q.TimePeriod.Start != "2026-08-03" || *q.TimePeriod.End != "2026-09-01" {
		t.Errorf("forecast window = %s..%s", *q.TimePeriod.Start, *q.TimePeriod.End)
	}
	if q.Metric != cetypes.MetricUnblendedCost || q.Granularity != cetypes.GranularityMonthly {
		t.Errorf("forecast metric/granularity = %s/%s", q.Metric, q.Granularity)
	}
}

func TestFetchSkipsForecastAfterMonthEnd(t *testing.T) {
	now := time.Date(2026, 9, 1, 6, 0, 0, 0, time.UTC) // report on Aug 31
	api := &fakeAPI{}
	f := New(api, "SERVICE", "UnblendedCost")
	f.Forecast = true
	in, err := f.Fetch(context.Background(), now)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(api.forecastIn) != 0 || in.Forecast != nil {
		t.Fatalf("no forecast expected once the month is over, got %d calls", len(api.forecastIn))
	}
	if in.LastMonth == nil || in.LastMonth.Start != "2026-07-01" {
		t.Fatalf("last month should still be July: %+v", in.LastMonth)
	}
}

func TestFetchForecastFailureIsAWarning(t *testing.T) {
	now := time.Date(2026, 8, 3, 6, 0, 0, 0, time.UTC)
	api := &fakeAPI{forecastErr: errors.New("AccessDeniedException")}
	var warnings []string
	f := New(api, "SERVICE", "UnblendedCost")
	f.Forecast = true
	f.Warnf = func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }

	in, err := f.Fetch(context.Background(), now)
	if err != nil {
		t.Fatalf("forecast failure must not fail the fetch: %v", err)
	}
	if in.Forecast != nil {
		t.Errorf("forecast should be nil after failure")
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "AccessDeniedException") {
		t.Fatalf("want one forecast warning, got %v", warnings)
	}
}

func TestForecastRejectsUsageMetric(t *testing.T) {
	f := New(&fakeAPI{}, "SERVICE", "UsageQuantity")
	if _, err := f.forecast(context.Background(), time.Now(), time.Now().AddDate(0, 0, 1)); err == nil {
		t.Fatal("UsageQuantity has no cost forecast; want an error")
	}
}

func TestDailyBucketsByDayAndFillsGaps(t *testing.T) {
	day := func(d string, groups ...cetypes.Group) cetypes.ResultByTime {
		return cetypes.ResultByTime{
			TimePeriod: &cetypes.DateInterval{Start: aws.String(d)},
			Groups:     groups,
		}
	}
	// Aug 2 is split across two pages; Aug 3 has no data at all.
	page1 := &ce.GetCostAndUsageOutput{
		ResultsByTime: []cetypes.ResultByTime{
			day("2026-08-01", grp("Amazon EC2", "10")),
			day("2026-08-02", grp("Amazon EC2", "11")),
		},
		NextPageToken: aws.String("more"),
	}
	page2 := &ce.GetCostAndUsageOutput{
		ResultsByTime: []cetypes.ResultByTime{day("2026-08-02", grp("Amazon S3", "2"))},
	}
	calls := 0
	api := paging(func() *ce.GetCostAndUsageOutput {
		calls++
		if calls == 1 {
			return page1
		}
		return page2
	})

	f := New(api, "SERVICE", "")
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	days, cur, err := f.Daily(context.Background(), start, start.AddDate(0, 0, 3))
	if err != nil {
		t.Fatalf("daily: %v", err)
	}
	if cur != "USD" {
		t.Errorf("currency = %q", cur)
	}
	if len(days) != 3 {
		t.Fatalf("want 3 days, got %d: %+v", len(days), days)
	}
	if days[0].Start != "2026-08-01" || days[0].End != "2026-08-02" || days[0].ByGroup["Amazon EC2"] != 10 {
		t.Errorf("day 1 wrong: %+v", days[0])
	}
	if days[1].ByGroup["Amazon EC2"] != 11 || days[1].ByGroup["Amazon S3"] != 2 {
		t.Errorf("day 2 should merge both pages: %+v", days[1])
	}
	if days[2].Start != "2026-08-03" || len(days[2].ByGroup) != 0 {
		t.Errorf("gap day should be present and empty: %+v", days[2])
	}
}

func TestFetchBaselineHistoryWindow(t *testing.T) {
	now := time.Date(2026, 8, 3, 6, 0, 0, 0, time.UTC) // report on Aug 2
	api := &fakeAPI{}
	f := New(api, "SERVICE", "UnblendedCost")
	f.BaselineDays = 14
	in, err := f.Fetch(context.Background(), now)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(in.History) != 14 || in.History[0].Start != "2026-07-19" || in.History[13].Start != "2026-08-01" {
		t.Fatalf("history should be the 14 days before Aug 2, got %d starting %v", len(in.History), in.History)
	}
	found := false
	for _, w := range api.seen {
		if w == "2026-07-19..2026-08-02" {
			found = true
		}
	}
	if !found {
		t.Fatalf("history window not requested: %v", api.seen)
	}
}
