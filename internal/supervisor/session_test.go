package supervisor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lgoyal6/amac/internal/acp"
	"github.com/lgoyal6/amac/internal/event"
)

func testSup(t *testing.T) *Supervisor {
	t.Helper()
	log, err := event.Open(filepath.Join(t.TempDir(), "events.db"), event.Relaxed)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	return New(log)
}

// asking builds a session parked on a permission request, without a real agent
// behind it. The reply channel is what an answer actually travels down, so the
// test reads from it rather than asserting on state alone.
func asking(t *testing.T, opts ...acp.PermissionOption) (*Session, chan acp.PermissionOutcome) {
	t.Helper()
	reply := make(chan acp.PermissionOutcome, 1)
	s := &Session{ID: "am-test", Agent: "claude", sup: testSup(t)}
	s.pending = &Pending{
		ToolCallID: "tc1", Title: "Run rm -rf /", Options: opts,
		AskedAt: time.Now(), reply: reply,
	}
	s.state = StateBlocked
	return s, reply
}

func opt(id, kind string) acp.PermissionOption {
	return acp.PermissionOption{OptionID: id, Name: id, Kind: kind}
}

// An option the agent never offered must be refused. The board sends whatever
// the card was rendered from, and a card can be stale by the time a thumb
// reaches it; answering with an id this question does not have would resolve it
// as something nobody chose.
func TestAnswerRefusesAnOptionThatWasNotOffered(t *testing.T) {
	s, reply := asking(t, opt("allow", "allow_once"), opt("deny", "reject_once"))

	err := s.Answer("allow_always")
	if err == nil {
		t.Fatal("an unoffered option must be refused")
	}
	if !strings.Contains(err.Error(), "allow") || !strings.Contains(err.Error(), "deny") {
		t.Errorf("the refusal should name what was on offer, got %v", err)
	}
	select {
	case out := <-reply:
		t.Fatalf("nothing should have been sent to the agent, got %+v", out)
	default:
	}

	// And a real option still works afterwards: the refusal does not poison it.
	if err := s.Answer("deny"); err != nil {
		t.Fatalf("a valid answer after a refused one failed: %v", err)
	}
	if out := <-reply; out.OptionID != "deny" {
		t.Fatalf("agent received %+v", out)
	}
}

// The dashboard and the phone can both be looking at the same question, so two
// answers must not become two replies on one channel.
func TestAnsweringTwiceSendsOnce(t *testing.T) {
	s, reply := asking(t, opt("allow", "allow_once"))

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = s.Answer("allow") }()
	}
	wg.Wait()

	if got := <-reply; got.OptionID != "allow" {
		t.Fatalf("got %+v", got)
	}
	select {
	case extra := <-reply:
		t.Fatalf("the agent was answered twice: %+v", extra)
	default:
	}
}

// An empty option is a cancellation, not a selection of nothing. The agent
// treats those differently: cancelled means the human declined to decide.
func TestAnEmptyAnswerCancels(t *testing.T) {
	s, reply := asking(t, opt("allow", "allow_once"))
	if err := s.Answer(""); err != nil {
		t.Fatal(err)
	}
	out := <-reply
	if out.Outcome != acp.OutcomeCancelled {
		t.Fatalf("got %q, want cancelled", out.Outcome)
	}
	if out.OptionID != "" {
		t.Errorf("a cancellation must not name an option, got %q", out.OptionID)
	}
}

func TestAnsweringWithNothingPendingIsAnError(t *testing.T) {
	s := &Session{ID: "am-test", sup: testSup(t)}
	if err := s.Answer("allow"); err == nil {
		t.Fatal("answering a session with no question must fail")
	}
}

