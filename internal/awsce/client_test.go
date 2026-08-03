package awsce

import (
	"context"
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
