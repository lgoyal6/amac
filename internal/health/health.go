// Package health answers one question per automation: is it still doing the
// thing it exists to do?
//
// The distinction that shapes this package is between a run and a delivery.
// Every automation here is scheduled several times over for redundancy, so
// most runs deliberately do nothing: morning-brief fires four crons a day and
// the first success claims the slot, hacklist fires four and the gate lets one
// through. "The last run was green" is therefore almost always true and almost
// never informative. Each probe reports the last real DELIVERY instead, read
// from the artifact the automation commits only once the work landed.
//
// The second half is silence. A push-based log records the runs that happened
// and can never tell you about the run that didn't. So every automation
// declares the cadence it is expected to deliver at, and Run applies the
// lateness test centrally, against that declared cadence. No probe has to
// remember to do it, and an automation that dies quietly still produces a
// finding.
package health

import (
	"context"
	"sort"
	"time"
)

type State string

const (
	OK      State = "ok"      // delivered recently, last run green
	Failing State = "failing" // it ran and it broke
	Late    State = "late"    // no delivery within cadence + grace; the silent failure
	Down    State = "down"    // the thing hosting it is unreachable
	Unknown State = "unknown" // probe could not establish the truth; never assume OK
)

// Rank orders states worst-first so a digest leads with what needs attention.
func (s State) Rank() int {
	switch s {
	case Failing:
		return 0
	case Down:
		return 1
	case Late:
		return 2
	case Unknown:
		return 3
	default:
		return 4
	}
}

func (s State) Icon() string {
	switch s {
	case Failing:
		return "🔴"
	case Down:
		return "🟠"
	case Late:
		return "🟡"
	case Unknown:
		return "⚪"
	default:
		return "🟢"
	}
}

// Report is one automation's verdict. Last is the last real delivery, not the
// last run, and stays zero when the probe genuinely could not determine it,
// which suppresses the lateness test rather than inventing a failure.
type Report struct {
	Name   string    `json:"name"`
	State  State     `json:"state"`
	Last   time.Time `json:"last,omitempty"`
	Detail string    `json:"detail"`
	Notes  []string  `json:"notes,omitempty"`
	Err    string    `json:"err,omitempty"`
}

// Automation is a declared expectation. Every and Grace are what make silence
// detectable, so they are required rather than optional: an automation with no
// declared cadence can go dark forever without anyone noticing.
type Automation struct {
	Name  string
	What  string
	Every time.Duration
	Grace time.Duration
	Check func(context.Context) (Report, error)
}

// Run probes every automation and applies the lateness test.
//
// Probes run sequentially. There are four of them and each is a couple of HTTP
// calls; concurrency here would buy a second and cost the ability to read the
// output in order.
func Run(ctx context.Context, list []Automation) []Report {
	out := make([]Report, 0, len(list))
	for _, a := range list {
		r, err := a.Check(ctx)
		r.Name = a.Name
		if err != nil {
			// A probe that failed tells us nothing about the automation, only
			// about the probe. Reporting OK here would be a lie, and reporting
			// Failing would page him for our own bug.
			r.State = Unknown
			r.Err = err.Error()
			if r.Detail == "" {
				r.Detail = "probe failed"
			}
			out = append(out, r)
			continue
		}
		if r.State == OK && !r.Last.IsZero() {
			if age := time.Since(r.Last); age > a.Every+a.Grace {
				r.State = Late
				r.Detail = "no delivery in " + short(age) + " (expected every " + short(a.Every) + ")"
			}
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].State.Rank() < out[j].State.Rank() })
	return out
}

// short renders a duration the way you would say it out loud. time.Duration's
// own String gives "27h14m3.002s", which is noise in a phone notification.
func short(d time.Duration) string {
	switch {
	case d < time.Minute:
		return itoa(int(d.Seconds())) + "s"
	case d < time.Hour:
		return itoa(int(d.Minutes())) + "m"
	case d < 48*time.Hour:
		return itoa(int(d.Hours())) + "h"
	default:
		return itoa(int(d.Hours()/24)) + "d"
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}
