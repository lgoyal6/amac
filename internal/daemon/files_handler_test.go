package daemon

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The file endpoints are reachable from a phone with a token and they serve
// bytes off this laptop, which makes them the highest-consequence handlers in
// the daemon. `within` had a test; the handlers on top of it did not, and a
// containment check is only worth what the code calling it does with the answer.

// sessionAt makes a real tmux session in dir.
//
// dirFor resolves a session id through the supervisor first and then through
// tmux, and the tmux path is the one a phone actually uses: most sessions on
// this machine were started in a terminal rather than by the daemon. A real
// tmux session exercises that lookup instead of substituting for it, which is
// also how the crew tests work.
func sessionAt(t *testing.T, s *Server, id, dir string) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("no tmux")
	}
	_ = exec.Command("tmux", "kill-session", "-t", "="+id).Run()
	if err := exec.Command("tmux", "new-session", "-d", "-s", id, "-c", dir).Run(); err != nil {
		t.Skipf("tmux unavailable: %v", err)
	}
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", "="+id).Run() })
}

func getJSON(t *testing.T, s *Server, path string) (int, map[string]any) {
	t.Helper()
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, authed("GET", path, ""))
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return w.Code, body
}

// Every shape of escape, refused. A prefix check on unresolved strings passes
// for a symlink inside the root pointing anywhere on disk, which is the whole
// trick, so the symlink cases are the ones that matter.
func TestFileEndpointsRefuseEveryEscape(t *testing.T) {
	s := testServer(t)
	root := t.TempDir()
	outside := t.TempDir()
	sessionAt(t, s, "sess", root)

	if err := os.WriteFile(filepath.Join(root, "inside.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("private"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A symlink that lives inside the root and points out of it.
	if err := os.Symlink(secret, filepath.Join(root, "escape.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escapedir")); err != nil {
		t.Fatal(err)
	}

	for _, rel := range []string{
		"../", "../../etc/passwd", "..%2f..%2fetc",
		"escape.txt",           // symlink to a file outside
		"escapedir/secret.txt", // through a symlinked directory
		"/etc/passwd",          // absolute, which Join must not honour
	} {
		code, body := getJSON(t, s, "/api/sessions/sess/file?path="+rel)
		if code == 200 {
			t.Errorf("path %q was served: %v", rel, body)
			continue
		}
		if text, _ := body["text"].(string); strings.Contains(text, "private") {
			t.Errorf("path %q leaked the file outside the root", rel)
		}
	}

	// And the ordinary case still works, or the check is just a wall.
	code, body := getJSON(t, s, "/api/sessions/sess/file?path=inside.txt")
	if code != 200 || body["text"] != "ok" {
		t.Errorf("a file inside the root was refused: %d %v", code, body)
	}
}

// The listing has the same exposure as the read and gets the same treatment.
func TestListingRefusesEscapesAndHidesTheNoise(t *testing.T) {
	s := testServer(t)
	root := t.TempDir()
	sessionAt(t, s, "sess", root)

	for _, d := range []string{"node_modules", ".git", "src"} {
		if err := os.Mkdir(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, body := getJSON(t, s, "/api/sessions/sess/files?path=")
	if code != 200 {
		t.Fatalf("listing the root failed: %d %v", code, body)
	}
	entries, _ := body["entries"].([]any)
	names := map[string]bool{}
	for _, e := range entries {
		m, _ := e.(map[string]any)
		names[m["name"].(string)] = true
	}
	// Always huge, never what you opened a phone to read.
	if names["node_modules"] || names[".git"] {
		t.Errorf("noise directories were listed: %v", names)
	}
	if !names["src"] || !names["main.go"] {
		t.Errorf("real entries missing: %v", names)
	}
	// Directories first, because on a session you are watching the file you
	// want is nearly always the one just written.
	if len(entries) > 1 {
		first, _ := entries[0].(map[string]any)
		if first["dir"] != true {
			t.Errorf("directories should sort first, got %v", first)
		}
	}

	// `..` clamps to the root rather than escaping it, because the request is
	// cleaned as an absolute path before being joined: Clean("/../..") is "/".
	// So this is allowed, and what matters is that it lists the session root
	// and not its parent.
	code, body = getJSON(t, s, "/api/sessions/sess/files?path=../..")
	if code != 200 {
		t.Fatalf("clamping to the root should succeed, got %d", code)
	}
	clamped := map[string]bool{}
	for _, e := range must(body["entries"]) {
		clamped[e["name"].(string)] = true
	}
	if !clamped["main.go"] {
		t.Errorf("`..` did not clamp to the session root: %v", clamped)
	}
	if clamped[filepath.Base(filepath.Dir(root))] {
		t.Errorf("`..` listed the parent directory: %v", clamped)
	}
}

// An unknown session is a 404 rather than a listing of whatever the daemon's
// own working directory happens to be.
func TestUnknownSessionServesNothing(t *testing.T) {
	s := testServer(t)
	for _, path := range []string{
		"/api/sessions/nope/files?path=",
		"/api/sessions/nope/file?path=x",
		"/api/sessions/nope/diff",
	} {
		if code, _ := getJSON(t, s, path); code != 404 {
			t.Errorf("%s returned %d, want 404", path, code)
		}
	}
}

// A phone should not be handed a hundred megabytes, and the refusal has to say
// what happened rather than time out.
func TestAnOversizedFileIsRefusedWithItsSize(t *testing.T) {
	s := testServer(t)
	root := t.TempDir()
	sessionAt(t, s, "sess", root)
	big := filepath.Join(root, "big.bin")
	if err := os.WriteFile(big, make([]byte, maxFile+1024), 0o644); err != nil {
		t.Fatal(err)
	}
	code, body := getJSON(t, s, "/api/sessions/sess/file?path=big.bin")
	if code != 413 {
		t.Fatalf("got %d, want 413", code)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "KB") {
		t.Errorf("the refusal should name the size: %q", msg)
	}
}

// A directory is not a readable file, and asking for one must not return its
// bytes or an empty success.
func TestADirectoryIsNotAFile(t *testing.T) {
	s := testServer(t)
	root := t.TempDir()
	sessionAt(t, s, "sess", root)
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if code, _ := getJSON(t, s, "/api/sessions/sess/file?path=sub"); code != 404 {
		t.Errorf("reading a directory returned %d, want 404", code)
	}
}

// diff is what the agent has actually done as opposed to what it says, so a
// directory that is not a repository has to say so rather than look like a
// clean tree, which would read as "it changed nothing".
func TestDiffDistinguishesNoRepoFromNoChanges(t *testing.T) {
	s := testServer(t)

	plain := t.TempDir()
	sessionAt(t, s, "plain", plain)
	code, body := getJSON(t, s, "/api/sessions/plain/diff")
	if code != 200 {
		t.Fatalf("got %d", code)
	}
	if body["repo"] != false {
		t.Errorf("a non-repository must report repo=false, got %v", body)
	}

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.email", "t@t"}, {"config", "user.name", "t"}} {
		if err := exec.Command("git", append([]string{"-C", repo}, args...)...).Run(); err != nil {
			t.Skipf("git unavailable: %v", err)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sessionAt(t, s, "repo", repo)

	code, body = getJSON(t, s, "/api/sessions/repo/diff")
	if code != 200 || body["repo"] != true {
		t.Fatalf("expected a repository: %d %v", code, body)
	}
	if status, _ := body["status"].(string); !strings.Contains(status, "a.txt") {
		t.Errorf("an untracked file should show in status: %q", status)
	}
}

// must unwraps the entries array, which every listing assertion needs.
func must(v any) []map[string]any {
	raw, _ := v.([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, e := range raw {
		if m, ok := e.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}
