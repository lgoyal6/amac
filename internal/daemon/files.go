package daemon

// Reading a session's work from the phone.
//
// The pane mirror shows what the agent is saying. It does not show what the
// agent has done, and those are different questions: an agent that says it
// fixed the bug and an agent that has fixed the bug look identical in a
// terminal. The diff is the answer to the second one, and reviewing it was the
// last thing that still required walking to the machine.
//
// Everything here is read-only and confined to the session's own directory.
// The board can already type into a pane, so this adds no capability that was
// not there; what it adds is the ability to check before you do.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/lgoyal6/amac/internal/tmux"
)

// maxFile is what a phone can usefully show and what a request should carry.
// A source file over this is a generated one, and a diff over this is not a
// diff anyone is reading on a phone.
const maxFile = 256 << 10

// dirFor resolves the working directory of a session, ACP or tmux.
//
// The context is threaded rather than passed as nil. tmuxSessions queries the
// event log, and database/sql dereferences the context it is handed: a nil one
// panics inside the driver, the panic is recovered by net/http with the
// connection left mid-query, and every request after it blocks forever waiting
// for a pool that never comes back. The symptom is a dashboard that works once
// and then hangs, which took a two-minute timeout to see.
func (s *Server) dirFor(ctx context.Context, id string) (string, bool) {
	if sess, ok := s.sup.Get(id); ok && sess.Dir != "" {
		return sess.Dir, true
	}
	// tmux knows the directory on its own; no need to go through the log or
	// the attention join that tmuxSessions does for the board.
	list, err := tmux.List()
	if err != nil {
		return "", false
	}
	for _, t := range list {
		if t.Name == id && t.Dir != "" {
			return t.Dir, true
		}
	}
	return "", false
}

// within resolves a requested path inside a root and refuses anything that
// escapes it.
//
// Both sides are symlink-resolved before comparison. A prefix check on the
// unresolved strings passes for a symlink inside the root that points anywhere
// on the disk, which is the whole trick, and this endpoint is reachable from a
// phone with a token.
func within(root, rel string) (string, bool) {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", false
	}
	target := filepath.Join(realRoot, filepath.Clean("/"+rel))
	real, err := filepath.EvalSymlinks(target)
	if err != nil {
		// Not yet on disk is a 404 for the caller, not a traversal: still check
		// the unresolved path stays inside before saying so.
		real = target
	}
	if real != realRoot && !strings.HasPrefix(real, realRoot+string(os.PathSeparator)) {
		return "", false
	}
	return real, true
}

type fileEntry struct {
	Name  string    `json:"name"`
	Dir   bool      `json:"dir"`
	Size  int64     `json:"size"`
	Mtime time.Time `json:"mtime"`
}

func (s *Server) files(w http.ResponseWriter, r *http.Request) {
	root, ok := s.dirFor(r.Context(), r.PathValue("id"))
	if !ok {
		writeJSON(w, 404, map[string]string{"error": "no such session"})
		return
	}
	rel := r.URL.Query().Get("path")
	target, ok := within(root, rel)
	if !ok {
		writeJSON(w, 400, map[string]string{"error": "path escapes the session directory"})
		return
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": err.Error()})
		return
	}

	out := []fileEntry{}
	for _, e := range entries {
		// The two directories that are always huge and never what you opened a
		// phone to read.
		if e.Name() == "node_modules" || e.Name() == ".git" {
			continue
		}
		fe := fileEntry{Name: e.Name(), Dir: e.IsDir()}
		if fi, err := e.Info(); err == nil {
			fe.Size, fe.Mtime = fi.Size(), fi.ModTime()
		}
		out = append(out, fe)
	}
	// Directories first, then newest, because on a session you are watching the
	// file you want is nearly always the one just written.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir
		}
		return out[i].Mtime.After(out[j].Mtime)
	})
	writeJSON(w, 200, map[string]any{"path": rel, "root": root, "entries": out})
}

func (s *Server) file(w http.ResponseWriter, r *http.Request) {
	root, ok := s.dirFor(r.Context(), r.PathValue("id"))
	if !ok {
		writeJSON(w, 404, map[string]string{"error": "no such session"})
		return
	}
	target, ok := within(root, r.URL.Query().Get("path"))
	if !ok {
		writeJSON(w, 400, map[string]string{"error": "path escapes the session directory"})
		return
	}
	fi, err := os.Stat(target)
	if err != nil || fi.IsDir() {
		writeJSON(w, 404, map[string]string{"error": "not a readable file"})
		return
	}
	if fi.Size() > maxFile {
		writeJSON(w, 413, map[string]string{
			"error": fmt.Sprintf("%.0fKB is too large to read here", float64(fi.Size())/1024)})
		return
	}
	b, err := os.ReadFile(target)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"path": r.URL.Query().Get("path"), "text": string(b)})
}

// diff is what the agent has actually done, as opposed to what it says.
//
// Uncommitted work only, which is the right scope: an agent that has committed
// has left a message and a history to read, and one that has not is exactly the
// case where the terminal tells you nothing useful.
func (s *Server) diff(w http.ResponseWriter, r *http.Request) {
	dir, ok := s.dirFor(r.Context(), r.PathValue("id"))
	if !ok {
		writeJSON(w, 404, map[string]string{"error": "no such session"})
		return
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		writeJSON(w, 200, map[string]any{"dir": dir, "repo": false,
			"status": "", "diff": "not a git repository"})
		return
	}

	git := func(args ...string) string {
		out, _ := exec.CommandContext(r.Context(), "git",
			append([]string{"-C", dir}, args...)...).Output()
		if len(out) > maxFile {
			return string(out[:maxFile]) + "\n...[truncated]"
		}
		return string(out)
	}
	writeJSON(w, 200, map[string]any{
		"dir":    dir,
		"repo":   true,
		"branch": strings.TrimSpace(git("rev-parse", "--abbrev-ref", "HEAD")),
		"status": git("status", "--short"),
		// Staged and unstaged together: an agent that has run `git add` is
		// mid-thought, not finished, and showing only one half would report
		// half its work.
		"diff": git("diff", "HEAD", "--stat") + "\n" + git("diff", "HEAD"),
	})
}
