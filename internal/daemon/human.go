package daemon

import (
	"net/http"
	"strings"

	"github.com/lgoyal6/amac/internal/event"
)

// Recording what a person did, as opposed to what a page fetched.
//
// amac has always known what it sent. The half it did not know was what
// happened next, and the first attempt at that recorded one thing: a board
// page load. Counted over the four weeks before this was written, that plus
// permission answers, task claims, filings and actuations came to about two
// observable human actions a day against 111 notifications a day, which puts
// two hundred labelled examples roughly a hundred days out. The problem was
// never that a person does little. It is that almost none of it was recorded.
//
// The rule is whether an agent could have caused it. A GET is the board
// polling, and treating a poll as engagement is how the last label ended up
// measuring adapter chattiness instead of a person. Every write through the
// authenticated API took a deliberate act: somebody answered a permission,
// sent a prompt, stopped a session, claimed a task.
//
// Recorded around the handler rather than inside each one, because a rule
// enforced in twenty places is a rule that is missing from the twenty-first.

// deliberate reports whether a request is an act rather than a poll.
func deliberate(r *http.Request) bool {
	switch r.Method {
	case http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodDelete:
		return true
	}
	// The one read that is a decision. Somebody typed a question; the board
	// never issues this on its own.
	return r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/search")
}

// witness wraps an authenticated handler so the act is recorded once it has
// been accepted. Nothing is recorded for a request that was refused, since a
// 404 on a stale session id is a thumb landing on something that is no longer
// there rather than a decision about it.
func (s *Server) witness(pattern string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !deliberate(r) {
			next(w, r)
			return
		}
		rec := &statusWriter{ResponseWriter: w}
		next(rec, r)
		if rec.status >= 400 {
			return
		}
		s.recordAct(r, pattern, "")
	}
}

// recordAct writes the fact. notice names the notification the act answers,
// when the act arrived by a link that carried one.
func (s *Server) recordAct(r *http.Request, action, notice string) {
	payload := map[string]any{"action": action, "method": r.Method}
	session := r.PathValue("id")
	if session == "" {
		session = r.URL.Query().Get("session")
	}
	if session != "" {
		payload["session"] = session
	}
	if notice != "" {
		payload["notice"] = notice
	}
	// How the request arrived, because it separates a thumb on a phone from a
	// terminal, and those are different kinds of attention. Not a security
	// claim: a header is not evidence, and nothing is authorised by it.
	if r.Header.Get("Sec-Fetch-Site") != "" || r.Header.Get("Origin") != "" || r.Referer() != "" {
		payload["via"] = "browser"
	} else {
		payload["via"] = "cli"
	}

	ev, err := event.New(event.KindHumanActed, "human", session, payload)
	if err != nil {
		return
	}
	// Fire and forget, like every other observability write here: losing the
	// record of an action must never fail the action.
	_, _ = s.log.Append(r.Context(), ev)
}

// statusWriter remembers the status so a refused request is not recorded as a
// decision. It forwards everything else untouched.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// Flush keeps the SSE stream working through the wrapper. Without it the
// stream buffers until the connection closes, which is the whole endpoint
// broken by an observability decorator.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
