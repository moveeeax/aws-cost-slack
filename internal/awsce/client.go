// Package awsce wraps the AWS Cost Explorer GetCostAndUsage and GetCostForecast
// APIs and normalizes their nested responses into the flat costs.Input the
// report builder wants.
package awsce

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ce "github.com/aws/aws-sdk-go-v2/service/costexplorer"
	cetypes "github.com/aws/aws-sdk-go-v2/service/costexplorer/types"

	"github.com/moveeeax/aws-cost-slack/internal/costs"
)

const dateLayout = "2006-01-02"

// API is the slice of the Cost Explorer client we use. Narrowing to an
// interface lets tests inject a fake without touching AWS.
type API interface {
	GetCostAndUsage(ctx context.Context, in *ce.GetCostAndUsageInput, optFns ...func(*ce.Options)) (*ce.GetCostAndUsageOutput, error)
	GetCostForecast(ctx context.Context, in *ce.GetCostForecastInput, optFns ...func(*ce.Options)) (*ce.GetCostForecastOutput, error)
}

// Fetcher pulls the three windows a report needs (yesterday, day-before, MTD)
// plus the optional forecast and baseline history.
type Fetcher struct {
	api     API
	groupBy string
	metric  string

	// Forecast also fetches the previous full month and the Cost Explorer
	// forecast for the rest of the reported month.
	Forecast bool
	// BaselineDays > 0 also fetches that many days of daily history before
	// the reported day.
	BaselineDays int
	// Warnf, when set, receives non-fatal problems (a failed forecast). The
	// report still goes out without the missing piece.
	Warnf func(format string, args ...any)
}

// forecastMetrics maps GetCostAndUsage metric names to the GetCostForecast
// enum. Usage metrics have no cost forecast and are left out.
var forecastMetrics = map[string]cetypes.Metric{
	"UnblendedCost":    cetypes.MetricUnblendedCost,
	"AmortizedCost":    cetypes.MetricAmortizedCost,
	"BlendedCost":      cetypes.MetricBlendedCost,
	"NetUnblendedCost": cetypes.MetricNetUnblendedCost,
	"NetAmortizedCost": cetypes.MetricNetAmortizedCost,
}

// New builds a Fetcher. groupBy is a Cost Explorer DIMENSION such as SERVICE or
// LINKED_ACCOUNT; metric is a cost metric such as UnblendedCost.
func New(api API, groupBy, metric string) *Fetcher {
	if metric == "" {
		metric = "UnblendedCost"
	}
	return &Fetcher{api: api, groupBy: groupBy, metric: metric}
}

// Fetch returns the normalized input for the day ending at `now` (i.e. it
// reports on the full calendar day before now). Cost Explorer treats the End
// date as exclusive, so a one-day window is [day, day+1).
func (f *Fetcher) Fetch(ctx context.Context, now time.Time) (costs.Input, error) {
	yEnd := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	yStart := yEnd.AddDate(0, 0, -1)
	dbStart := yEnd.AddDate(0, 0, -2)
	monthStart := time.Date(yStart.Year(), yStart.Month(), 1, 0, 0, 0, 0, time.UTC)

	yesterday, cur, err := f.window(ctx, yStart, yEnd)
	if err != nil {
		return costs.Input{}, fmt.Errorf("yesterday window: %w", err)
	}
	dayBefore, _, err := f.window(ctx, dbStart, yStart)
	if err != nil {
		return costs.Input{}, fmt.Errorf("day-before window: %w", err)
	}
	mtd, curMTD, err := f.window(ctx, monthStart, yEnd)
	if err != nil {
		return costs.Input{}, fmt.Errorf("month-to-date window: %w", err)
	}
	if cur == "" {
		cur = curMTD
	}
	if cur == "" {
		cur = "USD"
	}

	in := costs.Input{
		GroupBy:   f.groupBy,
		Currency:  cur,
		Yesterday: yesterday,
		DayBefore: dayBefore,
		MTD:       mtd,
	}

	if f.BaselineDays > 0 {
		hist, _, err := f.Daily(ctx, yStart.AddDate(0, 0, -f.BaselineDays), yStart)
		if err != nil {
			return costs.Input{}, fmt.Errorf("baseline history: %w", err)
		}
		in.History = hist
	}

	if f.Forecast {
		lastMonth, _, err := f.window(ctx, monthStart.AddDate(0, -1, 0), monthStart)
		if err != nil {
			f.warnf("last-month window: %v", err)
		} else {
			in.LastMonth = &lastMonth
		}
		// Once the reported day closes its month there is nothing left to
		// forecast: the month total is MTD.
		if nextMonth := monthStart.AddDate(0, 1, 0); yEnd.Before(nextMonth) {
			fc, err := f.forecast(ctx, yEnd, nextMonth)
			if err != nil {
				f.warnf("forecast: %v", err)
			} else {
				in.Forecast = fc
			}
		}
	}

	return in, nil
}

