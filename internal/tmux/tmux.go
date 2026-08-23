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

// List returns every tmux session, newest activity first. A missing tmux
// server is not an error: it means there are no sessions.
func List() ([]Session, error) {
	out, err := exec.Command("tmux", "list-sessions", "-F",
		"#{session_name}\t#{session_created}\t#{session_activity}\t#{?session_attached,1,0}").Output()
	if err != nil {
		return nil, nil
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
	out, err := exec.Command("tmux", "list-panes", "-a", "-F",
		"#{session_name}\t#{pane_current_path}\t#{pane_current_command}\t#{?pane_active,1,0}").Output()
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
