// Package crew opens agent sessions a human can take over.
//
// The orchestrator's existing path runs each role as an ACP subprocess: fully
// driven, fully headless, and impossible to step into. That is right for work
// nobody needs to watch and wrong for everything else, because the moment a
// run goes sideways the only options are to let it finish or kill it.
//
// A crew session is a real tmux session running the real CLI. It can be
// attached, argued with, corrected and resumed. amac seeds it and then gets
// out of the way; what it learns afterwards comes from the attention hooks,
// the same ones Codex and the bell already feed.
//
// The cost of handing over the keyboard is that amac can no longer read a
// role's output off the wire to hand to the next role. Scraping the pane would
// get it back and is exactly what ACP was adopted to stop doing. So each role
// writes to a file instead, and the next role is told to read it. The handoff
// becomes an artifact on disk: inspectable, editable before the next role
// starts, and identical whether a human or an agent produced it.
package crew

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// Session is one attachable role.
type Session struct {
	Name   string // tmux session name
	Role   string
	Agent  string
	Dir    string // where the agent runs
	Input  string // file this role reads, "" for the first
	Output string // file this role must write
}

// Attach is the command that puts a human in front of it.
func (s Session) Attach() string { return "tmux attach -t " + s.Name }

// RunDir is where a task's handoff artifacts live. Keeping them outside the
// working tree means a run never shows up as uncommitted changes in the repo
// being worked on, which would be indistinguishable from the executor's own
// edits.
func RunDir(slug string) string {
	return filepath.Join(os.Getenv("HOME"), ".amac", "runs", slug)
}

var unsafe = regexp.MustCompile(`[^a-z0-9]+`)

// Slug turns a task into something usable as a tmux session name and a
// directory. tmux treats "." and ":" as target separators, so a name
// containing either cannot be addressed reliably.
func Slug(task string) string {
	s := unsafe.ReplaceAllString(strings.ToLower(task), "-")
	s = strings.Trim(s, "-")
	if len(s) > 32 {
		s = strings.Trim(s[:32], "-")
	}
	if s == "" {
		s = "task"
	}
	return s
}

// Name follows the am- convention the rest of the machine already uses, so
// these sessions show up in the existing tooling rather than only in amac.
func Name(slug, role string) string { return "am-" + slug + "-" + role }

// Exists reports whether a tmux session is already there. Session commands
// accept the "=name" exact form; pane commands and set-option do not, which is
// a distinction that has cost real debugging time here before.
func Exists(name string) bool {
	return exec.Command("tmux", "has-session", "-t", "="+name).Run() == nil
}

// Status is where a role is in the chain.
//
// Derived entirely from two facts on disk: whether the tmux session exists and
// whether the artifact does. Nothing here asks an agent how it is doing, which
// is what makes the same answer available to the CLI, the dashboard, and a run
// nobody has looked at since yesterday.
func Status(s Session) string {
	switch {
	case Exists(s.Name):
		return "running"
	case HasArtifact(s.Output):
		return "done"
	case s.Input != "" && !HasArtifact(s.Input):
		return "waiting"
	default:
		return "ready"
	}
}

// HasArtifact reports whether a handoff file exists with something in it. An
// empty file is a role that opened the file and then died, which for the next
// role in the chain is the same as no file at all.
func HasArtifact(path string) bool {
	if path == "" {
		return false
	}
	st, err := os.Stat(path)
	return err == nil && st.Size() > 0
}

// Open creates the tmux session and starts the agent with its brief.
//
// The brief goes in through a file rather than the command line. A role brief
// is several hundred characters of prose with quotes and newlines in it, and
// embedding that in a shell command sent through tmux send-keys is a quoting
// bug waiting to happen; the failure mode is a session that starts with a
// truncated or mangled instruction, which looks like the model misbehaving.
func Open(s Session, brief string) error {
	if Exists(s.Name) {
		return fmt.Errorf("session %s already exists", s.Name)
	}
	if err := os.MkdirAll(filepath.Dir(s.Output), 0o755); err != nil {
		return err
	}
	briefPath := filepath.Join(filepath.Dir(s.Output), s.Role+".brief.md")
	if err := os.WriteFile(briefPath, []byte(brief), 0o644); err != nil {
		return err
	}

	// Start a shell first, then type the command. Passing the agent directly
	// to new-session would end the session the moment the agent exits, taking
	// its scrollback with it; landing back at a prompt keeps the transcript
	// readable and lets the agent be restarted in place.
	if err := exec.Command("tmux", "new-session", "-d", "-s", s.Name, "-c", s.Dir).Run(); err != nil {
		return fmt.Errorf("create %s: %w", s.Name, err)
	}
	cmd := fmt.Sprintf("%s \"$(cat %s)\"", s.Agent, shellQuote(briefPath))
	if err := exec.Command("tmux", "send-keys", "-t", "="+s.Name+":", cmd, "Enter").Run(); err != nil {
		return fmt.Errorf("seed %s: %w", s.Name, err)
	}
	return nil
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// Brief assembles what the role is told: its own instruction, the task, where
// to read the previous step from, and where to leave its own result.
//
// The file contract is stated to the agent explicitly rather than implied. An
// agent that finishes without writing its output file has broken the chain,
// and saying so plainly is cheaper than detecting it afterwards.
func Brief(s Session, roleBrief, task string) string {
	var b strings.Builder
	b.WriteString(roleBrief)
	b.WriteString("\n\nTask: ")
	b.WriteString(task)
	if s.Input != "" {
		fmt.Fprintf(&b, "\n\nThe previous step's result is at %s. Read it first.", s.Input)
	}
	fmt.Fprintf(&b, "\n\nWhen you are done, write your result to %s. "+
		"The next role reads that file and nothing else, so anything you leave only "+
		"in this terminal is lost.", s.Output)
	return b.String()
}
