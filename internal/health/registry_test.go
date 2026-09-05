package health

import (
	"path/filepath"
	"testing"

	"github.com/lgoyal6/amac/internal/event"
)

// The bug this cache shape exists to prevent.
//
// Behind a plain sync.Once the first caller fixed the roster for the whole
// process, so anything that set AMAC_HEALTH_CONFIG afterwards was silently
// answered with somebody else's automations. Nothing failed at the point of
// the mistake, which is why it surfaced somewhere else entirely: a new test in
// the daemon package that merely issued a GET to /api/health made an unrelated
// test there fail, by pinning this machine's thirteen real automations onto a
// test that declares two.
func TestASecondPathLoadsItsOwnRoster(t *testing.T) {
	log, err := event.Open(filepath.Join(t.TempDir(), "roster.db"), event.Relaxed)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()

	decl := func(name string) string {
		return `{"automations":[{"name":"` + name + `","what":"w","every":"1h","grace":"1h",
			"probe":"launchd_marker","with":{"label":"com.example.` + name + `","log":"/dev/null"}}]}`
	}
	first, second := writeRoster(t, decl("alpha")), writeRoster(t, decl("beta"))

	only := func(t *testing.T, want string) {
		t.Helper()
		got, err := Roster(log)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Name != want {
			t.Fatalf("roster = %v, want just %s", got, want)
		}
	}

	t.Setenv("AMAC_HEALTH_CONFIG", first)
	only(t, "alpha")

	t.Setenv("AMAC_HEALTH_CONFIG", second)
	only(t, "beta") // the first load used to fix this for the whole process

	// Going back is still served from the cache rather than re-read, which is
	// the property the original sync.Once was there for and which this keeps.
	t.Setenv("AMAC_HEALTH_CONFIG", first)
	only(t, "alpha")
}
