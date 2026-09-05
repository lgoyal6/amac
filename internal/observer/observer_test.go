package observer

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lgoyal6/amac/internal/event"
)

// The observer is the only part of amac that watches the person rather than the
// machine, and its whole design is default deny plus a kill switch. Neither had
// a test, which is the wrong thing to leave unverified: a privacy control that
// has never been executed is a privacy claim, not a privacy control.

// A missing policy file must deny everything. Getting this backwards would mean
// a fresh install observes every app until somebody notices, and "I forgot to
// configure it" is the most likely state any config file is ever in.
func TestNoPolicyFileObservesNothing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p, err := LoadPolicy()
	if err != nil {
		t.Fatalf("a missing policy is not an error: %v", err)
	}
	for _, app := range []string{"Terminal", "Safari", "Messages", ""} {
		if p.allowed(app) {
			t.Errorf("%q was allowed by an empty policy", app)
		}
	}
	if p.Titles == nil {
		t.Error("Titles must be usable rather than nil, or every lookup panics")
	}
}

// An app absent from the allowlist is denied. There is no wildcard and no
// implicit allow, which is what makes the list readable as the whole truth.
func TestOnlyListedAppsAreObserved(t *testing.T) {
	p := Policy{Allow: []string{"Terminal", "Ghostty"}, Titles: map[string]bool{}}
	for app, want := range map[string]bool{
		"Terminal": true, "Ghostty": true,
		"terminal": true, // case-insensitive: the OS is not consistent about it
		"Safari":   false, "Messages": false, "": false,
		"Terminal.app": false, // no prefix matching; a near miss is a miss
	} {
		if got := p.allowed(app); got != want {
			t.Errorf("allowed(%q) = %v, want %v", app, got, want)
		}
	}
}

// Titles are a separate decision from the app, because a title leaks far more
// than a name: document names, URLs, subject lines. Allowing an app must not
// silently start recording what is inside it.
func TestAllowingAnAppDoesNotAllowItsTitles(t *testing.T) {
	p := Policy{Allow: []string{"Terminal", "Safari"}, Titles: map[string]bool{"Terminal": true}}
	if !p.titlesFor("Terminal") {
		t.Error("Terminal titles were explicitly allowed")
	}
	if p.titlesFor("Safari") {
		t.Error("Safari is observed but its titles were never allowed")
	}
	if p.titlesFor("Messages") {
		t.Error("an app that is not observed at all must not have titles allowed")
	}
}

func TestPolicyRoundTripsThroughItsFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".amac"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(PolicyPath(),
		[]byte(`{"allow":["Ghostty"],"titles":{"Ghostty":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := LoadPolicy()
	if err != nil {
		t.Fatal(err)
	}
	if !p.allowed("Ghostty") || !p.titlesFor("Ghostty") {
		t.Errorf("policy did not load: %+v", p)
	}
	if p.allowed("Safari") {
		t.Error("loading a policy must not widen it")
	}
}

// A corrupt policy is an error, not an empty allowlist.
//
// Both refuse to observe, so it is tempting to treat them the same. They are
// not: an empty policy is a decision and a broken one is a bug, and silently
// turning the second into the first means a typo disables observation forever
// with nothing said about it.
func TestACorruptPolicyIsAnErrorNotSilentDenial(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".amac"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(PolicyPath(), []byte(`{"allow": [`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPolicy(); err == nil {
		t.Error("a policy that will not parse must be reported, not treated as deny-all")
	}
}

// The kill switch is a file whose existence stops observation, so that turning
// it off does not depend on the daemon choosing to cooperate. Both paths live
// under the home directory and must move with it.
func TestControlPathsFollowTheHomeDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for name, got := range map[string]string{"policy": PolicyPath(), "kill switch": KillSwitchPath()} {
		if filepath.Dir(got) != filepath.Join(home, ".amac") {
			t.Errorf("%s at %q is not under this HOME", name, got)
		}
	}
	if PolicyPath() == KillSwitchPath() {
		t.Error("the kill switch must not be the policy file; creating one would rewrite the other")
	}
}

// ------------------------------------------------------------ the tick ----

func observing(t *testing.T, p Policy) (*Observer, *event.Log) {
	t.Helper()
	log, err := event.Open(filepath.Join(t.TempDir(), "events.db"), event.Relaxed)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	return New(log, p), log
}

func observations(t *testing.T, log *event.Log) []map[string]any {
	t.Helper()
	rows, err := log.DB().Query(`SELECT payload FROM events WHERE kind = ? ORDER BY seq`,
		string(event.KindObservation))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var b []byte
		if rows.Scan(&b) != nil {
			continue
		}
		var m map[string]any
		if json.Unmarshal(b, &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

// The kill switch exists so that stopping observation does not depend on the
// daemon choosing to cooperate, and so it can be used while the thing you do
// not want observed is on screen right now. Nothing may be recorded once the
// file is there.
func TestTheKillSwitchStopsObservationImmediately(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".amac"), 0o755); err != nil {
		t.Fatal(err)
	}
	o, log := observing(t, Policy{Allow: []string{"Terminal"}, Titles: map[string]bool{}})

	old := frontmost
	t.Cleanup(func() { frontmost = old })
	frontmost = func() (string, string, error) { return "Terminal", "", nil }

	if err := os.WriteFile(KillSwitchPath(), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	_ = o.Run(ctx, 10*time.Millisecond)

	if got := observations(t, log); len(got) != 0 {
		t.Errorf("recorded %d observations with the kill switch on: %v", len(got), got)
	}
}

// A denied app must close the open span rather than be skipped over. Skipping
// would let the previous allowed app absorb the time spent in the denied one,
// so a private app would show up as extra minutes in the terminal.
func TestADeniedAppEndsTheSpanRatherThanExtendingIt(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	o, log := observing(t, Policy{Allow: []string{"Terminal"}, Titles: map[string]bool{}})

	o.mark(context.Background(), "Terminal")
	o.since = time.Now().Add(-3 * time.Second) // pretend three seconds passed
	o.flush(context.Background())              // what the deny path does

	got := observations(t, log)
	if len(got) != 1 {
		t.Fatalf("expected the Terminal span to be closed, got %v", got)
	}
	if got[0]["app"] != "Terminal" {
		t.Errorf("wrong app recorded: %v", got[0])
	}
	if secs, _ := got[0]["seconds"].(float64); secs > 4 {
		t.Errorf("span of %vs absorbed time after the switch away", secs)
	}
}

// Allowing an app must not start recording what is inside it. Titles carry
// document names, URLs and subject lines, which is a different disclosure from
// "he was in a browser".
func TestTitlesAreWithheldUnlessSeparatelyAllowed(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	oldT := windowTitle
	t.Cleanup(func() { windowTitle = oldT })
	windowTitle = func(string) string { return "PRIVATE - salary negotiation.pdf" }

	o, log := observing(t, Policy{Allow: []string{"Preview"}, Titles: map[string]bool{}})
	o.emit(context.Background(), "Preview", 5*time.Second)

	got := observations(t, log)
	if len(got) != 1 {
		t.Fatalf("expected one observation, got %v", got)
	}
	if _, present := got[0]["title"]; present {
		t.Errorf("a title was recorded for an app whose titles were never allowed: %v", got[0])
	}

	o2, log2 := observing(t, Policy{Allow: []string{"Preview"}, Titles: map[string]bool{"Preview": true}})
	o2.emit(context.Background(), "Preview", 5*time.Second)
	if got := observations(t, log2); len(got) != 1 || got[0]["title"] == nil {
		t.Errorf("an explicitly allowed title was not recorded: %v", got)
	}
}

// Sub-second spans are switching noise rather than work, and recording them
// would turn a flick through three windows into three observations.
func TestSwitchingNoiseIsNotRecorded(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	o, log := observing(t, Policy{Allow: []string{"Terminal"}, Titles: map[string]bool{}})
	o.emit(context.Background(), "Terminal", 200*time.Millisecond)
	if got := observations(t, log); len(got) != 0 {
		t.Errorf("recorded a sub-second span: %v", got)
	}
}
