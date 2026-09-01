package spend

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/lgoyal6/amac/internal/account"
)

const sample = `{
  "generatedAt": "2026-08-26T17:47:33.449Z",
  "monthlyCents": 2500,
  "alerts": [
    {"kind":"unread_money","severity":1,"service":"X","message":"low"},
    {"kind":"credit_burndown","severity":3,"service":"AWS","message":"$10 left, ~1.3 days"}
  ],
  "usage": {"days":30,"tools":[
    {"tool":"Claude Code","total":{"cents":1525091},"byDay":{"2026-08-26":{"cents":102963}}},
    {"tool":"Codex","total":{"cents":390000},"byDay":{"2026-08-26":{"cents":34500}}}
  ]}
}`

func load(t *testing.T) Snapshot {
	t.Helper()
	var s Snapshot
	if err := json.Unmarshal([]byte(sample), &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// Worst is what reaches a phone banner, so the ordering is the feature. A
// credit balance with a day left must not sit under an unread receipt.
func TestWorstPutsTheUrgentThingFirst(t *testing.T) {
	got := load(t).Worst(1)
	if len(got) != 1 || got[0].Service != "AWS" {
		t.Fatalf("got %+v, want the AWS burndown first", got)
	}
}

func TestTotalsAcrossTools(t *testing.T) {
	s := load(t)
	if got := s.AgentCents(); got != 1915091 {
		t.Errorf("AgentCents = %d, want 1915091", got)
	}
	// Local date, not UTC: this answers "what have I run up today", and a UTC
	// boundary would zero it in the late afternoon here.
	day := time.Date(2026, 8, 26, 15, 4, 0, 0, time.Local)
	if got := s.TodayCents(day); got != 137463 {
		t.Errorf("TodayCents = %d, want 137463", got)
	}
	if got := s.TodayCents(day.AddDate(0, 0, 1)); got != 0 {
		t.Errorf("a day with no usage must be 0, got %d", got)
	}
}

func TestServicesExposeTrialsCreditsAndAttention(t *testing.T) {
	const richer = `{
  "generatedAt":"2026-08-28T12:00:00Z",
  "monthlyCents":4500,
  "events":[
    {"id":"gmail-secret-id","subject":"private inbox subject","date":"2026-08-28T09:00:00Z","serviceId":"cursor","service":"Cursor","scope":"dev","kind":"trial_ending","severity":2,"amountCents":null,"creditsRemainingCents":null,"unread":true,"trashed":false},
    {"date":"2026-08-27T09:00:00Z","serviceId":"aws","service":"AWS","scope":"dev","kind":"credits_low","severity":2,"creditsRemainingCents":1000,"unread":false},
    {"date":"2026-08-20T09:00:00Z","serviceId":"aws","service":"AWS","scope":"dev","kind":"credits_low","severity":2,"creditsRemainingCents":3000,"unread":false},
    {"date":"2026-08-26T09:00:00Z","serviceId":"railway","service":"Railway","scope":"dev","via":"stripe.com","kind":"charge","severity":1,"amountCents":2500,"unread":false},
    {"date":"2026-08-25T09:00:00Z","serviceId":"railway","service":"Railway","scope":"dev","via":"stripe.com","kind":"charge","severity":1,"amountCents":2000,"unread":false}
  ],
  "alerts":[
    {"kind":"trial_ending","severity":2,"service":"Cursor","message":"Cursor trial ends soon"},
    {"kind":"credit_burndown","severity":3,"service":"AWS","message":"AWS credits are low"}
  ]
}`
	var snap Snapshot
	if err := json.Unmarshal([]byte(richer), &snap); err != nil {
		t.Fatal(err)
	}

	services := snap.Services()
	if len(services) != 3 {
		t.Fatalf("got %d services, want 3: %+v", len(services), services)
	}
	// Attention first; ties are stable and deterministic by spend then name.
	if services[0].Name != "AWS" || !services[0].NeedsAttention || services[0].CreditsRemainingCents == nil || *services[0].CreditsRemainingCents != 1000 {
		t.Fatalf("AWS credit state not preserved: %+v", services[0])
	}
	if services[1].Name != "Cursor" || services[1].TrialStatus != "ending" || !services[1].NeedsAttention {
		t.Fatalf("Cursor trial state not preserved: %+v", services[1])
	}
	if services[2].Name != "Railway" || services[2].ChargeCount != 2 || services[2].TotalCents != 4500 || services[2].BilledThrough != "stripe.com" {
		t.Fatalf("Railway charge rollup is wrong: %+v", services[2])
	}

	counts := snap.Counts()
	if counts.ServicesSeen != 3 || counts.ActiveAlerts != 2 || counts.UnreadMoney != 1 || counts.Trials != 1 || counts.CreditAccounts != 1 || counts.AttentionServices != 2 || counts.Charges != 2 {
		t.Fatalf("unexpected counts: %+v", counts)
	}

	// Raw inbox identifiers and subjects are intentionally not part of AMAC's
	// typed snapshot and therefore cannot accidentally cross its API boundary.
	b, err := json.Marshal(snap.Events)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "gmail-secret-id") || strings.Contains(string(b), "private inbox subject") {
		t.Fatalf("private mail metadata survived safe decoding: %s", b)
	}
}

// A cost report that rounds real money to zero is the one thing it must never
// do, and one that prints eight decimals is unreadable on a phone.
func TestUSD(t *testing.T) {
	for _, tc := range []struct {
		cents int64
		want  string
	}{
		{0, "$0"}, {7, "$0.07"}, {99, "$0.99"}, {100, "$1"}, {2500, "$25"}, {1915091, "$19151"},
	} {
		if got := USD(tc.cents); got != tc.want {
			t.Errorf("USD(%d) = %s, want %s", tc.cents, got, tc.want)
		}
	}
}

// A missing snapshot means looseapi has not run, which the caller must say
// plainly. Returning a zeroed struct with no error would render as "you spent
// nothing", which is the worst available answer.
func TestMissingSnapshotIsAnError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if _, err := Read(); err == nil {
		t.Fatal("a missing snapshot must be an error, not an empty report")
	}
}

