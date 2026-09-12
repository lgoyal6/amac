package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAHackqueueBuildIsRecognisedByItsDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if got := hackqueueSlug(filepath.Join(home, "hackqueue-builds", "volthacks-6e3df37a", "src")); got != "volthacks-6e3df37a" {
		t.Fatalf("slug = %q", got)
	}
	if got := hackqueueSlug(filepath.Join(home, "hackqueue")); got != "" {
		t.Fatalf("the hackqueue repo itself is not a build, got %q", got)
	}
	if got := hackqueueSlug(""); got != "" {
		t.Fatalf("no dir, no slug, got %q", got)
	}
}

// The stream is rendered as speech, tool calls named rather than dumped, and
// only the tail is kept.
func TestAStreamRendersAsWhoSaidWhat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude.log")
	lines := []string{
		`{"type":"system","subtype":"init","session_id":"x"}`,
		`{"type":"user","message":{"role":"user","content":"Build the approved idea.\nSecond line."}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Starting with the runner."},{"type":"tool_use","name":"Bash","input":{"command":"npm test"}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"ok"}]}}`,
		`not json at all`,
		`{"type":"result","result":"{\"verdict\":\"ready\"}"}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := renderStream(path, 50)
	for _, want := range []string{"-- session started --", "you: Build the approved idea. ...", "claude: Starting with the runner.", "  > Bash npm test", "  < result", `result: {"verdict":"ready"}`} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "not json") {
		t.Fatalf("a line that is not an event must not leak through:\n%s", got)
	}
	if tail := renderStream(path, 2); strings.Count(tail, "\n") != 1 {
		t.Fatalf("want the last two lines only, got:\n%s", tail)
	}
}

func TestTheNewestBuildLogWins(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	runs := filepath.Join(home, "Documents", "agent-materials", "hackqueue_materials", "runs", "slug-1")
	for _, v := range []string{"build-v1", "build-v2"} {
		if err := os.MkdirAll(filepath.Join(runs, v), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(runs, v, "claude.log"), []byte(v+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// v1 touched an hour ago and v2 two hours ago: a retry can leave the older
	// numbered log as the one still being written, so mtime decides, not the
	// version number.
	if err := os.Chtimes(filepath.Join(runs, "build-v1", "claude.log"), fileTime(1), fileTime(1)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(runs, "build-v2", "claude.log"), fileTime(2), fileTime(2)); err != nil {
		t.Fatal(err)
	}
	if got := buildLog("slug-1"); !strings.HasSuffix(got, filepath.Join("build-v1", "claude.log")) {
		t.Fatalf("want the most recently written log, got %q", got)
	}
	if got := buildLog("no-such-slug"); got != "" {
		t.Fatalf("no log, empty string, got %q", got)
	}
}

func fileTime(hoursAgo int) time.Time { return time.Now().Add(-time.Duration(hoursAgo) * time.Hour) }
