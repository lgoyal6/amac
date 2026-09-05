package health

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The roster is the file that decides what amac watches, and there is
// deliberately no built-in fallback: a default list of somebody else's
// automations would probe paths that do not exist and report a fine machine as
// broken, while an empty fallback would sweep nothing and report success. Both
// are worse than refusing to start, so the failure modes here are the design.

func TestConfigPathFollowsHomeAndTheOverride(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AMAC_HEALTH_CONFIG", "")
	if got := ConfigPath(); got != filepath.Join(home, ".amac", "health.json") {
		t.Errorf("ConfigPath() = %q, not under this HOME", got)
	}
	// The override exists so a test, a container or a second roster does not
	// have to move the home directory to be read.
	t.Setenv("AMAC_HEALTH_CONFIG", "/tmp/other.json")
	if got := ConfigPath(); got != "/tmp/other.json" {
		t.Errorf("the override was ignored: %q", got)
	}
}

// A missing roster must name the file and say what writes one. "no automations
// configured" sends somebody to read the source.
func TestAMissingRosterNamesTheFileAndTheFix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "health.json")
	_, err := Declarations(path)
	if err == nil {
		t.Fatal("a missing roster must be an error, not an empty sweep")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not name the file", err)
	}
	if !strings.Contains(err.Error(), "init") {
		t.Errorf("error %q does not say what writes one", err)
	}
}

// A roster that will not parse is an error rather than a partial sweep. Half a
// roster reports the automations it managed to read as the whole picture, which
// is the false all-clear this package exists to avoid.
func TestACorruptRosterIsRefusedWholesale(t *testing.T) {
	path := filepath.Join(t.TempDir(), "health.json")
	if err := os.WriteFile(path, []byte(`{"automations":[{"name":"a"`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Declarations(path); err == nil {
		t.Error("a truncated roster was accepted")
	}
}

// Every automation has to declare a cadence, because that is what makes silence
// detectable. One without it can go dark forever with nothing noticing, which
// is the failure the whole design is built around.
func TestAnAutomationWithoutACadenceIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "health.json")
	if err := os.WriteFile(path, []byte(`{"automations":[
		{"name":"silent","what":"does a thing","probe":"launchd_marker",
		 "with":{"label":"com.x","log":"/tmp/x.log"}}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Declarations is the raw parse and stays permissive; Load is where the
	// roster becomes something that sweeps, and that is where it must be
	// refused. The error has to say why, because "every is required" alone
	// invites somebody to put any number in it.
	_, err := Load(path, nil)
	if err == nil {
		t.Fatal("an automation with no cadence was accepted; its silence would be undetectable")
	}
	if !strings.Contains(err.Error(), "silence") {
		t.Errorf("error %q does not say why a cadence is required", err)
	}
}

// A continuous service is the one thing that may omit a cadence, because it is
// either up or it is not and a fake cadence would be a fake delivery.
func TestAServiceMayOmitItsCadence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "health.json")
	if err := os.WriteFile(path, []byte(`{"automations":[
		{"name":"amac-daemon","what":"serves the board","probe":"service",
		 "with":{"label":"com.amac.daemon","port":7788}}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path, nil); err != nil {
		t.Errorf("a service without a cadence was refused: %v", err)
	}
}

// A roster that loads has to survive the round trip with the fields the sweep
// depends on, or a probe reads a blank where a repo or a log path should be.
func TestADeclarationKeepsWhatTheProbeNeeds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "health.json")
	if err := os.WriteFile(path, []byte(`{"automations":[{
		"name":"nightly","what":"copies the database offsite","every":"24h","grace":"6h",
		"schedule":"daily 02:00","host":"This Mac","home":"~/backup","probe":"launchd_marker",
		"with":{"label":"com.demo.backup","log":"/tmp/backup.log"}}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	decls, err := Declarations(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(decls) != 1 {
		t.Fatalf("got %d declarations", len(decls))
	}
	d := decls[0]
	if d.Name != "nightly" || d.Every != "24h" || d.Grace != "6h" {
		t.Errorf("cadence lost in the round trip: %+v", d)
	}
	if d.Probe != "launchd_marker" || d.With["label"] != "com.demo.backup" {
		t.Errorf("probe configuration lost: %+v", d)
	}
	// Home is where somebody goes to fix it, and it is declared rather than
	// inferred because guessing which repo a failing pipeline lives in is the
	// kind of confident wrong answer that sends an agent to edit the wrong tree.
	if d.Home != "~/backup" {
		t.Errorf("home = %q, want the declared one", d.Home)
	}
}
