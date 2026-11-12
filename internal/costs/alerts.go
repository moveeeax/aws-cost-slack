package costs

import "sort"

// AlertState is what persists between runs so one spike alerts once, not once
// per run. Open is keyed by alert fingerprint (group-by dimension + group).
type AlertState struct {
	Open map[string]OpenAlert `json:"open"`
}

// OpenAlert is an anomaly that has been alerted and not yet resolved.
type OpenAlert struct {
	GroupBy string  `json:"group_by"`
	Name    string  `json:"name"`
	Since   string  `json:"since"`  // report date of the first alert
	Amount  float64 `json:"amount"` // spend on that date
}

// Resolved is an open alert whose group is no longer anomalous.
type Resolved struct {
	Name     string
	Amount   float64 // spend on the report date
	Baseline float64 // baseline mean, in baseline mode
	Since    string
}

func alertKey(groupBy, name string) string { return groupBy + "/" + name }

// Reconcile compares the report's anomalies with the alerts opened by earlier
// runs. Anomalies already open are marked Ongoing; open alerts whose group is
// back to normal move to r.Resolved. In baseline mode "back to normal" means
// within resolveSigma of the baseline, not merely under Sigma; in DoD mode it
// means no longer flagged. It returns the state to save.
// Entries for another group-by dimension are kept untouched, so one state file
// can serve both SERVICE and LINKED_ACCOUNT runs.
func (r *Report) Reconcile(st AlertState) AlertState {
	next := AlertState{Open: map[string]OpenAlert{}}
	current := map[string]bool{}

	for i := range r.Anomalies {
		a := &r.Anomalies[i]
		key := alertKey(r.GroupBy, a.Name)
		current[key] = true
		if prev, ok := st.Open[key]; ok {
			a.Ongoing = true
			a.Since = prev.Since
			next.Open[key] = prev
			continue
		}
		a.Since = r.Date
		next.Open[key] = OpenAlert{GroupBy: r.GroupBy, Name: a.Name, Since: r.Date, Amount: a.Amount}
	}

	r.Resolved = nil
	for key, prev := range st.Open {
		if prev.GroupBy != r.GroupBy {
			next.Open[key] = prev
			continue
		}
		if current[key] {
			continue
		}
		row := r.all[prev.Name]
		if row.elevated {
			next.Open[key] = prev
			continue
		}
		r.Resolved = append(r.Resolved, Resolved{
			Name:     prev.Name,
			Amount:   row.Amount,
			Baseline: row.Baseline,
			Since:    prev.Since,
		})
	}
	sort.Slice(r.Resolved, func(i, j int) bool { return r.Resolved[i].Name < r.Resolved[j].Name })

	// Keep the top-N rows consistent with the anomaly list.
	for i := range r.Rows {
		for _, a := range r.Anomalies {
			if a.Name == r.Rows[i].Name {
				r.Rows[i].Ongoing, r.Rows[i].Since = a.Ongoing, a.Since
			}
		}
	}
	return next
}

// NewAnomalies counts anomalies that were not already alerted.
func (r Report) NewAnomalies() int {
	n := 0
	for _, a := range r.Anomalies {
		if !a.Ongoing {
			n++
		}
	}
	return n
}

// HasAlerts reports whether the report carries something worth an alert: a
// new anomaly or a resolved one.
func (r Report) HasAlerts() bool {
	return r.NewAnomalies() > 0 || len(r.Resolved) > 0
}

// Event is one alert a backtest would have sent.
type Event struct {
	Date  string
	Kind  string // EventFire or EventResolve
	Row   Row    // for EventResolve only Name, Amount and Baseline are set
	Since string // EventResolve: date the alert was raised
}

const (
	EventFire    = "fire"
	EventResolve = "resolve"
)

// Backtest replays detection over a daily series (oldest first) as if the
// tool had run once per day with an alert state, and returns every alert that
// would have fired or resolved. Each replayed day is scored against the
// o.BaselineDays days before it (or the single day before it in DoD mode), so
// the first scored day is days[max(o.BaselineDays, 1)].
func Backtest(groupBy, currency string, days []Period, o Options) []Event {
	o.TopN, o.Budget = 0, 0
	first := o.BaselineDays
	if first < 1 {
		first = 1
	}
	var events []Event
	st := AlertState{}
	for i := first; i < len(days); i++ {
		in := Input{
			GroupBy:   groupBy,
			Currency:  currency,
			Yesterday: days[i],
			DayBefore: days[i-1],
		}
		if o.BaselineDays > 0 {
			in.History = days[i-o.BaselineDays : i]
		}
		r := Build(in, o)
		st = r.Reconcile(st)
		for _, a := range r.Anomalies {
			if !a.Ongoing {
				events = append(events, Event{Date: r.Date, Kind: EventFire, Row: a})
			}
		}
		for _, res := range r.Resolved {
			events = append(events, Event{
				Date:  r.Date,
				Kind:  EventResolve,
				Row:   Row{Name: res.Name, Amount: res.Amount, Baseline: res.Baseline},
				Since: res.Since,
			})
		}
	}
	return events
}