func (f *Fetcher) warnf(format string, args ...any) {
	if f.Warnf != nil {
		f.Warnf(format, args...)
	}
}

// forecast asks Cost Explorer for the spend forecast over [start, end), where
// start is today (the API rejects a start in the past).
func (f *Fetcher) forecast(ctx context.Context, start, end time.Time) (*costs.Forecast, error) {
	metric, ok := forecastMetrics[f.metric]
	if !ok {
		return nil, fmt.Errorf("metric %s has no cost forecast", f.metric)
	}
	fc := &costs.Forecast{Start: start.Format(dateLayout), End: end.Format(dateLayout)}
	out, err := f.api.GetCostForecast(ctx, &ce.GetCostForecastInput{
		TimePeriod: &cetypes.DateInterval{
			Start: aws.String(fc.Start),
			End:   aws.String(fc.End),
		},
		Granularity: cetypes.GranularityMonthly,
		Metric:      metric,
	})
	if err != nil {
		return nil, err
	}
	if out.Total == nil || out.Total.Amount == nil {
		return nil, fmt.Errorf("forecast response has no total")
	}
	amt, err := strconv.ParseFloat(*out.Total.Amount, 64)
	if err != nil {
		return nil, fmt.Errorf("parse forecast amount %q: %w", *out.Total.Amount, err)
	}
	fc.Amount = amt
	return fc, nil
}

// Daily returns one Period per day in [start, end), oldest first, grouped
// like the report. Days Cost Explorer returns nothing for are present with an
// empty map, so the series has no gaps.
func (f *Fetcher) Daily(ctx context.Context, start, end time.Time) ([]costs.Period, string, error) {
	byDay := map[string]map[string]float64{}
	for d := start; d.Before(end); d = d.AddDate(0, 0, 1) {
		byDay[d.Format(dateLayout)] = map[string]float64{}
	}
	currency, err := f.query(ctx, start, end, func(day, name string, amt float64) {
		if m, ok := byDay[day]; ok {
			m[name] += amt
		}
	})
	if err != nil {
		return nil, "", err
	}

	days := make([]costs.Period, 0, len(byDay))
	for day, groups := range byDay {
		t, _ := time.Parse(dateLayout, day)
		days = append(days, costs.Period{
			Start:   day,
			End:     t.AddDate(0, 0, 1).Format(dateLayout),
			ByGroup: groups,
		})
	}
	sort.Slice(days, func(i, j int) bool { return days[i].Start < days[j].Start })
	return days, currency, nil
}

// window queries one time range, paging through NextPageToken, and folds every
// grouped bucket into a single map keyed by group name.
func (f *Fetcher) window(ctx context.Context, start, end time.Time) (costs.Period, string, error) {
	period := costs.Period{
		Start:   start.Format(dateLayout),
		End:     end.Format(dateLayout),
		ByGroup: map[string]float64{},
	}
	currency, err := f.query(ctx, start, end, func(_, name string, amt float64) {
		period.ByGroup[name] += amt
	})
	if err != nil {
		return costs.Period{}, "", err
	}
	return period, currency, nil
}

// query runs a DAILY, grouped GetCostAndUsage over [start, end), drains every
// page, and calls add once per (day, group) bucket. It returns the currency
// of the first bucket that carries one.
func (f *Fetcher) query(ctx context.Context, start, end time.Time, add func(day, name string, amt float64)) (string, error) {
	var currency string
	var token *string

	for {
		out, err := f.api.GetCostAndUsage(ctx, &ce.GetCostAndUsageInput{
			TimePeriod: &cetypes.DateInterval{
				Start: aws.String(start.Format(dateLayout)),
				End:   aws.String(end.Format(dateLayout)),
			},
			Granularity: cetypes.GranularityDaily,
			Metrics:     []string{f.metric},
			GroupBy: []cetypes.GroupDefinition{{
				Type: cetypes.GroupDefinitionTypeDimension,
				Key:  aws.String(f.groupBy),
			}},
			NextPageToken: token,
		})
		if err != nil {
			return "", err
		}

		for _, rbt := range out.ResultsByTime {
			var day string
			if rbt.TimePeriod != nil && rbt.TimePeriod.Start != nil {
				day = *rbt.TimePeriod.Start
			}
			for _, g := range rbt.Groups {
				if len(g.Keys) == 0 {
					continue
				}
				name := g.Keys[0]
				mv, ok := g.Metrics[f.metric]
				if !ok || mv.Amount == nil {
					continue
				}
				amt, err := strconv.ParseFloat(*mv.Amount, 64)
				if err != nil {
					return "", fmt.Errorf("parse amount %q for %q: %w", *mv.Amount, name, err)
				}
				add(day, name, amt)
				if currency == "" && mv.Unit != nil {
					currency = *mv.Unit
				}
			}
		}

		if out.NextPageToken == nil || *out.NextPageToken == "" {
			break
		}
		token = out.NextPageToken
	}

	return currency, nil
}
