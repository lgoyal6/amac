// Package spend reads what looseapi already knows.
//
// amac has its own cost query, and it is the narrower of the two by
// construction: it sees only sessions amac started, priced from whatever the
// ACP adapter chose to report, which for Codex is nothing at all. looseapi
// reads the session logs both CLIs write whoever started them, and the billing
// mail that is the only source in existence for credit burndown and consumer
// plans. A card statement cannot see a credit balance falling to zero, because
// no transaction ever happens.
//
// So this reads rather than recomputes. Two implementations of "what am I
// spending" that can disagree is strictly worse than one that lives in another
// repo, and the one that lives elsewhere is the one with more inputs.
//
// The snapshot is a legitimate artifact to read, not a cache to distrust:
// looseapi writes it after the mail scan, the provider poll and the usage read
// have all completed, so a run that died halfway leaves the previous one in
// place rather than a partial file.
package spend

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Path is where looseapi's CLI leaves its snapshot.
func Path() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".devspend", "snapshot.json")
}

type Snapshot struct {
	GeneratedAt  time.Time `json:"generatedAt"`
	MonthlyCents int       `json:"monthlyCents"`
	Alerts       []Alert   `json:"alerts"`
	Usage        Usage     `json:"usage"`
}

// Alert is something looseapi thinks is worth acting on. Severity is its own
// scale, higher being worse; amac sorts by it and otherwise passes it through
// rather than re-deriving a judgement it has no extra information to make.
type Alert struct {
	Kind     string `json:"kind"`
	Severity int    `json:"severity"`
	Service  string `json:"service"`
	Message  string `json:"message"`
}

type Usage struct {
	Days  int    `json:"days"`
	Tools []Tool `json:"tools"`
}

type Tool struct {
	Tool      string            `json:"tool"`
	Total     Totals            `json:"total"`
	ByDay     map[string]Totals `json:"byDay"`
	ByProject map[string]Totals `json:"byProject"`
	ByModel   map[string]Totals `json:"byModel"`
}

// Slice is one line of a breakdown: a project, or a model, and what it came to.
type Slice struct {
	Name  string `json:"name"`
	Cents int64  `json:"cents"`
	Share int    `json:"share"` // percent of the total, for reading at a glance
}

// Totals carries token counts and a cents figure that is deliberately not
// called spend anywhere. On a flat subscription these tokens cost nothing
// marginal; the number is what the same work would have cost through the API.
// looseapi is careful about that distinction and dropping it here would
// reintroduce exactly the overstatement it avoided.
type Totals struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cacheRead"`
	CacheWrite int64 `json:"cacheWrite"`
	Cents      int64 `json:"cents"`
	Messages   int64 `json:"messages"`
}

// Read loads the newest snapshot.
//
// A missing file is an ordinary answer rather than an error condition: it means
// looseapi has not run here, which the caller should say plainly instead of
// showing a zero that reads as "you spent nothing".
func Read() (Snapshot, error) {
	b, err := os.ReadFile(Path())
	if err != nil {
		return Snapshot{}, err
	}
	var s Snapshot
	if err := json.Unmarshal(b, &s); err != nil {
		return Snapshot{}, fmt.Errorf("%s: %w", Path(), err)
	}
	return s, nil
}

// Age is how stale the reading is. The scan runs daily, so anything much older
// than that is a spend figure that has stopped tracking reality, and a stale
// number presented without its age is the failure mode this whole system
// exists to avoid.
func (s Snapshot) Age() time.Duration { return time.Since(s.GeneratedAt) }

// Worst returns the alerts that matter most, highest severity first.
func (s Snapshot) Worst(n int) []Alert {
	out := append([]Alert(nil), s.Alerts...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Severity > out[j].Severity })
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// AgentCents totals the equivalent API cost across every coding agent, over
// looseapi's whole window.
func (s Snapshot) AgentCents() int64 {
	var n int64
	for _, t := range s.Usage.Tools {
		n += t.Total.Cents
	}
	return n
}

// TodayCents is the same figure for one local day.
//
// The key is the agent's own local date, which is the right one: this exists to
// answer "what have I run up today", and a UTC boundary would zero the number
// in the late afternoon.
func (s Snapshot) TodayCents(now time.Time) int64 {
	key := now.Format("2006-01-02")
	var n int64
	for _, t := range s.Usage.Tools {
		n += t.ByDay[key].Cents
	}
	return n
}

// USD renders cents the way a report should: no false precision, and never
// rounded to zero when there is real money in it.
func USD(cents int64) string {
	if cents == 0 {
		return "$0"
	}
	if cents < 100 {
		return fmt.Sprintf("$%.2f", float64(cents)/100)
	}
	return fmt.Sprintf("$%.0f", float64(cents)/100)
}

// Breakdown merges one of looseapi's per-tool groupings into a ranked list.
//
// Merged rather than reported per tool, because the question is what a project
// cost and not what a project cost in Claude Code specifically. Two agents
// working in the same repo are one line of spend.
func (s Snapshot) Breakdown(key string, top int) []Slice {
	merged := map[string]int64{}
	for _, t := range s.Usage.Tools {
		group := t.ByProject
		if key == "model" {
			group = t.ByModel
		}
		for name, v := range group {
			merged[name] += v.Cents
		}
	}

	total := s.AgentCents()
	out := make([]Slice, 0, len(merged))
	for name, cents := range merged {
		share := 0
		if total > 0 {
			share = int(float64(cents) / float64(total) * 100)
		}
		out = append(out, Slice{Name: name, Cents: cents, Share: share})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Cents > out[j].Cents })
	if len(out) > top {
		out = out[:top]
	}
	return out
}
