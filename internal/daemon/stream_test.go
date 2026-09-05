package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lgoyal6/amac/internal/event"
)

// The stream is Server-Sent Events rather than a WebSocket for one reason: SSE
// carries an event id, browsers resend the last one they saw as Last-Event-ID
// after a drop, and the ids here are the log's own sequence numbers, so
// reconnect-with-replay is the protocol's default instead of something
// hand-rolled. Phones drop connections constantly, which makes that the whole
// argument for the choice, and none of it had a test.

type sseEvent struct {
	ID    int64
	Kind  string
	Data  map[string]any
	Alive bool // a keepalive comment frame rather than an event
}

// readSSE consumes frames until it has n events or the deadline passes. It
// parses the wire format rather than the handler's internals, because the thing
// under test is what a browser receives.
func readSSE(t *testing.T, body *bufio.Reader, n int, deadline time.Duration) []sseEvent {
	t.Helper()
	done := time.After(deadline)
	out := []sseEvent{}
	lines := make(chan string, 64)
	go func() {
		for {
			line, err := body.ReadString('\n')
			if err != nil {
				close(lines)
				return
			}
			lines <- strings.TrimRight(line, "\n")
		}
	}()

	var cur sseEvent
	for len(out) < n {
		select {
		case <-done:
			return out
		case line, ok := <-lines:
			if !ok {
				return out
			}
			switch {
			case strings.HasPrefix(line, ": "):
				out = append(out, sseEvent{Alive: true})
			case strings.HasPrefix(line, "id: "):
				id, _ := strconv.ParseInt(strings.TrimPrefix(line, "id: "), 10, 64)
				cur.ID = id
			case strings.HasPrefix(line, "event: "):
				cur.Kind = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &cur.Data)
			case line == "":
				if cur.Kind != "" {
					out = append(out, cur)
					cur = sseEvent{}
				}
			}
		}
	}
	return out
}

func appendN(t *testing.T, s *Server, n int, kind event.Kind) []int64 {
	t.Helper()
	var seqs []int64
	for i := range n {
		e, err := event.New(kind, "test", "s1", map[string]any{"i": i})
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.log.Append(context.Background(), e)
		if err != nil {
			t.Fatal(err)
		}
		seqs = append(seqs, got.Seq)
	}
	return seqs
}

// openStream runs the handler against a live server so the response can be read
// while it is still open. httptest.NewRecorder buffers, which cannot express a
// stream that never ends.
func openStream(t *testing.T, s *Server, query string, lastID string) (*bufio.Reader, func()) {
	t.Helper()
	srv := httptest.NewServer(s.Handler())
	req, err := http.NewRequest("GET", srv.URL+"/api/stream"+query, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Amac-Token", tok)
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		srv.Close()
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		resp.Body.Close()
		srv.Close()
		t.Fatalf("stream returned %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("content-type = %q, want text/event-stream", ct)
	}
	return bufio.NewReader(resp.Body), func() { resp.Body.Close(); srv.Close() }
}

// The property the protocol choice was made for. A phone that dropped resends
// the last id it saw, and must get everything after it and nothing before.
func TestReconnectReplaysExactlyWhatWasMissed(t *testing.T) {
	s := testServer(t)
	seqs := appendN(t, s, 6, event.KindObservation)
	missedFrom := seqs[2] // pretend the phone saw the first three

	body, closeIt := openStream(t, s, "", strconv.FormatInt(missedFrom, 10))
	defer closeIt()

	got := readSSE(t, body, 3, 5*time.Second)
	var ids []int64
	for _, e := range got {
		if !e.Alive {
			ids = append(ids, e.ID)
		}
	}
	if len(ids) != 3 {
		t.Fatalf("replayed %d events, want the 3 that were missed: %v", len(ids), ids)
	}
	for _, id := range ids {
		if id <= missedFrom {
			t.Errorf("replayed event %d, which the client had already seen", id)
		}
	}
	// A gap is the failure mode that matters. Duplicates are fine for a client
	// tracking the last sequence it rendered; a missing event is not.
	for i := 1; i < len(ids); i++ {
		if ids[i] != ids[i-1]+1 {
			t.Errorf("gap in the replay: %v", ids)
		}
	}
}

// Every frame has to carry the sequence as its id, or Last-Event-ID has nothing
// to be. This is the join between the protocol and the log.
func TestEachFrameCarriesItsSequenceAndKind(t *testing.T) {
	s := testServer(t)
	appendN(t, s, 2, event.KindAutomationRun)

	body, closeIt := openStream(t, s, "?since=0", "")
	defer closeIt()

	got := readSSE(t, body, 2, 5*time.Second)
	if len(got) < 2 {
		t.Fatalf("read %d frames", len(got))
	}
	for _, e := range got {
		if e.Alive {
			continue
		}
		if e.ID == 0 {
			t.Error("a frame arrived without an id; reconnect would replay everything")
		}
		if e.Kind != string(event.KindAutomationRun) {
			t.Errorf("event type = %q, want the kind so a client can filter without parsing", e.Kind)
		}
		if e.Data["seq"] == nil {
			t.Errorf("payload should carry the event: %v", e.Data)
		}
	}
}

// Live events must arrive without polling, which is the other half of why this
// endpoint exists.
func TestEventsAppendedWhileConnectedArrive(t *testing.T) {
	s := testServer(t)
	head, _ := s.log.Head(context.Background())

	body, closeIt := openStream(t, s, "?since="+strconv.FormatInt(head, 10), "")
	defer closeIt()

	// Give the subscription a moment to be in place before appending, so this
	// tests delivery rather than the backlog path.
	time.Sleep(150 * time.Millisecond)
	appendN(t, s, 1, event.KindActuation)

	got := readSSE(t, body, 1, 5*time.Second)
	var live []sseEvent
	for _, e := range got {
		if !e.Alive {
			live = append(live, e)
		}
	}
	if len(live) == 0 {
		t.Fatal("an event appended while connected never arrived")
	}
	if live[0].Kind != string(event.KindActuation) {
		t.Errorf("got %q, want the event just appended", live[0].Kind)
	}
}

// The stream is behind the token like everything else. It is a live feed of
// everything happening on this machine.
func TestTheStreamNeedsTheToken(t *testing.T) {
	s := testServer(t)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("unauthenticated stream returned %d, want 401", resp.StatusCode)
	}
}

// Buffering is what breaks SSE behind a proxy: the client waits for a buffer to
// fill that never does. The headers that disable it are load-bearing.
func TestStreamDisablesBuffering(t *testing.T) {
	s := testServer(t)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/api/stream", nil)
	req.Header.Set("X-Amac-Token", tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Cache-Control"); !strings.Contains(got, "no-cache") {
		t.Errorf("Cache-Control = %q, want no-cache", got)
	}
	if got := resp.Header.Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering = %q, want no", got)
	}
}
