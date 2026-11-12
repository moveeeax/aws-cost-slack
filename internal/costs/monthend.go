package costs

import "time"

const dateLayout = "2006-01-02"

// Budget levels. A budget warns at budgetWarnPct of the limit spent and is
// over at 100%.
const (
	BudgetOK   = "ok"
	BudgetWarn = "warn"
	BudgetOver = "over"

	budgetWarnPct = 80.0
)

// MonthEnd is the month total the report projects: month-to-date plus the
// Cost Explorer forecast for the remaining days, or the actual total once the
// reported day is the last of its month.
type MonthEnd struct {
	Total          float64
	Final          bool    // the month is over; Total is MTD, no forecast involved
	LastMonth      float64 // full previous month; meaningful only if HasLastMonth
	HasLastMonth   bool
	VsLastMonthPct float64
}

// BudgetStatus is spend against a monthly budget.
type BudgetStatus struct {
	Name        string
	Amount      float64
	Spent       float64 // month to date
	UsedPct     float64 // Spent / Amount
	ForecastPct float64 // MonthEnd.Total / Amount; meaningful only if HasForecast
	HasForecast bool
	Level       string // BudgetOK, BudgetWarn or BudgetOver, from UsedPct
}

// monthEnd projects the month total. It returns nil when the month is still
// running and there is no forecast to project with.
func monthEnd(in Input, mtd float64) *MonthEnd {
	var me MonthEnd
	switch {
	case lastDayOfMonth(in.Yesterday):
		me = MonthEnd{Total: mtd, Final: true}
	case in.Forecast != nil:
		me = MonthEnd{Total: mtd + in.Forecast.Amount}
	default:
		return nil
	}
	if in.LastMonth != nil {
		if last := in.LastMonth.Total(); last > 0 {
			me.LastMonth = last
			me.HasLastMonth = true
			me.VsLastMonthPct = pctChange(me.Total, last)
		}
	}
	return &me
}

// lastDayOfMonth reports whether the one-day period p is the last day of its
// month (its exclusive end falls on the 1st).
func lastDayOfMonth(p Period) bool {
	end, err := time.Parse(dateLayout, p.End)
	if err != nil {
		start, err := time.Parse(dateLayout, p.Start)
		if err != nil {
			return false
		}
		end = start.AddDate(0, 0, 1)
	}
	return end.Day() == 1
}

func budgetStatus(amount float64, name string, mtd float64, me *MonthEnd) *BudgetStatus {
	if amount <= 0 {
		return nil
	}
	b := &BudgetStatus{Name: name, Amount: amount, Spent: mtd, UsedPct: mtd / amount * 100}
	if me != nil {
		b.ForecastPct = me.Total / amount * 100
		b.HasForecast = true
	}
	switch {
	case b.UsedPct >= 100:
		b.Level = BudgetOver
	case b.UsedPct >= budgetWarnPct:
		b.Level = BudgetWarn
	default:
		b.Level = BudgetOK
	}
	return b
}
