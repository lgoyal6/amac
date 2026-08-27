// Package tmux enumerates the agent sessions amac did not start.
//
// Most of the work on this machine runs in tmux sessions Laksh started
// himself, not as ACP subprocesses of the daemon. A board that showed only
// amac's own sessions would be empty most of the time and would miss every
// session that actually matters, so the daemon reads both and presents one
// list.
//
// This is enumeration, not state detection. It reports what tmux knows: the
// session exists, a client is or is not attached, this is the command running
// in the active pane. Whether an agent is blocked comes from the attention
// events its hooks recorded, never from reading the rendered pane, which is
// the practice ACP was adopted to end.
package tmux

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type Session struct {
	Name     string    `json:"name"`
	Dir      string    `json:"dir"`
	Command  string    `json:"command"` // what the active pane is running
	Created  time.Time `json:"created"`
	Activity time.Time `json:"activity"`
	Attached bool      `json:"attached"`
}

// Agent guesses which agent CLI a session is hosting, or "" for a plain shell.
//
// A guess is honest here in a way it would not be for session state: the
// command name is a fact, and getting it wrong shows a session under the wrong
// label rather than sending or withholding a notification.
func (s Session) Agent() string {
	switch s.Command {
	case "claude", "codex", "gemini":
		return s.Command
	}
	// Both CLIs are Node programs, so a wrapper or a resumed session often
	// shows up as the interpreter rather than its own name.
	if s.Command == "node" || s.Command == "bun" {
		return "node"
	}
	return ""
}

// run invokes tmux with a UTF-8 locale forced on.
//
// Without one, tmux sanitises control characters out of its own -F output and
// the tab separator comes back as an underscore:
//
//	am-amac_1787595768_1787596187_1
//
// Every line then has one field instead of four, every session is dropped, and
// the caller is handed an empty list with no error. An interactive shell has
// LANG set so this is invisible there; launchd gives an agent none, so the
// daemon showed a board with nothing on it while seventeen sessions were
// running. Forced here rather than in the plist because it is a property of
// reading tmux, not of how amac happens to be started.
func run(args ...string) ([]byte, error) {
	cmd := exec.Command("tmux", args...)
	cmd.Env = append(os.Environ(), "LC_ALL=en_US.UTF-8", "LANG=en_US.UTF-8")
	return cmd.Output()
}

// noServer reports the one failure that is not a failure: tmux exits non-zero
// when no server is running, and "there are no sessions" is the right answer
// rather than an error.
func noServer(err error) bool {
	var ee *exec.ExitError
	return errors.As(err, &ee) && bytes.Contains(ee.Stderr, []byte("no server running"))
}

// List returns every tmux session, newest activity first.
//
// A missing tmux server gives an empty list and no error. Anything else is
// returned: an empty board and an unreadable tmux look identical from outside,
// and telling them apart is the difference between "nothing is running" and
// "this screen has stopped working".
func List() ([]Session, error) {
	out, err := run("list-sessions", "-F",
		"#{session_name}\t#{session_created}\t#{session_activity}\t#{?session_attached,1,0}")
	if err != nil {
		if noServer(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("tmux list-sessions: %w", err)
	}

	panes := activePanes()
	var sessions []Session
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 4 {
			continue
		}
		s := Session{
			Name:     f[0],
			Created:  unix(f[1]),
			Activity: unix(f[2]),
			Attached: f[3] == "1",
		}
		if p, ok := panes[s.Name]; ok {
			s.Dir, s.Command = p.dir, p.command
		}
		sessions = append(sessions, s)
	}
	return sessions, nil
}

type pane struct{ dir, command string }

// activePanes maps session name to its active pane. `list-panes -a` covers
// every session in one call; asking per session would be one fork each and
// there are routinely twenty of them.
func activePanes() map[string]pane {
	out, err := run("list-panes", "-a", "-F",
		"#{session_name}\t#{pane_current_path}\t#{pane_current_command}\t#{?pane_active,1,0}")
	if err != nil {
		return nil
	}
	m := map[string]pane{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 4 || f[3] != "1" {
			continue
		}
		m[f[0]] = pane{dir: f[1], command: f[2]}
	}
	return m
}

func unix(s string) time.Time {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(n, 0)
}
