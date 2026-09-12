package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The hackathon board, served whole.
//
// hackqueue renders a complete board already: a rail grouped by what is waiting
// on you, and a detail pane carrying the full researched brief, the ranked
// ideas, the sourced requirements and every decision button. It was reachable
// from exactly one machine, through a loopback proxy on the laptop that injects
// the bearer token, which is the machine you are least likely to be holding
// when a deadline is six hours out.
//
// The first attempt at this tab re-rendered a summary of that board in amac's
// own JavaScript: two lists, a title and a status each. It threw away the brief,
// the ideas, the requirements and the layout, which is to say it threw away
// everything that makes the board worth opening, and it would have drifted from
// board.ts the first time either changed.
//
// So amac serves the real one. `/board` and `/board/*` proxy straight through
// with the bearer attached, the tab is that page full-bleed, and the forms
// inside it post to the same paths on this origin. Nothing is reimplemented and
// nothing can drift, because there is only one board.
//
// The page's own local console is the one part that does not come along. It
// probes `/__console` and only draws itself if something answers, and the thing
// that answers is the laptop proxy spawning Claude in a build directory. A
// 404 here is that check failing, which is exactly what it is for.

const (
	hackqueueEnvDefault = ".config/hackathon-bot.env"
	hackqueueTimeout    = 30 * time.Second
)

func homePath(rest string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return rest
	}
	return filepath.Join(home, rest)
}

// The secrets stay in the one 0600 file hackqueue already keeps them in. Copying
// them into amac's config would make a second place to rotate and a second place
// to leak, for no gain: both processes are this user on this machine.
func hackqueueEnv() (base, secret string, err error) {
	path := os.Getenv("HACKQUEUE_ENV_FILE")
	if path == "" {
		path = homePath(hackqueueEnvDefault)
	}
	file, err := os.Open(path)
	if err != nil {
		return "", "", fmt.Errorf("hackqueue env file: %w", err)
	}
	defer file.Close()

	scan := bufio.NewScanner(file)
	for scan.Scan() {
		key, value, found := strings.Cut(strings.TrimSpace(scan.Text()), "=")
		if !found {
			continue
		}
		switch strings.TrimSpace(key) {
		case "HACKQUEUE_URL":
			base = strings.TrimSpace(value)
		case "QUEUE_SECRET":
			secret = strings.TrimSpace(value)
		}
	}
	if base == "" || secret == "" {
		return "", "", fmt.Errorf("hackqueue env file has no HACKQUEUE_URL or QUEUE_SECRET")
	}
	return base, secret, nil
}

// Only the board. Not `/queue`, which is the API the dispatcher drives and has
// no business being reachable from a browser tab, and not an open relay: the
// path is rebuilt from a fixed prefix and a single cleaned segment rather than
// taken from the request, so nothing a URL says can reach another route.
func hackboardTarget(base, path string) (string, bool) {
	if path == "/board" {
		return base + "/board", true
	}
	action, ok := strings.CutPrefix(path, "/board/")
	if !ok || action == "" || strings.ContainsAny(action, "/?#") {
		return "", false
	}
	switch action {
	case "pass", "retry", "direction", "build", "chat", "submission", "result":
		return base + "/board/" + action, true
	}
	return "", false
}

func (s *Server) hackboard(w http.ResponseWriter, r *http.Request) {
	base, secret, err := hackqueueEnv()
	if err != nil {
		http.Error(w, "hackqueue is not configured on this machine: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	target, ok := hackboardTarget(base, r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), hackqueueTimeout)
	defer cancel()

	// The body is capped rather than streamed. Everything posted here is a form
	// with a few fields, and the one that is not is a chat message with a length
	// limit of its own, so anything larger is a mistake worth refusing.
	var body io.Reader
	if r.Method == http.MethodPost {
		body = http.MaxBytesReader(w, r.Body, 1<<20)
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, target, body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	req.Header.Set("authorization", "Bearer "+secret)
	if ct := r.Header.Get("content-type"); ct != "" {
		req.Header.Set("content-type", ct)
	}

	// The board answers a decision with 303 back to /board, which is already the
	// right path on this origin, so the redirect is handed to the browser rather
	// than followed here. Following it would fetch the whole page again to throw
	// it away, and would turn a POST into a GET inside the proxy where the
	// browser can no longer see what happened.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		http.Error(w, "hackqueue is unreachable: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer res.Body.Close()

	for _, header := range []string{"content-type", "location", "cache-control"} {
		if v := res.Header.Get(header); v != "" {
			w.Header().Set(header, v)
		}
	}
	// This page is rendered from a queue that changes under it, and it is the
	// one surface where a stale render could show a decision that was already
	// made somewhere else.
	if w.Header().Get("cache-control") == "" {
		w.Header().Set("cache-control", "no-store")
	}
	w.WriteHeader(res.StatusCode)
	_, _ = io.Copy(w, res.Body)
}

// Just the number on the tab, and nothing else.
//
// The board is a whole page, and the point of a badge is to save you opening
// it. So this reads the queue and counts the entries that are waiting on a
// person rather than on a machine, which is the same question the board's own
// header answers in words once you are already looking at it.
//
// It is deliberately not the two-list JSON this file used to serve. That tried
// to be a second board and was a worse one; this is a count.
func (s *Server) hackqueueCount(w http.ResponseWriter, r *http.Request) {
	base, secret, err := hackqueueEnv()
	if err != nil {
		writeJSON(w, 200, map[string]any{"needsYou": 0, "warning": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), hackqueueTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/queue", nil)
	if err != nil {
		writeJSON(w, 200, map[string]any{"needsYou": 0, "warning": err.Error()})
		return
	}
	req.Header.Set("authorization", "Bearer "+secret)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		// A badge that cannot be counted shows nothing rather than a zero,
		// because zero here means "nothing needs you", which is a claim.
		writeJSON(w, 200, map[string]any{"needsYou": 0, "warning": err.Error()})
		return
	}
	defer res.Body.Close()

	var queue map[string]json.RawMessage
	if err := json.NewDecoder(res.Body).Decode(&queue); err != nil {
		writeJSON(w, 200, map[string]any{"needsYou": 0, "warning": err.Error()})
		return
	}
	needs := 0
	// The four the board files under "needs you": a direction to pick, three
	// ideas to choose between, a build to submit, and a failure to resolve.
	for _, status := range []string{"pending", "idea_pending", "ready", "failed"} {
		var entries []struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(queue[status], &entries); err != nil {
			continue
		}
		for _, e := range entries {
			if e.ID != "" {
				needs++
			}
		}
	}
	writeJSON(w, 200, map[string]any{"needsYou": needs})
}
