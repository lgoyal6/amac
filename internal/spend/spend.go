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
	GeneratedAt         time.Time  `json:"generatedAt"`
	Source              string     `json:"source"`
	MessageCount        int        `json:"messageCount"`
	LedgerSize          int        `json:"ledgerSize"`
	RecoveredFromLedger int        `json:"recoveredFromLedger"`
	HiddenOutOfScope    int        `json:"hiddenOutOfScope"`
	MonthlyCents        int64      `json:"monthlyCents"`
	Events              []Event    `json:"events"`
	Alerts              []Alert    `json:"alerts"`
	Providers           []Provider `json:"providers"`
	Usage               Usage      `json:"usage"`
	NoAPI               []NoAPI    `json:"noApi"`
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

// Event is the safe, dashboard-relevant part of a billing-mail event. Message
// ids, subjects and alert evidence deliberately do not enter this type: AMAC
// needs to show what happened, not turn its general-purpose dashboard endpoint
// into an inbox export.
type Event struct {
	Date                  string `json:"date"`
	ServiceID             string `json:"serviceId"`
	Service               string `json:"service"`
	Scope                 string `json:"scope"`
	Via                   string `json:"via"`
	Kind                  string `json:"kind"`
	Severity              int    `json:"severity"`
	AmountCents           *int64 `json:"amountCents"`
	CreditsRemainingCents *int64 `json:"creditsRemainingCents"`
	Unread                *bool  `json:"unread"`
	Trashed               *bool  `json:"trashed"`
	MissingFromMailbox    bool   `json:"missingFromMailbox"`
}

type Provider struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Cents      *int64 `json:"cents"`
	PeriodDays int    `json:"periodDays"`
	Note       string `json:"note"`
	Error      string `json:"error"`
}

type NoAPI struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// Service rolls the event ledger up into one row per subscription or service.
// Trial and credit state remain explicit so callers do not have to reverse
// engineer account state from English alert messages.
type Service struct {
	ID                    string  `json:"id"`
	Name                  string  `json:"name"`
	Scope                 string  `json:"scope"`
	BilledThrough         string  `json:"billedThrough,omitempty"`
	ChargeCount           int     `json:"chargeCount"`
	TotalCents            int64   `json:"totalCents"`
	LatestAt              string  `json:"latestAt"`
	LatestKind            string  `json:"latestKind"`
	LastAmountCents       *int64  `json:"lastAmountCents"`
	CreditsRemainingCents *int64  `json:"creditsRemainingCents"`
	CreditsAsOf           string  `json:"creditsAsOf,omitempty"`
	TrialStatus           string  `json:"trialStatus,omitempty"`
	NeedsAttention        bool    `json:"needsAttention"`
	Alerts                []Alert `json:"alerts"`
}

// Counts are the dashboard's at-a-glance answers. They count services rather
// than messages where the user is deciding what needs attention.
type Counts struct {
	ServicesSeen      int `json:"servicesSeen"`
	ActiveAlerts      int `json:"activeAlerts"`
	UnreadMoney       int `json:"unreadMoney"`
	Trials            int `json:"trials"`
	CreditAccounts    int `json:"creditAccounts"`
	AttentionServices int `json:"attentionServices"`
	Charges           int `json:"charges"`
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

// Services turns looseapi's durable event ledger into the rows a control
// surface needs. The snapshot remains the source of truth; this is only a
// presentation projection and does not attempt to classify billing mail again.
func (s Snapshot) Services() []Service {
	type state struct {
		Service
		trialAt  string
		creditAt string
		amountAt string
	}

	byID := make(map[string]*state)
	for _, e := range s.Events {
		id := e.ServiceID
		if id == "" {
			id = e.Service
		}
		row := byID[id]
		if row == nil {
			row = &state{Service: Service{ID: id, Name: e.Service, Scope: e.Scope, Alerts: []Alert{}}}
			byID[id] = row
		}
		if row.Name == "" {
			row.Name = e.Service
		}
		if row.Scope == "" {
			row.Scope = e.Scope
		}
		if row.BilledThrough == "" && e.Via != "" {
			row.BilledThrough = e.Via
		}
		if row.LatestAt == "" || e.Date > row.LatestAt {
			row.LatestAt = e.Date
			row.LatestKind = e.Kind
		}
		if e.AmountCents != nil && (row.amountAt == "" || e.Date > row.amountAt) {
			amount := *e.AmountCents
			row.LastAmountCents = &amount
			row.amountAt = e.Date
		}
		if e.Kind == "charge" {
			row.ChargeCount++
			if e.AmountCents != nil {
				row.TotalCents += *e.AmountCents
			}
		}
		if e.CreditsRemainingCents != nil && (row.creditAt == "" || e.Date > row.creditAt) {
			balance := *e.CreditsRemainingCents
			row.CreditsRemainingCents = &balance
			row.CreditsAsOf = e.Date
			row.creditAt = e.Date
		}
		if (e.Kind == "trial_converting" || e.Kind == "trial_ending" || e.Kind == "subscription_cancelled") &&
			(row.trialAt == "" || e.Date > row.trialAt) {
			switch e.Kind {
			case "trial_converting":
				row.TrialStatus = "converting"
			case "trial_ending":
				row.TrialStatus = "ending"
			case "subscription_cancelled":
				row.TrialStatus = "cancelled"
			}
			row.trialAt = e.Date
		}
	}

	// Alerts already encode looseapi's suppression rules (for example, a trial
	// alert disappears after a later cancellation), so use those judgements
	// instead of duplicating them here.
	for _, a := range s.Alerts {
		for _, row := range byID {
			if row.Name != a.Service {
				continue
			}
			row.Alerts = append(row.Alerts, a)
			if a.Severity >= 2 {
				row.NeedsAttention = true
			}
		}
	}

	out := make([]Service, 0, len(byID))
	for _, row := range byID {
		if row.TrialStatus == "converting" || row.TrialStatus == "ending" {
			row.NeedsAttention = true
		}
		out = append(out, row.Service)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].NeedsAttention != out[j].NeedsAttention {
			return out[i].NeedsAttention
		}
		if out[i].TotalCents != out[j].TotalCents {
			return out[i].TotalCents > out[j].TotalCents
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func (s Snapshot) Counts() Counts {
	c := Counts{ActiveAlerts: len(s.Alerts)}
	for _, e := range s.Events {
		if e.Kind == "charge" {
			c.Charges++
		}
		if e.Severity >= 2 && e.Unread != nil && *e.Unread {
			c.UnreadMoney++
		}
	}
	services := s.Services()
	c.ServicesSeen = len(services)
	for _, service := range services {
		if service.TrialStatus == "converting" || service.TrialStatus == "ending" {
			c.Trials++
		}
		if service.CreditsRemainingCents != nil {
			c.CreditAccounts++
		}
		if service.NeedsAttention {
			c.AttentionServices++
		}
	}
	return c
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