// The documented security property: prefer allow_once over allow_always. A
// policy that silently grants standing permission changes what every future
// turn may do without anyone having decided that.
func TestLeastPermissiveAllowPrefersOnce(t *testing.T) {
	p := &Pending{Options: []acp.PermissionOption{
		opt("always", "allow_always"),
		opt("once", "allow_once"),
		opt("no", "reject_once"),
	}}
	got, ok := LeastPermissiveAllow(p)
	if !ok || got != "once" {
		t.Fatalf("got %q ok=%v, want the once option", got, ok)
	}

	// Order must not decide it: once still wins when it is listed last.
	p2 := &Pending{Options: []acp.PermissionOption{
		opt("always", "allow_always"), opt("once", "allow_once"),
	}}
	if got, _ := LeastPermissiveAllow(p2); got != "once" {
		t.Fatalf("listing order changed the answer: got %q", got)
	}

	// With no narrow option, a broad allow is better than parking forever.
	p3 := &Pending{Options: []acp.PermissionOption{opt("always", "allow_always")}}
	if got, ok := LeastPermissiveAllow(p3); !ok || got != "always" {
		t.Fatalf("got %q ok=%v", got, ok)
	}

	// Nothing affirmative on offer means the policy declines to choose, and
	// the request falls through to a human rather than being answered wrongly.
	p4 := &Pending{Options: []acp.PermissionOption{opt("no", "reject_once")}}
	if _, ok := LeastPermissiveAllow(p4); ok {
		t.Fatal("a policy with no allow option must not claim to have decided")
	}
}

func TestRejectAllDecidesAndDenies(t *testing.T) {
	id, ok := RejectAll(&Pending{Options: []acp.PermissionOption{opt("allow", "allow_once")}})
	if !ok {
		t.Fatal("RejectAll must decide, or the run parks waiting for a human")
	}
	if id != "" {
		t.Fatalf("a denial must not name an option, got %q", id)
	}
}

// ------------------------------------------------------------------ files ---

func TestReadTextFileRefusesARelativePath(t *testing.T) {
	if _, err := readTextFile(acp.ReadTextFileParams{Path: "../../etc/passwd"}); err == nil {
		t.Fatal("a relative path must be refused")
	}
}

func TestReadTextFileWindowing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree\nfour\nfive"), 0o644); err != nil {
		t.Fatal(err)
	}
	line := func(n int) *int { return &n }

	for _, tc := range []struct {
		name        string
		line, limit *int
		want        string
	}{
		{"whole file", nil, nil, "one\ntwo\nthree\nfour\nfive"},
		// Line numbers are 1-based per the ACP spec, so line 2 is "two".
		{"from a line", line(2), nil, "two\nthree\nfour\nfive"},
		{"a window", line(2), line(2), "two\nthree"},
		{"a limit past the end is not an error", line(4), line(99), "four\nfive"},
		// Reading past the end returns nothing rather than failing: an agent
		// asking for line 900 of a 5-line file has made a harmless mistake.
		{"past the end", line(900), nil, ""},
	} {
		got, err := readTextFile(acp.ReadTextFileParams{Path: path, Line: tc.line, Limit: tc.limit})
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestReadTextFileMissingFile(t *testing.T) {
	if _, err := readTextFile(acp.ReadTextFileParams{
		Path: filepath.Join(t.TempDir(), "nope.txt")}); err == nil {
		t.Fatal("a missing file must be an error, not empty content")
	}
}

// ------------------------------------------------------------------- ids -----

// Session ids end up as tmux names and in the log, so a collision would merge
// two sessions' history into one.
func TestSessionIDsAreUnique(t *testing.T) {
	sup := testSup(t)
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		id, err := sup.newID("claude")
		if err != nil {
			t.Fatal(err)
		}
		if seen[id] {
			t.Fatalf("%s was issued twice", id)
		}
		seen[id] = true
		if !strings.HasPrefix(id, "claude-") {
			t.Fatalf("%s should name its agent", id)
		}
	}
}

// ---------------------------------------------------------------- listing ---

func TestBlockedListsOnlyWhatIsWaiting(t *testing.T) {
	sup := testSup(t)
	for i, st := range []State{StateBlocked, StateWorking, StateBlocked, StateIdle, StateEnded} {
		s := &Session{ID: fmt.Sprintf("s%d", i), sup: sup, state: st}
		if st == StateBlocked {
			s.pending = &Pending{Title: "?", reply: make(chan acp.PermissionOutcome, 1)}
		}
		sup.mu.Lock()
		sup.sessions[s.ID] = s
		sup.mu.Unlock()
	}

	blocked := sup.Blocked()
	if len(blocked) != 2 {
		t.Fatalf("got %d blocked, want 2", len(blocked))
	}
	for _, s := range blocked {
		if st, _ := s.State(); st != StateBlocked {
			t.Errorf("%s is %s, not blocked", s.ID, st)
		}
	}
	if got := len(sup.List()); got != 5 {
		t.Errorf("List returned %d, want all 5", got)
	}
}
