package tmux

import (
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
