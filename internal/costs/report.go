// Package costs turns raw Cost Explorer numbers into a ranked, diffed report.
//
// Everything here is pure: no AWS calls, no I/O. That keeps the interesting
// logic — deltas, trend arrows, anomaly detection — trivially unit-testable
// against fixtures instead of a live billing account.
package costs

import (
	"fmt"
	"math"
	"sort"
)

// Period is one grouped Cost Explorer window: a display date range plus the
// spend per group key (service name or linked-account id).
type Period struct {
	Start   string             `json:"start"`
	End     string             `json:"end"`
	ByGroup map[string]float64 `json:"by_group"`
}

// Total sums every group in the period.
func (p Period) Total() float64 {
	var t float64
	for _, v := range p.ByGroup {
		t += v
	}
	return t
}

// Input is the normalized shape the report builder consumes. The fetcher
// (real AWS or a fixture) fills the three core windows; the rest is optional.
type Input struct {
	GroupBy   string `json:"group_by"` // "SERVICE" or "LINKED_ACCOUNT"
	Currency  string `json:"currency"`
	Yesterday Period `json:"yesterday"`
	DayBefore Period `json:"day_before"`
	MTD       Period `json:"mtd"`

	// LastMonth is the full previous calendar month, for the month-end
	// comparison. Nil when it was not fetched.
	LastMonth *Period `json:"last_month,omitempty"`
	// Forecast is Cost Explorer's forecast for the rest of the reported month
	// (today through month end). Nil when forecasting is off or failed.
	Forecast *Forecast `json:"forecast,omitempty"`
	// History holds one Period per day before Yesterday, oldest first, for the
	// rolling baseline. Empty unless baseline mode is on.
	History []Period `json:"history,omitempty"`
}

// Forecast is the forecast spend for [Start, End).
type Forecast struct {
	Start  string  `json:"start"`
	End    string  `json:"end"`
	Amount float64 `json:"amount"`
}

// Options tunes BuildReport. The zero value of every optional field turns
// that feature off.
type Options struct {
	TopN         int
	ThresholdPct float64 // DoD % increase that flags an anomaly (DoD mode)
	MinDelta     float64 // absolute increase an anomaly must clear; 0 means anomalyMinDelta

	// BaselineDays > 0 (and a non-empty Input.History) switches anomaly
	// detection from day-over-day to the rolling baseline: a group is flagged
	// when yesterday sits Sigma standard deviations above its baseline mean.
	BaselineDays int
	Sigma        float64

	// Budget is a monthly budget amount in the report currency; 0 means none.
	Budget     float64
	BudgetName string // display only
}

// Row is one line of the report: yesterday's spend for a group, how it moved
// versus the day before, and whether that move looks anomalous.
type Row struct {
	Name       string
	Amount     float64 // yesterday
	PrevAmount float64 // day before
	DoDDelta   float64 // Amount - PrevAmount
	DoDPct     float64 // percentage change vs PrevAmount
	Arrow      string
	Anomaly    bool

	// Baseline mode only.
	Baseline float64   // baseline mean for the matching weekday/weekend days
	Score    float64   // (Amount - Baseline) / baseline standard deviation
	Recent   []float64 // last seven days, oldest first, ending with Amount

	// Set by Reconcile when an alert state is in use.
	Ongoing bool   // already alerted in an earlier run
	Since   string // date the open alert was first raised

	// elevated keeps an open alert from resolving: in baseline mode the group
	// is still more than resolveSigma above its baseline (the spike itself
	// inflates the baseline, so "no longer anomalous" comes too early).
	elevated bool
}

// Report is the finished, render-ready summary.
type Report struct {
	Currency    string
	GroupBy     string
	Date        string // the "yesterday" date being reported
	Total       float64
	PrevTotal   float64
	TotalDoDPct float64
	MTDTotal    float64
	Rows        []Row // top-N by yesterday spend, descending
	Anomalies   []Row // every group flagged as anomalous (not only the top N)
	Threshold   float64

	// Baseline is non-nil when anomalies came from the rolling baseline.
	Baseline *BaselineInfo
	// Resolved lists alerts from an earlier run that are no longer
	// anomalous. Filled by Reconcile.
	Resolved []Resolved
	// MonthEnd is the projected (or, on the last day, final) month total.
	MonthEnd *MonthEnd
	// Budget is the budget status; nil when no budget is configured.
	Budget *BudgetStatus

	all map[string]Row // every row before the top-N cut, for Reconcile
}

// BaselineInfo describes the baseline settings a report was scored with.
type BaselineInfo struct {
	Days  int
	Sigma float64
}

const (
	arrowUp   = "▲"
	arrowDown = "▼"
	arrowFlat = "▬"

	// flatEpsilon: below this absolute daily move (in currency units) we call
	// it flat rather than showing a noisy arrow on rounding dust.
	flatEpsilon = 0.01

	// anomalyMinDelta: an anomaly must also clear this absolute day-over-day
	// increase. Percentage alone is noise — a service going $0.00 → $0.50 is a
	// "+100%" spike nobody wants paged about. Requiring real money filters it.
	anomalyMinDelta = 1.00

	// resolveSigma: in baseline mode an open alert resolves once the group is
	// back within this many standard deviations of its baseline.
	resolveSigma = 1.0
)

