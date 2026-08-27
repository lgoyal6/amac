package health

import (
	"errors"
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
	list, err := Load(p)
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
}

// A cadence is optional and its absence means the lateness test does not
// apply. A service is either up or it is not, and declaring a fake cadence to
// satisfy a schema would be declaring a fake delivery.
func TestServiceNeedsNoCadence(t *testing.T) {
	p := writeRoster(t, `{"automations":[
	  {"name":"daemon","probe":"service","with":{"label":"com.amac.daemon","port":7788}}
	]}`)
	list, err := Load(p)
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
	_, err := Load(p)
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
	if list, err := Load(p); err == nil {
		t.Fatalf("loaded %d automations from a roster with a bad entry", len(list))
	}
}

// A missing roster is its own error so the CLI can point at `amac init` rather
// than printing a bare file-not-found at someone who has just cloned this.
func TestMissingRosterIsNamed(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.json"))
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
	if _, err := Load(p); err == nil {
		t.Fatal("expected an error for the misspelled field")
	}
}

func TestEmptyRosterIsRefused(t *testing.T) {
	if _, err := Load(writeRoster(t, `{"automations":[]}`)); err == nil {
		t.Fatal("an empty roster must be an error, not a clean bill of health")
	}
}

func asErr(err error, target any) bool { return errors.As(err, target) }
