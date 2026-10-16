// Package core contains the pure health evaluator shared by AMAC and its
// browser lab. It has no host probes, database access, or external services.
package core

import (
	"context"
	"sort"
	"time"
)

type State string

const (
	OK      State = "ok"
	Failing State = "failing"
	Late    State = "late"
	Down    State = "down"
	Unknown State = "unknown"
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
// last run, and stays zero when the probe cannot determine it.
type Report struct {
	Name     string    `json:"name"`
	Category string    `json:"category,omitempty"`
	State    State     `json:"state"`
	Last     time.Time `json:"last,omitempty"`
	Detail   string    `json:"detail"`
	Notes    []string  `json:"notes,omitempty"`
	Err      string    `json:"err,omitempty"`
}

// Automation declares the cadence used to detect a silent delivery failure.
type Automation struct {
	Name     string
	What     string
	Every    time.Duration
	Grace    time.Duration
	Check    func(context.Context) (Report, error)
	Home     string
	Category string
}

// Sweep samples the current clock after each probe, as native AMAC does.
func Sweep(ctx context.Context, list []Automation) []Report {
	return sweep(ctx, list, time.Now)
}

// SweepAt probes each automation and applies the lateness test at now.
func SweepAt(ctx context.Context, list []Automation, now time.Time) []Report {
	return sweep(ctx, list, func() time.Time { return now })
}

func sweep(ctx context.Context, list []Automation, clock func() time.Time) []Report {
	out := make([]Report, 0, len(list))
	for _, a := range list {
		r, err := a.Check(ctx)
		r.Name = a.Name
		r.Category = a.Category
		if err != nil {
			r.State = Unknown
			r.Err = err.Error()
			if r.Detail == "" {
				r.Detail = "probe failed"
			}
			out = append(out, r)
			continue
		}
		if r.State == OK && !r.Last.IsZero() {
			if age := clock().Sub(r.Last); age > a.Every+a.Grace {
				r.State = Late
				r.Detail = "no delivery in " + Short(age) + " (expected every " + Short(a.Every) + ")"
			}
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].State.Rank() < out[j].State.Rank() })
	return out
}

// Short keeps durations compact in user-facing health details.
func Short(d time.Duration) string {
	switch {
	case d < time.Minute:
		return Itoa(int(d.Seconds())) + "s"
	case d < time.Hour:
		return Itoa(int(d.Minutes())) + "m"
	case d < 48*time.Hour:
		return Itoa(int(d.Hours())) + "h"
	default:
		return Itoa(int(d.Hours()/24)) + "d"
	}
}

// Itoa converts an integer to decimal text for native health messages.
func Itoa(n int) string {
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
