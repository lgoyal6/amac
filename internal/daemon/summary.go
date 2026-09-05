package daemon

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"sync"
)

// Home renders from seven feeds. On a laptop that is seven cheap requests; on
// a phone on the tailnet it is seven round trips before the screen has
// anything on it, and round trips are what the page waits on, not bytes.
//
// So the whole screen arrives in one request. Each part is served by the same
// handler that serves it at its own URL, rather than by a fresh read of the
// database: a summary that answers "what did I spend today" in its own code is
// a second answer to that question, and two answers to one question disagree
// eventually. The handlers run concurrently, because serialising seven of them
// behind one request would trade seven parallel round trips for seven
// sequential queries, which is not obviously an improvement and might be worse.
//
// Every part carries its own status. Home already distinguishes a feed that
// failed from a feed that is empty, and collapsing seven statuses into one
// would take that away: an unreadable health check and a healthy machine
// looking identical is the exact failure that behaviour exists to prevent.
type summaryPart struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body,omitempty"`
}

// recorder captures a handler's reply instead of writing it to the wire. It is
// what httptest.ResponseRecorder is, minus a test-only dependency in the
// daemon binary, and it needs only the three methods the handlers here use.
type recorder struct {
	head   http.Header
	status int
	body   bytes.Buffer
}

func (rec *recorder) Header() http.Header {
	if rec.head == nil {
		rec.head = http.Header{}
	}
	return rec.head
}

func (rec *recorder) WriteHeader(code int) {
	if rec.status == 0 {
		rec.status = code
	}
}

func (rec *recorder) Write(b []byte) (int, error) {
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	return rec.body.Write(b)
}

func (s *Server) summary(w http.ResponseWriter, r *http.Request) {
	// The queries are the ones home was already sending. limit=1 is enough for
	// the jobs tile because the handler counts separately from the page it
	// returns, and the due rows are filtered by the database rather than
	// downloaded and filtered here.
	feeds := []struct {
		key, query string
		h          http.HandlerFunc
	}{
		{"health", "", s.health},
		{"tasks", "", s.tasks},
		{"spend", "", s.spend},
		{"jobs", "limit=1", s.listApplications},
		{"due", "limit=1000&due=1", s.listApplications},
		{"runs", "", s.healthRuns},
		{"machine", "", s.machine},
	}

	out := make(map[string]summaryPart, len(feeds))
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, f := range feeds {
		wg.Add(1)
		go func(key, query string, h http.HandlerFunc) {
			defer wg.Done()
			// Cloned from the incoming request so the caller hanging up still
			// cancels the seven reads underneath it, and so anything a handler
			// reads off the request is what it would have read anyway.
			sub := r.Clone(r.Context())
			sub.Method = http.MethodGet
			u := *r.URL
			u.RawQuery = query
			sub.URL = &u
			sub.Form = nil
			if q, err := url.ParseQuery(query); err == nil {
				sub.Form = q
			}

			rec := &recorder{}
			h(rec, sub)

			part := summaryPart{Status: rec.status}
			if part.Status == 0 {
				part.Status = http.StatusOK
			}
			// A handler that wrote something unparseable is reported by status
			// alone rather than by corrupting the envelope around it.
			if body := bytes.TrimSpace(rec.body.Bytes()); json.Valid(body) {
				part.Body = json.RawMessage(body)
			}
			mu.Lock()
			out[key] = part
			mu.Unlock()
		}(f.key, f.query, f.h)
	}
	wg.Wait()
	writeJSON(w, 200, out)
}
