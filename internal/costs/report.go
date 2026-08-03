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
// (real AWS or a fixture) fills these three windows.
type Input struct {
	GroupBy   string `json:"group_by"` // "SERVICE" or "LINKED_ACCOUNT"
	Currency  string `json:"currency"`
	Yesterday Period `json:"yesterday"`
	DayBefore Period `json:"day_before"`
	MTD       Period `json:"mtd"`
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
	Anomalies   []Row // subset of Rows flagged over threshold
	Threshold   float64
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
// flatEpsilon, so a $0.001 → $0.002 group never trips the alarm).
func BuildReport(in Input, topN int, thresholdPct float64) Report {
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

	var anomalies []Row
	for i := range rows {
		if rows[i].DoDDelta >= anomalyMinDelta && rows[i].DoDPct >= thresholdPct {
			rows[i].Anomaly = true
			anomalies = append(anomalies, rows[i])
		}
	}

	if topN > 0 && len(rows) > topN {
		rows = rows[:topN]
	}

	total := in.Yesterday.Total()
	prevTotal := in.DayBefore.Total()

	return Report{
		Currency:    in.Currency,
		GroupBy:     in.GroupBy,
		Date:        in.Yesterday.Start,
		Total:       total,
		PrevTotal:   prevTotal,
		TotalDoDPct: pctChange(total, prevTotal),
		MTDTotal:    in.MTD.Total(),
		Rows:        rows,
		Anomalies:   anomalies,
		Threshold:   thresholdPct,
	}
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
