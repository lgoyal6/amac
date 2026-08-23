// Package attention decides whether an agent session that wants something
// should interrupt the user, and delivers the interruption if so.
//
// The predecessor got this wrong in a way worth recording. agentmon suppressed
// a push whenever a tmux client was attached to the session or the keyboard
// had been touched in the last two minutes. Both are true almost always: there
// are a dozen clients attached right now and he is at the machine all day. So
// every alert was correct, and silent. No agent notification went out between
// Aug 13 and Aug 22.
//
// Claude Code's Remote Control gets it right by suppressing on *focus*: it
// pushes unless you are actually looking at that terminal. This package
// computes the same signal for Codex, which Remote Control does not cover.
package attention

import (
	"os/exec"
	"strings"
)

// terminalApps are the macOS apps that can be hosting a tmux client. If the
// frontmost app is not one of these, he is reading email or a browser and the
// session is unattended no matter how many clients are attached to it.
var terminalApps = map[string]bool{
	"Terminal": true, "iTerm2": true, "Ghostty": true,
	"Alacritty": true, "kitty": true, "WezTerm": true,
}

// Watching reports which tmux session the user is looking at right now.
//
// The second return is false when nothing is being watched, which includes the
// screen being locked, the frontmost app not being a terminal, and any case
// where we could not tell. Failing to "nothing is watched" means an uncertain
// answer produces a notification rather than silence: a spurious ping costs a
// glance, a missed one costs a blocked agent nobody notices.
func Watching() (string, bool) {
	app, err := frontmostApp()
	if err != nil || !terminalApps[app] {
		return "", false
	}
	// Terminal.app can name the exact tab in front. Nothing else here can, so
	// for other terminals we fall back to the most recently active client,
	// which is right whenever only one terminal window is in play.
	if app == "Terminal" {
		if tty, err := frontmostTerminalTTY(); err == nil && tty != "" {
			if s, ok := sessionForTTY(tty); ok {
				return s, true
			}
			// A frontmost tab that is not running tmux at all: he is at a
			// plain shell, watching no session.
			return "", false
		}
	}
	return mostRecentClientSession()
}

func frontmostApp() (string, error) {
	out, err := exec.Command("osascript", "-e",
		`tell application "System Events" to get name of first application process whose frontmost is true`).Output()
	return strings.TrimSpace(string(out)), err
}

func frontmostTerminalTTY() (string, error) {
	out, err := exec.Command("osascript", "-e",
		`tell application "Terminal" to get tty of selected tab of front window`).Output()
	return strings.TrimSpace(string(out)), err
}

// sessionForTTY maps a terminal tty to the tmux session its client is viewing.
func sessionForTTY(tty string) (string, bool) {
	out, err := exec.Command("tmux", "list-clients", "-F", "#{client_tty}\t#{client_session}").Output()
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		got, sess, ok := strings.Cut(line, "\t")
		if ok && got == tty {
			return sess, true
		}
	}
	return "", false
}

// mostRecentClientSession is the fallback for terminals that cannot report
// their front tab. client_activity is a unix timestamp of last input.
func mostRecentClientSession() (string, bool) {
	out, err := exec.Command("tmux", "list-clients", "-F", "#{client_activity}\t#{client_session}").Output()
	if err != nil {
		return "", false
	}
	var best, bestSess = "", ""
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		at, sess, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		// Timestamps are same-width unix seconds, so string compare is fine
		// and avoids parsing a field tmux could pad differently.
		if len(at) > len(best) || (len(at) == len(best) && at > best) {
			best, bestSess = at, sess
		}
	}
	return bestSess, bestSess != ""
}
