package procs

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The two command shapes seen on the machine this was written for, verbatim
// apart from the allowedTools list, plus the neighbours that must be ignored:
// the disclaimer wrapper the desktop app runs the agent through, an exiting
// process ps shows in parentheses, a Codex helper whose name merely starts
// with codex, and an Electron helper from the same app bundle.
const psFixture = `  PID  PPID  ELAPSED COMMAND
    1     0 19:18:37 /sbin/launchd
26180  1289 15:31:22 /Applications/Claude.app/Contents/Helpers/disclaimer -- /Users/lakshgoyal/Library/Application Support/Claude/claude-code/2.1.266/claude.app/Contents/MacOS/claude --output-format stream-json --verbose --input-format stream-json --thinking adaptive --effort xhigh --model claude-opus-5 --permission-prompt-tool stdio --resume=ae1af7de-c887-4d7f-b8ab-f8b225b891c5 --allowedTools mcp__computer-use --add-dir /Users/lakshgoyal/hackqueue
26181 26180 15:31:22 /Users/lakshgoyal/Library/Application Support/Claude/claude-code/2.1.266/claude.app/Contents/MacOS/claude --output-format stream-json --verbose --input-format stream-json --thinking adaptive --effort xhigh --model claude-opus-5 --permission-prompt-tool stdio --resume=ae1af7de-c887-4d7f-b8ab-f8b225b891c5 --allowedTools mcp__computer-use --add-dir /Users/lakshgoyal/hackqueue
55267 78075 14:00:19 claude --continue
15017 14637    05:16 claude
30012 30010    00:01 (claude)
34451 34051 1-02:03:04 /Applications/ChatGPT.app/Contents/Resources/codex -c features.code_mode_host=true app-server
40810 34451 14:11:03 /Applications/ChatGPT.app/Contents/Resources/codex-code-mode-host
 4037  1289 19:11:44 /Applications/Claude.app/Contents/Frameworks/Claude Helper.app/Contents/MacOS/Claude Helper --type=utility --lang=en-US
  900     1 20:00:00 /opt/homebrew/bin/tmux
  901   900 19:00:00 -zsh
  902   901 18:00:00 claude --resume 1234abcd
`

