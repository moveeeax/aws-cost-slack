// Package awsce wraps the AWS Cost Explorer GetCostAndUsage API and normalizes
// its nested response into the flat costs.Input the report builder wants.
package awsce

import (
	"context"
	"fmt"
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
}

// Fetcher pulls the three windows a report needs (yesterday, day-before, MTD).
type Fetcher struct {
	api     API
	groupBy string
	metric  string
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

	return costs.Input{
		GroupBy:   f.groupBy,
		Currency:  cur,
		Yesterday: yesterday,
		DayBefore: dayBefore,
		MTD:       mtd,
	}, nil
}

// window queries one time range, paging through NextPageToken, and folds every
// grouped bucket into a single map keyed by group name.
func (f *Fetcher) window(ctx context.Context, start, end time.Time) (costs.Period, string, error) {
	period := costs.Period{
		Start:   start.Format(dateLayout),
		End:     end.Format(dateLayout),
		ByGroup: map[string]float64{},
	}
	var currency string
	var token *string

	for {
		out, err := f.api.GetCostAndUsage(ctx, &ce.GetCostAndUsageInput{
			TimePeriod: &cetypes.DateInterval{
				Start: aws.String(period.Start),
				End:   aws.String(period.End),
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
			return costs.Period{}, "", err
		}

		for _, rbt := range out.ResultsByTime {
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
					return costs.Period{}, "", fmt.Errorf("parse amount %q for %q: %w", *mv.Amount, name, err)
				}
				period.ByGroup[name] += amt
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

	return period, currency, nil
}
