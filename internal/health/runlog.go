package health

import (
	"fmt"
	"sort"
	"time"
)

// The run log is the history the status sweep throws away.
//
// Sweep answers "is this delivering right now". That is the right question to
// wake someone up with and the wrong one to understand a pipeline by: a run
// that failed and was rescued by the next one never appears in it at all.
// job-discovery crashed three times in twenty hours and the sweep reported it
// green throughout, because each crash was followed by a success before anyone
// looked.
//
// Nothing new is collected here. Every run is already recorded as one
// automation.run event, so this is a grouping and a sentence over events that
// exist. It is deliberately not a log viewer: the raw Actions transcript or
// launchd log is one click away in each run's own URL, and what is actually
// missing on a phone is the shape of the last week on one screen.

// renamed folds an automation's old event names onto its current one.
//
// hacklist was called hacklist-sf until the board stopped being SF-only. Most
// of its recorded history sits under the old name, and without this the log
// shows two automations, one of which apparently died on the day of the
// rename, and neither of which is the whole story.
var renamed = map[string]string{"hacklist-sf": "hacklist"}

// Canonical is the name an automation's runs should be filed under.
func Canonical(name string) string {
	if to, ok := renamed[name]; ok {
		return to
	}
	return name
}

// Verdict is the run log's own vocabulary, and deliberately not health.State.
//
// State says whether an automation is delivering now; these say what the
// window held. The two genuinely disagree, and that disagreement is the reason
// this screen exists: a pipeline that broke twice and is green again is ok to
// the sweep and recovered here.
type Verdict string

const (
	Clean     Verdict = "clean"     // every run in the window finished
	Recovered Verdict = "recovered" // it broke, and the newest run is green again
	Broken    Verdict = "broken"    // the newest run failed
	Quiet     Verdict = "quiet"     // nothing ran in the window
	Unwatched Verdict = "unwatched" // no per-run reporting exists for this one
)

// Tone maps a verdict onto the status colours the board already uses, so the
// two automation screens need one legend between them rather than two.
func (v Verdict) Tone() string {
	switch v {
	case Broken:
		return "failing"
	case Recovered:
		return "late"
	case Clean:
		return "ok"
	default:
		return ""
	}
}

// Rank orders worst-first, the same way the sweep does, with one departure:
// unwatched sorts below healthy rather than above it. Those rows are here for
// honesty, not urgency, and six grey "nobody is counting" lines wedged between
// the failures and the healthy ones is most of a phone screen spent saying
// nothing happened.
func (v Verdict) Rank() int {
	switch v {
	case Broken:
		return 0
	case Recovered:
		return 1
	case Quiet:
		return 2
	case Clean:
		return 3
	}
	return 4
}

// RunLog is one automation's window, plus the one-line reading of it.
type RunLog struct {
	Automation string `json:"automation"`
	Label      string `json:"label"`
	Total      int    `json:"total"`
	Worked     int    `json:"worked"` // ran and did something
	NoOp       int    `json:"noop"`   // ran and correctly decided to do nothing
	Failed     int    `json:"failed"`

	Last        time.Time `json:"last,omitempty"`
	LastFailure time.Time `json:"last_failure,omitempty"`
	// CleanRuns is how many runs have succeeded since the last failure. It is
	// the number that says whether a thing is still broken, and it is not
	// derivable from the counts beside it: "8 failed" reads the same whether
	// the last one was an hour ago or the pipeline has been fixed for two days.
	CleanRuns int `json:"clean_runs"`

	Verdict  Verdict `json:"verdict"`
	Tone     string  `json:"tone"`
	Headline string  `json:"headline"`

	// Runs is newest-first and capped. The strip on the board is drawn from
	// it, so the cap is the only thing bounding how much of a chatty
	// automation's week crosses the wire: the reaper alone runs 48 times a day.
	Runs []RunEntry `json:"runs"`
}

// RunEntry is one run, said the way the Discord digest already says it.
type RunEntry struct {
	ID       string    `json:"id"`
	Status   RunStatus `json:"status"`
	At       time.Time `json:"at"`
	Duration string    `json:"duration,omitempty"`
	Detail   string    `json:"detail"`
	URL      string    `json:"url,omitempty"`
}