func TestParseKeepsAgentsAndIgnoresTheirNeighbours(t *testing.T) {
	got := Parse(psFixture)
	want := []Session{
		{PID: 902, Agent: "claude", Kind: "terminal", ResumeID: "1234abcd",
			Command: "claude --resume 1234abcd", Elapsed: 18 * time.Hour, InTmux: true},
		{PID: 15017, Agent: "claude", Kind: "terminal", Command: "claude",
			Elapsed: 5*time.Minute + 16*time.Second},
		{PID: 26181, Agent: "claude", Kind: "desktop", Model: "claude-opus-5",
			ResumeID: "ae1af7de-c887-4d7f-b8ab-f8b225b891c5",
			Elapsed:  15*time.Hour + 31*time.Minute + 22*time.Second},
		{PID: 34451, Agent: "codex", Kind: "desktop",
			Command: "codex -c features.code_mode_host=true app-server",
			Elapsed: 26*time.Hour + 3*time.Minute + 4*time.Second},
		{PID: 55267, Agent: "claude", Kind: "terminal", Command: "claude --continue",
			Elapsed: 14*time.Hour + 19*time.Second},
	}
	if len(got) != len(want) {
		t.Fatalf("parsed %d sessions, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		g := got[i]
		// The desktop command line is long and its exact text is not the
		// point; the fields pulled out of it are.
		if w.Command == "" {
			g.Command = ""
		}
		if g != w {
			t.Errorf("session %d:\n got %+v\nwant %+v", i, g, w)
		}
	}
}

// The path of the desktop app's binary has a space in it, so "first token" is
// the wrong definition of the executable.
func TestExecutableSurvivesASpaceInThePath(t *testing.T) {
	exe, args := executable("/Users/x/Library/Application Support/Claude/claude.app/Contents/MacOS/claude --model m --resume=abc")
	if exe != "/Users/x/Library/Application Support/Claude/claude.app/Contents/MacOS/claude" {
		t.Errorf("exe = %q", exe)
	}
	if len(args) != 3 || args[0] != "--model" || args[2] != "--resume=abc" {
		t.Errorf("args = %q", args)
	}
}

func TestArgValueAcceptsBothSpellings(t *testing.T) {
	args := []string{"--model", "claude-opus-5", "--resume=abc", "--continue"}
	if got := argValue(args, "--model"); got != "claude-opus-5" {
		t.Errorf("--model = %q", got)
	}
	if got := argValue(args, "--resume", "-r"); got != "abc" {
		t.Errorf("--resume = %q", got)
	}
	// A flag with no value must not swallow the next flag as its value.
	if got := argValue([]string{"--resume", "--continue"}, "--resume"); got != "" {
		t.Errorf("--resume with no value = %q", got)
	}
}

func TestElapsed(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want time.Duration
	}{
		{"00:41", 41 * time.Second},
		{"05:16", 5*time.Minute + 16*time.Second},
		{"15:31:22", 15*time.Hour + 31*time.Minute + 22*time.Second},
		{"1-02:03:04", 26*time.Hour + 3*time.Minute + 4*time.Second},
		{"garbage", 0},
	} {
		if got := elapsed(tc.in); got != tc.want {
			t.Errorf("elapsed(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

// Checked against the directories Claude Code actually created under
// ~/.claude/projects: a space and a dot become dashes like a slash does.
func TestSlugMatchesClaudeCodesProjectDirectories(t *testing.T) {
	for _, tc := range []struct{ dir, want string }{
		{"/Users/lakshgoyal/hackqueue", "-Users-lakshgoyal-hackqueue"},
		{"/Users/lakshgoyal", "-Users-lakshgoyal"},
		{"/Users/lakshgoyal/Library/Application Support/Claude/scratch-workspaces/scratch-2026-09-09-9a0e50",
			"-Users-lakshgoyal-Library-Application-Support-Claude-scratch-workspaces-scratch-2026-09-09-9a0e50"},
		{"/Users/lakshgoyal/.ao/data/worktrees/skeptic/skeptic-1", "-Users-lakshgoyal--ao-data-worktrees-skeptic-skeptic-1"},
	} {
		if got := Slug(tc.dir); got != tc.want {
			t.Errorf("Slug(%q) = %q, want %q", tc.dir, got, tc.want)
		}
	}
}

func TestStateNeverClaimsBlocked(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		last time.Time
		want string
	}{
		{time.Time{}, "unknown"},
		{now.Add(-30 * time.Second), "working"},
		{now.Add(-3 * time.Minute), "idle"},
	} {
		if got := (Session{LastActivity: tc.last}).State(now); got != tc.want {
			t.Errorf("State(last=%v) = %q, want %q", tc.last, got, tc.want)
		}
	}
}

// writeTranscript lays down a transcript under a fake ~/.claude/projects with
// the given last-write time, in the two-line shape Claude leaves behind: a
// timestamped record followed by a bookkeeping line that carries none.
func writeTranscript(t *testing.T, projects, dir, id string, last time.Time) string {
	t.Helper()
	slug := filepath.Join(projects, Slug(dir))
	if err := os.MkdirAll(slug, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(slug, id+".jsonl")
	body := `{"type":"assistant","timestamp":"` + last.UTC().Format(time.RFC3339Nano) + `","sessionId":"` + id + `"}` + "\n" +
		`{"type":"last-prompt","sessionId":"` + id + `"}` + "\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, last, last); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTranscriptsAreMatchedOnlyWhenTheMatchIsCertain(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	projects := ProjectsDir()
	now := time.Now().Truncate(time.Second)

	// A session that named its transcript, whose last line has no timestamp.
	named := writeTranscript(t, projects, "/work/a", "aaaa", now.Add(-30*time.Second))
	// One that named a transcript kept under a different directory's slug.
	moved := writeTranscript(t, projects, "/work/old", "bbbb", now.Add(-10*time.Minute))
	// A lone bare session: one transcript from before it started, one since.
	writeTranscript(t, projects, "/work/c", "stale", now.Add(-2*time.Hour))
	fresh := writeTranscript(t, projects, "/work/c", "fresh", now.Add(-5*time.Minute))
	// Two bare sessions in one directory: nothing on disk says which is whose.
	writeTranscript(t, projects, "/work/d", "either", now.Add(-time.Minute))

	list := []Session{
		{PID: 1, Agent: "claude", Dir: "/work/a", ResumeID: "aaaa", Elapsed: time.Hour},
		{PID: 2, Agent: "claude", Dir: "/work/new", ResumeID: "bbbb", Elapsed: time.Hour},
		{PID: 3, Agent: "claude", Dir: "/work/c", Elapsed: time.Hour},
		{PID: 4, Agent: "claude", Dir: "/work/d", Elapsed: time.Hour},
		{PID: 5, Agent: "claude", Dir: "/work/d", Elapsed: time.Hour},
		{PID: 6, Agent: "codex", Dir: "/work/a", Elapsed: time.Hour},
		{PID: 7, Agent: "claude", Dir: "", Elapsed: time.Hour},
	}
	attachTranscripts(list, now)

	want := map[int]struct {
		transcript string
		state      string
	}{
		1: {named, "working"},
		2: {moved, "idle"},
		3: {fresh, "idle"},
		4: {"", "unknown"},
		5: {"", "unknown"},
		6: {"", "unknown"},
		7: {"", "unknown"},
	}
	for _, s := range list {
		w := want[s.PID]
		if s.Transcript != w.transcript {
			t.Errorf("pid %d: transcript %q, want %q", s.PID, s.Transcript, w.transcript)
		}
		if got := s.State(now); got != w.state {
			t.Errorf("pid %d: state %q, want %q (last activity %v)", s.PID, got, w.state, s.LastActivity)
		}
	}
	if !list[0].LastActivity.Equal(now.Add(-30 * time.Second)) {
		t.Errorf("pid 1: last activity %v was not read from the timestamped line", list[0].LastActivity)
	}
}

// A transcript with no timestamped line in its tail still has an mtime, and
// that is a better answer than "unknown" for a file that is visibly being
// written to.
func TestLastTimestampFallsBackToTheFilesOwnTime(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.jsonl")
	if err := os.WriteFile(p, []byte(`{"type":"mode","mode":"normal"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(p, at, at); err != nil {
		t.Fatal(err)
	}
	if got := lastTimestamp(p); !got.Equal(at) {
		t.Errorf("got %v, want the mtime %v", got, at)
	}
}
