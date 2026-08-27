package orchestrator

import (
	"strings"
	"testing"
)

// Triage is the load-bearing decision: a team of five convened to rename a
// variable is worse than one agent, being slower, five times the cost and more
// places to go wrong. Without a model reachable it falls back to these rules,
// and a fallback that cannot decide would block all work.
func TestHeuristicSizing(t *testing.T) {
	for _, tc := range []struct {
		task string
		want Size
	}{
		{"rename the variable in config.go", SizeSolo},
		{"fix a typo in the README", SizeSolo},
		{"bump the version", SizeSolo},
		{"reformat this file", SizeSolo},

		{"refactor the event log", SizeTeam},
		{"migrate to the new API", SizeTeam},
		{"there is a security hole in the auth path", SizeTeam},
		{"fix the concurrency bug in the queue", SizeTeam},
		{"improve performance of the sweep", SizeTeam},

		{"add a flag that prints the roster", SizePair},
	} {
		if got := heuristicSize(tc.task); got != tc.want {
			t.Errorf("%q sized %s, want %s", tc.task, got, tc.want)
		}
	}
}

// A word that means "hard" beats a word that means "trivial", because the cost
// of under-convening on something dangerous is higher than the cost of
// over-convening on something small.
func TestHardWordsWinOverTrivialOnes(t *testing.T) {
	got := heuristicSize("rename the field in the security token refactor")
	if got != SizeTeam {
		t.Fatalf("got %s, want team: the risky word has to win", got)
	}
}

// The verifier runs on a different agent from the executor on purpose. An agent
// checking its own work shares its blind spots, and cross-agent verification is
// the cheapest diversity available once you are already vendor-neutral.
func TestTheVerifierIsNotTheExecutor(t *testing.T) {
	roles := Org(SizeTeam)
	var exec, verify string
	for _, r := range roles {
		switch r.Name {
		case "executor":
			exec = r.Agent
		case "verifier":
			verify = r.Agent
		}
	}
	if exec == "" || verify == "" {
		t.Fatal("a team must have both an executor and a verifier")
	}
	if exec == verify {
		t.Fatalf("both run on %s; an agent checking its own work shares its blind spots", exec)
	}
}

func TestOrgSizes(t *testing.T) {
	for _, tc := range []struct {
		size  Size
		roles []string
	}{
		{SizeSolo, []string{"executor"}},
		{SizePair, []string{"executor", "reviewer"}},
		{SizeTeam, []string{"planner", "executor", "verifier", "reviewer"}},
	} {
		var got []string
		for _, r := range Org(tc.size) {
			got = append(got, r.Name)
		}
		if strings.Join(got, ",") != strings.Join(tc.roles, ",") {
			t.Errorf("%s = %v, want %v", tc.size, got, tc.roles)
		}
	}
	// An unrecognised size convenes the full team rather than nothing. Doing
	// no work because a string was misspelled is the worse failure.
	if len(Org(Size("nonsense"))) != 4 {
		t.Error("an unknown size should fall back to the full org")
	}
}

// Every role's brief has to be narrow. "You are a world-class engineer"
// produces worse output than telling it exactly what to produce and what not to.
func TestBriefsSayWhatNotToDo(t *testing.T) {
	for _, r := range Org(SizeTeam) {
		if len(r.Brief) < 60 {
			t.Errorf("%s has a brief too thin to constrain anything", r.Name)
		}
		if r.Share <= 0 || r.Share > 1 {
			t.Errorf("%s has share %v, which is not a slice of a budget", r.Name, r.Share)
		}
	}
	shares := 0.0
	for _, r := range Org(SizeTeam) {
		shares += r.Share
	}
	if shares < 0.99 || shares > 1.01 {
		t.Errorf("the team's shares total %v, so a budget is either overspent or unused", shares)
	}
}

