package crew

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSlug(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"add a --json flag", "add-a-json-flag"},
		{"Fix THE Typo", "fix-the-typo"},
		// tmux reads "." and ":" as target separators, so a name containing
		// either cannot be addressed reliably afterwards.
		{"bump v1.2.3: urgent", "bump-v1-2-3-urgent"},
		{"...", "task"},
		{"", "task"},
		{strings.Repeat("long ", 20), "long-long-long-long-long-long-lo"},
	} {
		got := Slug(tc.in)
		if got != tc.want {
			t.Errorf("Slug(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if strings.ContainsAny(got, ".:") || strings.HasSuffix(got, "-") {
			t.Errorf("Slug(%q) = %q is not a safe tmux target", tc.in, got)
		}
	}
}

// The brief has to state the file contract, because a role that finishes
// without writing its output silently breaks the chain.
func TestBriefStatesTheContract(t *testing.T) {
	s := Session{Role: "executor", Input: "/runs/x/planner.md", Output: "/runs/x/executor.md"}
	b := Brief(s, "Implement the plan below.", "add a flag")
	for _, want := range []string{"Implement the plan below.", "add a flag", s.Input, s.Output} {
		if !strings.Contains(b, want) {
			t.Errorf("brief is missing %q:\n%s", want, b)
		}
	}
	// The first role has no input and must not be told to read a file that
	// will never exist.
	first := Brief(Session{Role: "planner", Output: "/runs/x/planner.md"}, "Plan it.", "add a flag")
	if strings.Contains(first, "previous step") {
		t.Errorf("the first role has no previous step:\n%s", first)
	}
}

// Exercises the real tmux path with a harmless command standing in for an
// agent, which is the part that quoting bugs actually break.
func TestOpenSeedsTheSession(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("no tmux")
	}
	dir := t.TempDir()
	name := "amac-crewtest"
	_ = exec.Command("tmux", "kill-session", "-t", "="+name).Run()
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", "="+name).Run() })

	s := Session{
		Name: name, Role: "planner", Agent: "cat", Dir: dir,
		Output: filepath.Join(dir, "planner.md"),
	}
	// Quotes, newlines and an apostrophe: everything that breaks a naive
	// send-keys, and all of it normal in a role brief.
	brief := "Read it first.\nDon't \"guess\" at the $PATH or `backticks`.\nTask: x"
	if err := Open(s, brief); err != nil {
		t.Fatal(err)
	}
	if !Exists(name) {
		t.Fatal("session was not created")
	}
	if err := Open(s, brief); err == nil {
		t.Fatal("opening an existing session must fail rather than clobber it")
	}

	// `cat <brief>` echoes the brief into the pane, so the pane proves the
	// brief arrived intact rather than mangled by quoting.
	var pane string
	for i := 0; i < 40; i++ {
		out, err := exec.Command("tmux", "capture-pane", "-p", "-t", "="+name+":").Output()
		if err == nil {
			pane = string(out)
			if strings.Contains(pane, "backticks") {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, want := range []string{"Read it first.", `Don't "guess"`, "$PATH", "backticks"} {
		if !strings.Contains(pane, want) {
			t.Errorf("brief did not survive the shell: missing %q\npane:\n%s", want, pane)
		}
	}

	// The brief is left on disk next to the handoff artifacts, so a run can be
	// read back afterwards without the session still being alive.
	if _, err := os.Stat(filepath.Join(dir, "planner.brief.md")); err != nil {
		t.Errorf("brief file not kept: %v", err)
	}
}