// pctChange returns the percentage change from prev to cur. A jump from zero
// spend is reported as +100% so a brand-new service still reads as a spike
// rather than a divide-by-zero.
func pctChange(cur, prev float64) float64 {
	if prev == 0 {
		if cur == 0 {
			return 0
		}
		return 100
	}
	return (cur - prev) / prev * 100
}

func arrowFor(delta float64) string {
	switch {
	case delta > flatEpsilon:
		return arrowUp
	case delta < -flatEpsilon:
		return arrowDown
	default:
		return arrowFlat
	}
}

// BuildReport ranks yesterday's groups, joins each to the day-before figure,
// keeps the top-N by spend, and flags a group as anomalous when its
// day-over-day increase exceeds thresholdPct (and the absolute move clears
// anomalyMinDelta, so a $0.001 → $0.002 group never trips the alarm).
func BuildReport(in Input, topN int, thresholdPct float64) Report {
	return Build(in, Options{TopN: topN, ThresholdPct: thresholdPct})
}

// Build is BuildReport with the full option set: rolling-baseline anomaly
// detection, month-end forecast and budget status.
func Build(in Input, o Options) Report {
	minDelta := o.MinDelta
	if minDelta <= 0 {
		minDelta = anomalyMinDelta
	}
	history := in.History
	if o.BaselineDays > 0 && len(history) > o.BaselineDays {
		history = history[len(history)-o.BaselineDays:]
	}
	useBaseline := o.BaselineDays > 0 && len(history) > 0

	rows := make([]Row, 0, len(in.Yesterday.ByGroup))
	for name, amt := range in.Yesterday.ByGroup {
		prev := in.DayBefore.ByGroup[name]
		delta := amt - prev
		rows = append(rows, Row{
			Name:       name,
			Amount:     amt,
			PrevAmount: prev,
			DoDDelta:   delta,
			DoDPct:     pctChange(amt, prev),
			Arrow:      arrowFor(delta),
		})
	}

	// A group that existed yesterday but vanished today would otherwise be
	// invisible; surface any day-before group that dropped to zero.
	for name, prev := range in.DayBefore.ByGroup {
		if _, ok := in.Yesterday.ByGroup[name]; ok {
			continue
		}
		if prev <= flatEpsilon {
			continue
		}
		rows = append(rows, Row{
			Name:       name,
			Amount:     0,
			PrevAmount: prev,
			DoDDelta:   -prev,
			DoDPct:     -100,
			Arrow:      arrowDown,
		})
	}

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Amount != rows[j].Amount {
			return rows[i].Amount > rows[j].Amount
		}
		return rows[i].Name < rows[j].Name
	})

	weekend := isWeekend(in.Yesterday.Start)
	var anomalies []Row
	for i := range rows {
		if useBaseline {
			mean, std := baseline(rows[i].Name, history, weekend)
			rows[i].Baseline = mean
			rows[i].Score = (rows[i].Amount - mean) / math.Max(std, flatEpsilon)
			rows[i].Recent = recent(rows[i].Name, history, rows[i].Amount)
			above := rows[i].Amount-mean >= minDelta
			rows[i].Anomaly = above && rows[i].Score >= o.Sigma
			rows[i].elevated = above && rows[i].Score > resolveSigma
		} else {
			rows[i].Anomaly = rows[i].DoDDelta >= minDelta && rows[i].DoDPct >= o.ThresholdPct
			rows[i].elevated = rows[i].Anomaly
		}
		if rows[i].Anomaly {
			anomalies = append(anomalies, rows[i])
		}
	}

	all := make(map[string]Row, len(rows))
	for _, row := range rows {
		all[row.Name] = row
	}
	if o.TopN > 0 && len(rows) > o.TopN {
		rows = rows[:o.TopN]
	}

	total := in.Yesterday.Total()
	prevTotal := in.DayBefore.Total()
	mtd := in.MTD.Total()

	r := Report{
		Currency:    in.Currency,
		GroupBy:     in.GroupBy,
		Date:        in.Yesterday.Start,
		Total:       total,
		PrevTotal:   prevTotal,
		TotalDoDPct: pctChange(total, prevTotal),
		MTDTotal:    mtd,
		Rows:        rows,
		Anomalies:   anomalies,
		Threshold:   o.ThresholdPct,
		all:         all,
	}
	if useBaseline {
		r.Baseline = &BaselineInfo{Days: len(history), Sigma: o.Sigma}
	}
	r.MonthEnd = monthEnd(in, mtd)
	r.Budget = budgetStatus(o.Budget, o.BudgetName, mtd, r.MonthEnd)
	return r
}

// Money formats an amount with its currency for display, e.g. "$12.34" for USD
// or "12.34 EUR" for anything without a known symbol.
func Money(currency string, v float64) string {
	if sym, ok := symbols[currency]; ok {
		return fmt.Sprintf("%s%s", sym, humanAmount(v))
	}
	return fmt.Sprintf("%s %s", humanAmount(v), currency)
}

var symbols = map[string]string{
	"USD": "$",
	"EUR": "€",
	"GBP": "£",
}

// humanAmount rounds to cents and avoids "-0.00".
func humanAmount(v float64) string {
	r := math.Round(v*100) / 100
	if r == 0 {
		r = 0
	}
	return fmt.Sprintf("%.2f", r)
}

// SignedPct renders a percentage with an explicit sign, e.g. "+42.0%".
func SignedPct(v float64) string {
	return fmt.Sprintf("%+.1f%%", v)
}
