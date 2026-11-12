package costs

import (
	"math"
	"time"
)

const (
	// minSeasonalDays: with fewer matching weekday (or weekend) days than this
	// in the window, the baseline falls back to every day in the window. Two
	// Saturdays are not enough to say what a Saturday costs.
	minSeasonalDays = 3

	// sparkDays is how many days the per-group sparkline covers.
	sparkDays = 7
)

// isWeekend reports whether a YYYY-MM-DD date is a Saturday or Sunday. An
// unparsable date counts as a weekday.
func isWeekend(date string) bool {
	t, err := time.Parse(dateLayout, date)
	if err != nil {
		return false
	}
	wd := t.Weekday()
	return wd == time.Saturday || wd == time.Sunday
}

// baseline returns the mean and sample standard deviation of a group's daily
// spend over the history days of the same kind (weekday or weekend) as the
// day being scored. A day with no entry for the group counts as zero spend.
func baseline(name string, history []Period, weekend bool) (mean, std float64) {
	var vals []float64
	for _, d := range history {
		if isWeekend(d.Start) == weekend {
			vals = append(vals, d.ByGroup[name])
		}
	}
	if len(vals) < minSeasonalDays {
		vals = vals[:0]
		for _, d := range history {
			vals = append(vals, d.ByGroup[name])
		}
	}
	return meanStd(vals)
}

func meanStd(vals []float64) (mean, std float64) {
	if len(vals) == 0 {
		return 0, 0
	}
	for _, v := range vals {
		mean += v
	}
	mean /= float64(len(vals))
	if len(vals) < 2 {
		return mean, 0
	}
	var ss float64
	for _, v := range vals {
		ss += (v - mean) * (v - mean)
	}
	return mean, math.Sqrt(ss / float64(len(vals)-1))
}

// recent returns the group's spend for the last sparkDays days, oldest first,
// ending with today's amount.
func recent(name string, history []Period, today float64) []float64 {
	start := len(history) - (sparkDays - 1)
	if start < 0 {
		start = 0
	}
	out := make([]float64, 0, sparkDays)
	for _, d := range history[start:] {
		out = append(out, d.ByGroup[name])
	}
	return append(out, today)
}
