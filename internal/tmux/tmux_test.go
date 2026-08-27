package tmux

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestAgent(t *testing.T) {
	for _, tc := range []struct{ command, want string }{
		{"claude", "claude"},
		{"codex", "codex"},
		{"gemini", "gemini"},
		// Both CLIs are Node programs, so a wrapper or a resumed session shows
		// the interpreter. Labelling that "node" is honest; guessing which
		// agent it is would put sessions under the wrong heading.
		{"node", "node"},
		{"bun", "node"},
		// A plain shell is not an agent session and must not be labelled one.
		{"zsh", ""},
		{"bash", ""},
		{"vim", ""},
		{"", ""},
	} {
		if got := (Session{Command: tc.command}).Agent(); got != tc.want {
			t.Errorf("Agent(%q) = %q, want %q", tc.command, got, tc.want)
		}
	}
}

func TestUnix(t *testing.T) {
	if got := unix("1787452453"); !got.Equal(time.Unix(1787452453, 0)) {
		t.Errorf("got %s", got)
	}
	// tmux has been seen to emit an empty field for a session created before
	// the server restarted. A zero time is right; a parse panic is not.
	for _, bad := range []string{"", "not-a-number", "-"} {
		if got := unix(bad); !got.IsZero() {
			t.Errorf("unix(%q) = %s, want zero", bad, got)
		}
	}
}

// The bug this guards cost an hour and looked like an empty machine.
//
// tmux sanitises control characters out of its own -F output when the locale is
// not UTF-8, so the tab separator comes back as an underscore:
//
//	am-amac_1787595768_1787596187_1
//
// Every line then has one field instead of four, every session is dropped, and
// List hands back an empty slice with no error. An interactive shell sets LANG
// so it never happens there; launchd gives an agent none, which is exactly
// where the dashboard runs.
func TestListSurvivesAnEmptyLocale(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	name := "amac-locale-test"
	if err := exec.Command("tmux", "new-session", "-d", "-s", name).Run(); err != nil {
		t.Skip("no tmux server available:", err)
	}
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", "="+name).Run() })

	// The environment a launchd agent actually gets: a PATH and nothing else
	// that matters.
	for _, k := range []string{"LC_ALL", "LANG", "LC_CTYPE"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}

	list, err := List()
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	var found bool
	for _, s := range list {
		if s.Name == name {
			found = true
		}
	}
	if !found {
		t.Fatalf("List() returned %d sessions and none was %q; the separator was eaten again", len(list), name)
	}
}
