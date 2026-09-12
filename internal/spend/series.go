package spend

import (
	"fmt"
	"time"
)

// Point is one day of the money chart. Both figures are pointers because a
// day with no data and a day of zero spend are different facts: the snapshot
// only carries a key for a date some transcript line landed on, and a chart
// that drew the gaps as zero would be claiming quiet days it never observed.
type Point struct {
	Date      string   `json:"date"`
	AgentUSD  *float64 `json:"agentUsd"`
	BilledUSD *float64 `json:"billedUsd"`
}

// Series is the by-day view of one window ending today.
type Series struct {
	Days         int     `json:"days"`
	Points       []Point `json:"points"`
	CoverageDays int     `json:"coverageDays"`
	Warning      string  `json:"warning,omitempty"`
}

// Series projects ByDay onto the last days calendar days.
//
// Coverage is measured rather than assumed. looseapi reads the transcripts
// modified inside its own window (Usage.Days) and books every line in them by
// date, so ByDay reaches further back than that window, but only through
// sessions that happened to still be open inside it. A 90-day request against
// a 30-day scan gets the dates it has, a count of how many that is, and a
// sentence saying why the rest are empty, instead of sixty zeros.
//
// Dates are local calendar days looked up by the same string TodayCents uses;
// billed charges are placed on the local day they landed.
func (s Snapshot) Series(now time.Time, days int) Series {
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	billed := s.chargesByDay(now.Location())
	out := Series{Days: days, Points: make([]Point, 0, days)}
	for i := days - 1; i >= 0; i-- {
		date := today.AddDate(0, 0, -i).Format("2006-01-02")
		p := Point{Date: date}
		var cents int64
		seen := false
		for _, t := range s.Usage.Tools {
			if v, ok := t.ByDay[date]; ok {
				cents += v.Cents
				seen = true
			}
		}
		if seen {
			p.AgentUSD = usd(cents)
			out.CoverageDays++
		}
		if c, ok := billed[date]; ok {
			p.BilledUSD = usd(c)
		}
		out.Points = append(out.Points, p)
	}
	out.Warning = s.coverageWarning(today, days, out.CoverageDays)
	return out
}

func (s Snapshot) coverageWarning(today time.Time, days, covered int) string {
	missing := days - covered
	if missing == 0 {
		return ""
	}
	if s.Usage.Days > 0 && days > s.Usage.Days {
		cutoff := today.AddDate(0, 0, -s.Usage.Days).Format("2006-01-02")
		return fmt.Sprintf("agent usage covers %d of %d days; looseapi scans transcripts modified in the last %d days, so dates before %s only count sessions still open after it, and %d days in this window have no data",
			covered, days, s.Usage.Days, cutoff, missing)
	}
	return fmt.Sprintf("agent usage covers %d of %d days; %d have no data", covered, days, missing)
}

// chargesByDay sums charge events by the local date they landed on.
func (s Snapshot) chargesByDay(loc *time.Location) map[string]int64 {
	out := map[string]int64{}
	for _, e := range s.Events {
		if e.Kind != "charge" || e.AmountCents == nil {
			continue
		}
		out[localDate(e.Date, loc)] += *e.AmountCents
	}
	return out
}

func localDate(stamp string, loc *time.Location) string {
	if t, err := time.Parse(time.RFC3339, stamp); err == nil {
		return t.In(loc).Format("2006-01-02")
	}
	if len(stamp) >= 10 {
		return stamp[:10]
	}
	return stamp
}

func usd(cents int64) *float64 {
	v := float64(cents) / 100
	return &v
}