// The account split is the answer to "which login is this", and the reason it
// exists is that a whole Codex account was invisible for as long as it had
// been in use. So the roster leads: an account with no usage, and one that is
// not installed here, both keep their row.
func TestAccountsKeepEveryKnownLogin(t *testing.T) {
	const withAccounts = `{
	  "usage": {"days":30,"tools":[
	    {"tool":"Claude Code","total":{"cents":1000},"byAccount":{"claude":{"cents":1000}}},
	    {"tool":"Codex","total":{"cents":1000},"byAccount":{"codex":{"cents":900},"codex-ish":{"cents":100}}}
	  ]}
	}`
	var s Snapshot
	if err := json.Unmarshal([]byte(withAccounts), &s); err != nil {
		t.Fatal(err)
	}
	roster := []account.Account{
		{ID: "codex", Agent: "codex", Label: "codex", Home: "/h/.codex", Local: true},
		{ID: "codex-ish", Agent: "codex", Label: "codex-ish", Home: "/h/.codex-ish", Local: true},
		{ID: "claude-gmi", Agent: "claude", Label: "gmi", Home: "/h/.claude", Local: true},
		{ID: "claude-lgoyal", Agent: "claude", Label: "lgoyal", Home: "/h/.claude-lgoyal"},
	}

	got := s.Accounts(roster)
	if len(got) != 4 {
		t.Fatalf("got %d rows, want one per known login", len(got))
	}
	// Ranked by cost, so the row that matters is at the top.
	if got[0].ID != "claude-gmi" || got[0].Cents != 1000 {
		t.Errorf("first row is %s at %d, want claude-gmi at 1000", got[0].ID, got[0].Cents)
	}
	// The second Codex login is the whole point: real money, small share.
	var ish AccountSlice
	for _, a := range got {
		if a.ID == "codex-ish" {
			ish = a
		}
	}
	if ish.Cents != 100 || ish.Share != 5 {
		t.Errorf("codex-ish = %d cents / %d%%, want 100 / 5", ish.Cents, ish.Share)
	}
	// An absent account reports zero AND says it is absent, so the zero can be
	// read as "not here" rather than "spent nothing".
	last := got[len(got)-1]
	if last.ID != "claude-lgoyal" || last.Cents != 0 || last.Present {
		t.Errorf("absent account = %+v, want claude-lgoyal at 0, present=false", last)
	}
}

// A home nobody has registered still has real tokens in it. Dropping it would
// leave the rows adding up to less than the total with nothing saying why.
func TestUnknownHomeStillGetsARow(t *testing.T) {
	const stray = `{"usage":{"days":30,"tools":[
	  {"tool":"Codex","total":{"cents":500},"byAccount":{"codex-someone":{"cents":500}}}]}}`
	var s Snapshot
	if err := json.Unmarshal([]byte(stray), &s); err != nil {
		t.Fatal(err)
	}
	got := s.Accounts(nil)
	if len(got) != 1 || got[0].ID != "codex-someone" || got[0].Cents != 500 {
		t.Fatalf("got %+v, want the unrecognised home carried through", got)
	}
}
