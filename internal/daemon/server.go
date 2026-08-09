package daemon

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/lgoyal6/amac/internal/agent"
	"github.com/lgoyal6/amac/internal/event"
	"github.com/lgoyal6/amac/internal/supervisor"
)

//go:embed ui/*
var uiFS embed.FS

type Server struct {
	sup   *supervisor.Supervisor
	log   *event.Log
	token string
}

func New(sup *supervisor.Supervisor, log *event.Log, token string) *Server {
	return &Server{sup: sup, log: log, token: token}
}

// Token returns the shared secret, creating it on first run.
//
// The tailnet is the outer gate; this is the inner one. Anything else on the
// tailnet (a compromised device, a shared node) should still not be able to
// drive agents on this machine.
func Token() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(home, ".amac", "token")
	if b, err := os.ReadFile(path); err == nil && len(b) >= 32 {
		return string(b), nil
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(buf)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	// 0600: the token is equivalent to shell access on this machine.
	if err := os.WriteFile(path, []byte(tok), 0o600); err != nil {
		return "", err
	}
	return tok, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/sessions", s.auth(s.listSessions))
	mux.HandleFunc("POST /api/sessions", s.auth(s.startSession))
	mux.HandleFunc("POST /api/sessions/{id}/prompt", s.auth(s.promptSession))
	mux.HandleFunc("POST /api/sessions/{id}/answer", s.auth(s.answerSession))
	mux.HandleFunc("DELETE /api/sessions/{id}", s.auth(s.stopSession))
	mux.HandleFunc("GET /api/events", s.auth(s.events))
	mux.HandleFunc("GET /api/stream", s.auth(s.stream))
	mux.HandleFunc("GET /api/agents", s.auth(s.agents))

	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		b, err := uiFS.ReadFile("ui/index.html")
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(b)
	})

	return mux
}

// auth accepts the token in a header or a query parameter. The query form is
// not laziness: EventSource cannot set headers, so an SSE stream has no other
// way to authenticate. Compared in constant time either way.
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("X-Amac-Token")
		if got == "" {
			got = r.URL.Query().Get("token")
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// ---------------------------------------------------------------- sessions --

type sessionView struct {
	ID      string       `json:"id"`
	Agent   string       `json:"agent"`
	Dir     string       `json:"dir"`
	State   string       `json:"state"`
	Detail  string       `json:"detail"`
	Started time.Time    `json:"started"`
	Pending *pendingView `json:"pending,omitempty"`
}

type pendingView struct {
	Title   string       `json:"title"`
	AskedAt time.Time    `json:"askedAt"`
	Options []optionView `json:"options"`
}

type optionView struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

func view(sess *supervisor.Session) sessionView {
	st, detail := sess.State()
	v := sessionView{
		ID: sess.ID, Agent: sess.Agent, Dir: sess.Dir,
		State: string(st), Detail: detail, Started: sess.Started,
	}
	if p := sess.Pending(); p != nil {
		pv := &pendingView{Title: p.Title, AskedAt: p.AskedAt}
		for _, o := range p.Options {
			pv.Options = append(pv.Options, optionView{OptionID: o.OptionID, Name: o.Name, Kind: o.Kind})
		}
		v.Pending = pv
	}
	return v
}

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	out := []sessionView{}
	for _, sess := range s.sup.List() {
		out = append(out, view(sess))
	}
	writeJSON(w, 200, out)
}

func (s *Server) agents(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, agent.Names())
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Agent  string `json:"agent"`
		Dir    string `json:"dir"`
		Prompt string `json:"prompt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if body.Agent == "" {
		body.Agent = "claude"
	}
	if body.Dir == "" {
		home, _ := os.UserHomeDir()
		body.Dir = home
	}

	// Spawning is slow enough (adapter start plus handshake) that holding the
	// request open would look like a hang on a phone. Detach from the request
	// context so a client that navigates away does not kill the session it
	// just asked for.
	sess, err := s.sup.Start(context.Background(), body.Agent, body.Dir)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	if body.Prompt != "" {
		go func() { _, _ = sess.Prompt(context.Background(), body.Prompt) }()
	}
	writeJSON(w, 200, view(sess))
}

func (s *Server) promptSession(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sup.Get(r.PathValue("id"))
	if !ok {
		writeJSON(w, 404, map[string]string{"error": "no such session"})
		return
	}
	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Text == "" {
		writeJSON(w, 400, map[string]string{"error": "text required"})
		return
	}
	// A turn can run for minutes and will block on permission requests. Return
	// immediately; progress arrives on the event stream.
	go func() { _, _ = sess.Prompt(context.Background(), body.Text) }()
	writeJSON(w, 202, map[string]string{"status": "accepted"})
}

func (s *Server) answerSession(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sup.Get(r.PathValue("id"))
	if !ok {
		writeJSON(w, 404, map[string]string{"error": "no such session"})
		return
	}
	var body struct {
		OptionID string `json:"optionId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if err := sess.Answer(body.OptionID); err != nil {
		writeJSON(w, 409, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]string{"status": "answered"})
}

func (s *Server) stopSession(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sup.Get(r.PathValue("id"))
	if !ok {
		writeJSON(w, 404, map[string]string{"error": "no such session"})
		return
	}
	sess.Close()
	writeJSON(w, 200, map[string]string{"status": "stopped"})
}

// ---------------------------------------------------------------- events ----

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	evs, err := s.log.Since(r.Context(), since, limit)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	if evs == nil {
		evs = []event.Event{}
	}
	writeJSON(w, 200, evs)
}

// stream is Server-Sent Events rather than a WebSocket. The choice is not
// arbitrary: SSE carries an event id, and browsers resend the last one they
// saw as Last-Event-ID after a drop. Since our ids are the log's sequence
// numbers, reconnect-with-replay is the protocol's default behaviour instead
// of something to hand-roll, and phones drop connections constantly.
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", 500)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)

	// Subscribe before replaying, so an event appended between the two is
	// delivered late rather than lost. Duplicates are fine for a client that
	// tracks the last sequence it rendered; a gap is not.
	live, unsub := s.log.Subscribe(512)
	defer unsub()

	from, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	if lastID := r.Header.Get("Last-Event-ID"); lastID != "" {
		if n, err := strconv.ParseInt(lastID, 10, 64); err == nil {
			from = n
		}
	}

	sent := from
	if backlog, err := s.log.Since(r.Context(), from, 500); err == nil {
		for _, e := range backlog {
			writeEvent(w, e)
			sent = e.Seq
		}
	}
	flusher.Flush()

	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return

		case e, ok := <-live:
			if !ok {
				return
			}
			if e.Seq <= sent {
				continue // already delivered in the backlog
			}
			writeEvent(w, e)
			sent = e.Seq
			flusher.Flush()

		case <-ticker.C:
			// Comment frame: keeps intermediaries and sleeping phones from
			// silently dropping an idle connection.
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

func writeEvent(w http.ResponseWriter, e event.Event) {
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", e.Seq, e.Kind, b)
}