// The budget is a real ceiling. When it is gone the remaining roles are skipped
// and the run says what it dropped, rather than quietly overspending.
func TestBudgetIsACeiling(t *testing.T) {
	b := &Budget{TotalUSD: 1.0}
	if b.Remaining() != 1.0 {
		t.Fatalf("a fresh budget has %v remaining", b.Remaining())
	}
	b.Spend(0.4)
	if b.Spent() != 0.4 || b.Remaining() != 0.6 {
		t.Fatalf("spent %v, remaining %v", b.Spent(), b.Remaining())
	}
	// Overspending clamps to zero rather than reporting a negative budget,
	// because the caller's test is whether anything is left.
	b.Spend(5)
	if b.Remaining() != 0 {
		t.Fatalf("remaining %v after overspending, want 0", b.Remaining())
	}
	if b.Spent() != 5.4 {
		t.Errorf("the real total spent must survive the clamp, got %v", b.Spent())
	}
}

// The chain is a chain: each role reads what the one before it wrote. A break
// here means a role opens and burns a context window waiting for a file that
// will never appear.
func TestAttachableChainsTheHandoffs(t *testing.T) {
	o := &Orchestrator{}
	sessions := o.Attachable("add a json flag", "/tmp/work", SizeTeam)

	if len(sessions) != 4 {
		t.Fatalf("got %d roles", len(sessions))
	}
	if sessions[0].Input != "" {
		t.Errorf("the first role reads nothing, got %q", sessions[0].Input)
	}
	for i := 1; i < len(sessions); i++ {
		if sessions[i].Input != sessions[i-1].Output {
			t.Errorf("%s reads %q but %s writes %q",
				sessions[i].Role, sessions[i].Input, sessions[i-1].Role, sessions[i-1].Output)
		}
	}
	for _, s := range sessions {
		if s.Dir != "/tmp/work" {
			t.Errorf("%s runs in %q", s.Role, s.Dir)
		}
		if !strings.HasPrefix(s.Name, "am-") {
			t.Errorf("%s is named %q, breaking the machine's convention", s.Role, s.Name)
		}
		if !strings.HasSuffix(s.Output, s.Role+".md") {
			t.Errorf("%s writes to %q", s.Role, s.Output)
		}
	}
}

// Two different tasks must not share a run directory, or the second overwrites
// the first's handoffs mid-run.
func TestDifferentTasksGetDifferentRuns(t *testing.T) {
	o := &Orchestrator{}
	a := o.Attachable("add a json flag", "/tmp", SizeSolo)[0]
	b := o.Attachable("delete the json flag", "/tmp", SizeSolo)[0]
	if a.Output == b.Output {
		t.Fatalf("both tasks write to %s", a.Output)
	}
	if a.Name == b.Name {
		t.Fatalf("both tasks open the session %s", a.Name)
	}
}

// A role's brief has to state the file contract, because a role that finishes
// without writing its output has silently broken the chain.
func TestBriefForNamesTheHandoff(t *testing.T) {
	o := &Orchestrator{}
	s := o.Attachable("do the thing", "/tmp", SizeTeam)[1] // executor
	brief := BriefFor(s, "do the thing")

	for _, want := range []string{s.Input, s.Output, "do the thing"} {
		if !strings.Contains(brief, want) {
			t.Errorf("the brief never mentions %q:\n%s", want, brief)
		}
	}
}

// Carrying a whole previous step into the next prompt is how a chain runs out
// of context on its third role.
func TestTruncateBounds(t *testing.T) {
	if got := truncate("short", 100); got != "short" {
		t.Errorf("something under the limit must pass through, got %q", got)
	}
	long := strings.Repeat("x", 500)
	got := truncate(long, 100)
	if len(got) > 140 {
		t.Errorf("truncate returned %d chars from a 100 limit", len(got))
	}
	if !strings.Contains(got, "truncated") {
		t.Error("a truncated string must say so, or the next role reads a sentence that stops mid-word and treats it as the whole answer")
	}
}
