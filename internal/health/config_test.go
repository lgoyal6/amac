package health

import (
	"context"
	"errors"
	"github.com/lgoyal6/amac/internal/event"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeRoster(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "health.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadParsesADeclaration(t *testing.T) {
	p := writeRoster(t, `{"automations":[
	  {"name":"backup","what":"nightly","every":"24h","grace":"4h","home":"~/scripts",
	   "probe":"launchd_marker","with":{"label":"com.example.backup","log":"~/Library/Logs/b.log"}}
	]}`)
	list, err := Load(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("got %d automations", len(list))
	}
	a := list[0]
	if a.Every != 24*time.Hour || a.Grace != 4*time.Hour {
		t.Errorf("cadence = %v/%v", a.Every, a.Grace)
	}
	// ~ has to resolve, or a portable roster names a directory that does not
	// exist and every probe under it reports unknown.
	if strings.HasPrefix(a.Home, "~") {
		t.Errorf("home not expanded: %q", a.Home)
	}
	if a.Check == nil {
		t.Error("no check built")
	}
	if a.Category != "automation" {
		t.Errorf("default category = %q, want automation", a.Category)
	}
}

func TestMachineCategoryIsPreserved(t *testing.T) {
	p := writeRoster(t, `{"automations":[
	  {"name":"pressure","category":"machine","probe":"marker_fields","with":{"log":"x","fields":[]}}
	]}`)
	list, err := Load(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if list[0].Category != "machine" {
		t.Fatalf("category = %q, want machine", list[0].Category)
	}
}

// A cadence is optional and its absence means the lateness test does not
// apply. A service is either up or it is not, and declaring a fake cadence to
// satisfy a schema would be declaring a fake delivery.
func TestServiceNeedsNoCadence(t *testing.T) {
	p := writeRoster(t, `{"automations":[
	  {"name":"daemon","probe":"service","with":{"label":"com.amac.daemon","port":7788}}
	]}`)
	list, err := Load(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if list[0].Every != 0 {
		t.Errorf("every = %v, want zero", list[0].Every)
	}
}

// Every problem at once. Someone editing this by hand should not have to run
// the command five times to learn about five typos.
func TestLoadReportsEveryProblem(t *testing.T) {
	p := writeRoster(t, `{"automations":[
	  {"name":"a","every":"24 hours","probe":"launchd_marker","with":{"label":"x","log":"y"}},
	  {"name":"b","probe":"telepathy"},
	  {"name":"a","probe":"service","with":{"label":"z","port":1}},
	  {"name":"d","probe":"launchd_marker","with":{"label":"only-a-label"}}
	]}`)
	_, err := Load(p, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"24 hours", "telepathy", "declared twice", "missing log"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%s", want, err)
		}
	}
	// An unknown probe has to name the ones that exist, or the fix is a guess.
	if !strings.Contains(err.Error(), "launchd_marker") {
		t.Errorf("unknown probe must list the valid kinds:\n%s", err)
	}
}

// A silently dropped automation is an automation nobody is watching, which is
// the one outcome this package exists to prevent. Nothing partial loads.
func TestABadEntryFailsTheWholeRoster(t *testing.T) {
	p := writeRoster(t, `{"automations":[
	  {"name":"good","every":"1h","probe":"service","with":{"label":"a","port":1}},
	  {"name":"bad","probe":"nonsense"}
	]}`)
	if list, err := Load(p, nil); err == nil {
		t.Fatalf("loaded %d automations from a roster with a bad entry", len(list))
	}
}

// A missing roster is its own error so the CLI can point at `amac init` rather
// than printing a bare file-not-found at someone who has just cloned this.
func TestMissingRosterIsNamed(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.json"), nil)
	var missing ErrNoConfig
	if !asErr(err, &missing) {
		t.Fatalf("got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "amac init") {
		t.Errorf("error should say what to run: %v", err)
	}
}

// An unknown field is a typo, and a typo silently ignored is a setting someone
// believes is in force.
func TestUnknownFieldIsRejected(t *testing.T) {
	p := writeRoster(t, `{"automations":[
	  {"name":"a","probe":"service","cadence":"24h","with":{"label":"x","port":1}}
	]}`)
	if _, err := Load(p, nil); err == nil {
		t.Fatal("expected an error for the misspelled field")
	}
}

func TestEmptyRosterIsRefused(t *testing.T) {
	if _, err := Load(writeRoster(t, `{"automations":[]}`), nil); err == nil {
		t.Fatal("an empty roster must be an error, not a clean bill of health")
	}
}

func asErr(err error, target any) bool { return errors.As(err, target) }

// A heartbeat is a different way of learning the same fact, so it gets the same
// cadence, grace and lateness test as everything else. What it does not get is
// a weaker rule for never having been heard from.
func TestHeartbeatProbe(t *testing.T) {
	log, err := event.Open(filepath.Join(t.TempDir(), "beats.db"), event.Relaxed)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()

	p := writeRoster(t, `{"automations":[
	  {"name":"vps-backup","every":"24h","grace":"4h","probe":"heartbeat"}
	]}`)
	list, err := Load(p, log)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Declared but never heard from is unknown, not late. A job nobody has
	// wired up yet and a job that has stopped are different problems, and there
	// is nothing to measure lateness from anyway.
	r, err := list[0].Check(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r.State != Unknown {
		t.Fatalf("never posted = %s, want unknown", r.State)
	}
	if !r.Last.IsZero() {
		t.Error("nothing has been heard from, so there is no last delivery")
	}

	// A bare beat, which is what `curl -X POST` sends.
	if err := Record(ctx, log, Beat{Name: "vps-backup"}); err != nil {
		t.Fatal(err)
	}
	if r, _ = list[0].Check(ctx); r.State != OK {
		t.Fatalf("after a beat = %s, want ok", r.State)
	}
	if r.Last.IsZero() {
		t.Error("a beat is a delivery and has to set Last, or lateness cannot work")
	}

	// A job may report its own failure.
	n := 3
	if err := Record(ctx, log, Beat{Name: "vps-backup", State: "failing", Detail: "disk full", Count: &n}); err != nil {
		t.Fatal(err)
	}
	r, _ = list[0].Check(ctx)
	if r.State != Failing || r.Detail != "disk full" {
		t.Fatalf("got %s %q", r.State, r.Detail)
	}
	// Last still moves on a failure report. A job that fails and keeps saying
	// so is in a different situation from one that failed and went quiet, and
	// collapsing them would hide the second.
	if r.Last.IsZero() {
		t.Error("a failure report is still contact")
	}
	if len(r.Notes) == 0 {
		t.Error("a reported count should survive to the report")
	}

	// A state amac does not understand is refused rather than stored, because
	// storing it means the probe has to guess later.
	if err := Record(ctx, log, Beat{Name: "vps-backup", State: "probably-fine"}); err == nil {
		t.Error("an unknown state must be refused")
	}
	if err := Record(ctx, log, Beat{}); err == nil {
		t.Error("a nameless beat must be refused")
	}
}

// systemd's timestamps are the reason its probe needs no log where launchd's
// does: launchd reports an exit status and not when it happened.
func TestSystemdTime(t *testing.T) {
	for _, s := range []string{
		"Tue 2026-08-27 09:30:01 UTC",
		"2026-08-27 09:30:01 UTC",
	} {
		got, ok := systemdTime(s)
		if !ok {
			t.Errorf("failed to parse %q", s)
			continue
		}
		if got.Year() != 2026 || got.Hour() != 9 {
			t.Errorf("%q parsed as %s", s, got)
		}
	}
	// A unit that has never run reports an empty timestamp. That is a fact
	// about the unit, not a parse failure, and reporting it as one would turn
	// "new" into "broken".
	for _, s := range []string{"", "n/a", "   "} {
		if _, ok := systemdTime(s); ok {
			t.Errorf("%q should not parse", s)
		}
	}
}

// A roster written on a Mac and run on Linux should say which probe to use
// instead, rather than surfacing a bare exec error.
func TestPlatformProbesExplainThemselves(t *testing.T) {
	p := writeRoster(t, `{"automations":[
	  {"name":"a","every":"24h","probe":"launchd_marker","with":{"label":"x","log":"/tmp/x.log"}},
	  {"name":"b","every":"24h","probe":"systemd_unit","with":{"unit":"x.service"}}
	]}`)
	list, err := Load(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Both kinds load anywhere. Whichever one is not native to this machine
	// reports why when it runs, which is the difference between a roster that
	// will not load and one that tells you what to change.
	if len(list) != 2 {
		t.Fatalf("both platform kinds must load, got %d", len(list))
	}
	for _, a := range list {
		if a.Check == nil {
			t.Fatalf("%s built no check", a.Name)
		}
	}
}