// SummarizeRuns groups recorded runs by automation and reads each group.
//
// declared is the roster, and every declared automation gets a row whether or
// not it produced a run. An automation quietly missing from this screen would
// read as one with nothing to report, which is the same false all-clear the
// sweep is built to avoid.
func SummarizeRuns(runs []Run, declared []string, now time.Time, keep int) []RunLog {
	// Folded onto the canonical name and deduped by the run's own ID, which
	// runs.go already declares the stable dedupe key.
	//
	// The rename made both necessary, not just the fold: reporting is
	// suppressed by an automation/id key, so renaming hacklist made every run
	// it had already seen look new, and the overlap sits in the log twice
	// under two names. Counting those twice would inflate a week and print
	// each of them as two rows an hour apart from nothing.
	byName := map[string][]Run{}
	seen := map[string]bool{}
	for _, r := range runs {
		n := Canonical(r.Automation)
		if k := n + "/" + r.ID; r.ID != "" {
			if seen[k] {
				continue
			}
			seen[k] = true
		}
		byName[n] = append(byName[n], r)
	}

	names := make([]string, 0, len(byName)+len(declared))
	listed := map[string]bool{}
	for _, n := range declared {
		n = Canonical(n)
		if !listed[n] {
			listed[n], names = true, append(names, n)
		}
	}
	// A name with recorded runs but no declaration is still shown. It means an
	// automation was retired from the roster while its history is still inside
	// the window, and dropping it would silently shorten the week.
	for n := range byName {
		if !listed[n] {
			listed[n], names = true, append(names, n)
		}
	}

	out := make([]RunLog, 0, len(names))
	for _, n := range names {
		out = append(out, summarizeOne(n, byName[n], now, keep))
	}
	sort.SliceStable(out, func(i, j int) bool {
		if a, b := out[i].Verdict.Rank(), out[j].Verdict.Rank(); a != b {
			return a < b
		}
		return out[i].Last.After(out[j].Last)
	})
	return out
}

func summarizeOne(name string, runs []Run, now time.Time, keep int) RunLog {
	l := RunLog{Automation: name, Label: automationLabel(name)}

	sort.SliceStable(runs, func(i, j int) bool { return runs[i].Started.After(runs[j].Started) })
	for _, r := range runs {
		switch r.Status {
		case RunFailed:
			l.Failed++
			if r.Started.After(l.LastFailure) {
				l.LastFailure = r.Started
			}
		case RunSkipped:
			l.NoOp++
		default:
			l.Worked++
		}
		if r.Started.After(l.Last) {
			l.Last = r.Started
		}
	}
	l.Total = len(runs)
	for _, r := range runs {
		if r.Status == RunFailed {
			break // runs are newest-first, so this ends the clean streak
		}
		l.CleanRuns++
	}

	for i, r := range runs {
		if i == keep {
			break
		}
		e := RunEntry{ID: r.ID, Status: r.Status, At: r.Started, Detail: runDetail(r), URL: r.URL}
		if r.Duration > 0 {
			e.Duration = short(r.Duration)
		}
		l.Runs = append(l.Runs, e)
	}

	l.Verdict = verdictOf(name, runs, l.Failed)
	l.Tone = l.Verdict.Tone()
	l.Headline = headline(l, now)
	return l
}

func verdictOf(name string, runs []Run, failed int) Verdict {
	switch {
	case len(runs) == 0 && !ReportsRuns(name):
		return Unwatched
	case len(runs) == 0:
		return Quiet
	case runs[0].Status == RunFailed:
		return Broken
	case failed > 0:
		return Recovered
	}
	return Clean
}

// headline is the sentence, and it earns its line by saying something the
// counts beside it do not: whether the window is a problem, and how old the
// news is. The counts stay counts.
func headline(l RunLog, now time.Time) string {
	switch l.Verdict {
	case Unwatched:
		return "No per-run history. Only the status check watches this one."
	case Quiet:
		// Deliberately not "late". brew-autoupgrade runs weekly, so an empty
		// window is normal for it, and the lateness test is the sweep's job.
		return "Nothing recorded in this window."
	case Broken:
		return fmt.Sprintf("The most recent run failed, %s ago. Needs a look.", short(now.Sub(l.Last)))
	case Recovered:
		// Leads with the streak, not the failures. Laksh fixed three pipelines
		// and the old wording still opened with "8 runs failed", which reads as
		// news about now rather than about a week ago.
		return fmt.Sprintf("%s clean since the last failure %s ago. %s failed before that.",
			plural(l.CleanRuns, "run"), short(now.Sub(l.LastFailure)), plural(l.Failed, "run"))
	}
	if l.Worked == 0 {
		return fmt.Sprintf("%s, every one of them correctly doing nothing.", plural(l.Total, "run"))
	}
	return fmt.Sprintf("%s, none failed. Last one %s ago.", plural(l.Total, "run"), short(now.Sub(l.Last)))
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return itoa(n) + " " + noun + "s"
}
