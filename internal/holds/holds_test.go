package holds

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/lgoyal6/amac/internal/event"
)

func open(t *testing.T) *Holds {
	t.Helper()
	log, err := event.Open(filepath.Join(t.TempDir(), "events.db"), event.Relaxed)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	h, err := Open(log)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

const min = time.Minute

// The whole point. Two agents cannot both believe they own a file.
func TestTwoSessionsCannotHoldTheSameFile(t *testing.T) {
	h := open(t)
	ctx := context.Background()

	if _, err := h.Claim(ctx, "session-a", []string{"/repo/server.go"}, min, "editing"); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	conflicts, err := h.Claim(ctx, "session-b", []string{"/repo/server.go"}, min, "also editing")
	if !errors.Is(err, ErrHeld) {
		t.Fatalf("second claim = %v, want ErrHeld", err)
	}
	if len(conflicts) != 1 || conflicts[0].Owner != "session-a" {
		t.Errorf(`"no" must say by whom, got %+v`, conflicts)
	}
}

// A path is a tree. Claiming a directory claims what is under it, and the
// check has to work from both directions or the second agent wins by asking
// the question the other way round.
func TestDirectoryAndFileContendBothWays(t *testing.T) {
	ctx := context.Background()

	h := open(t)
	if _, err := h.Claim(ctx, "a", []string{"/repo/internal/daemon"}, min, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Claim(ctx, "b", []string{"/repo/internal/daemon/server.go"}, min, ""); !errors.Is(err, ErrHeld) {
		t.Errorf("a file inside a held directory should conflict, got %v", err)
	}

	h2 := open(t)
	if _, err := h2.Claim(ctx, "a", []string{"/repo/internal/daemon/server.go"}, min, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h2.Claim(ctx, "b", []string{"/repo/internal/daemon"}, min, ""); !errors.Is(err, ErrHeld) {
		t.Errorf("a directory containing a held file should conflict, got %v", err)
	}

	// A sibling whose name merely shares a prefix is not inside anything.
	h3 := open(t)
	if _, err := h3.Claim(ctx, "a", []string{"/repo/daemon"}, min, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h3.Claim(ctx, "b", []string{"/repo/daemon2"}, min, ""); err != nil {
		t.Errorf("daemon2 is not inside daemon, got %v", err)
	}
}

// Granting four of five paths is the worst outcome available: the agent
// believes it has permission and is wrong about one file, which is exactly the
// case that produces a diff nobody can explain.
func TestAClaimIsAllOrNothing(t *testing.T) {
	h := open(t)
	ctx := context.Background()

	if _, err := h.Claim(ctx, "a", []string{"/repo/c.go"}, min, ""); err != nil {
		t.Fatal(err)
	}
	want := []string{"/repo/a.go", "/repo/b.go", "/repo/c.go", "/repo/d.go"}
	if _, err := h.Claim(ctx, "b", want, min, ""); !errors.Is(err, ErrHeld) {
		t.Fatalf("claim = %v, want ErrHeld", err)
	}

	// Nothing from the failed set may be left held, or the next agent is
	// blocked by a claim that was never granted to anyone.
	live, err := h.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || live[0].Path != "/repo/c.go" {
		t.Errorf("a refused claim must leave no holds behind, got %+v", live)
	}
}

// An agent that dies must not hold a file forever.
func TestAnExpiredHoldIsReclaimable(t *testing.T) {
	h := open(t)
	ctx := context.Background()
	now := time.Now().UTC()
	h.now = func() time.Time { return now }

	if _, err := h.Claim(ctx, "dead", []string{"/repo/x.go"}, time.Second, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Claim(ctx, "alive", []string{"/repo/x.go"}, min, ""); !errors.Is(err, ErrHeld) {
		t.Fatal("a live hold should still block")
	}

	now = now.Add(2 * time.Second)
	got, err := h.Claim(ctx, "alive", []string{"/repo/x.go"}, min, "")
	if err != nil {
		t.Fatalf("an expired hold should be reclaimable: %v", err)
	}
	if got[0].Token <= 1 {
		t.Errorf("reclaiming must issue a higher token, got %d", got[0].Token)
	}
}

// The failure every lease scheme has: the original holder wakes up after being
// declared dead and acts on work its replacement now owns. The token is what
// makes that a rejection rather than a silent corruption.
func TestARevivedHolderCannotReleaseItsReplacement(t *testing.T) {
	h := open(t)
	ctx := context.Background()
	now := time.Now().UTC()
	h.now = func() time.Time { return now }

	first, err := h.Claim(ctx, "stalled", []string{"/repo/x.go"}, time.Second, "")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Second)
	second, err := h.Claim(ctx, "replacement", []string{"/repo/x.go"}, min, "")
	if err != nil {
		t.Fatal(err)
	}

	// The stalled session wakes and tries to tidy up after itself.
	if err := h.Release(ctx, "stalled", first[0].Token, []string{"/repo/x.go"}); err == nil {
		t.Error("a stale token must not release the replacement's hold")
	}
	live, _ := h.List(ctx)
	if len(live) != 1 || live[0].Owner != "replacement" || live[0].Token != second[0].Token {
		t.Errorf("the replacement should still hold it, got %+v", live)
	}
}

// Re-claiming your own live hold is how an agent extends its work, not a
// deadlock against itself.
func TestReclaimingYourOwnHoldIsNotAConflict(t *testing.T) {
	h := open(t)
	ctx := context.Background()
	if _, err := h.Claim(ctx, "a", []string{"/repo/x.go"}, min, "first pass"); err != nil {
		t.Fatal(err)
	}
	got, err := h.Claim(ctx, "a", []string{"/repo/x.go", "/repo/y.go"}, min, "second pass")
	if err != nil {
		t.Fatalf("an owner re-claiming its own path: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("got %d holds, want 2", len(got))
	}
	// Extending your own grant must not invalidate the token you are holding,
	// or an agent that worked longer than its lease could no longer release
	// its own files.
	for _, x := range got {
		if x.Path == "/repo/x.go" && x.Token != 1 {
			t.Errorf("re-claiming your own path bumped the token to %d", x.Token)
		}
	}
}

// The property that matters under real concurrency: whatever the interleaving,
// exactly one session comes away believing it owns the file.
func TestConcurrentClaimsNeverOverlap(t *testing.T) {
	h := open(t)
	ctx := context.Background()

	const workers = 12
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_, err := h.Claim(ctx, string(rune('a'+n)), []string{"/repo/contended.go"}, min, "")
			if err == nil {
				mu.Lock()
				won++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	if won != 1 {
		t.Errorf("%d of %d workers claimed the same file; exactly 1 may", won, workers)
	}
	live, _ := h.List(ctx)
	if len(live) != 1 {
		t.Errorf("table holds %d rows for one path", len(live))
	}
}

// Who is what an agent calls before it edits, so it has to find a contending
// hold whether the other session named the file, its directory, or a parent.
func TestWhoFindsContendingHolds(t *testing.T) {
	h := open(t)
	ctx := context.Background()
	if _, err := h.Claim(ctx, "a", []string{"/repo/internal"}, min, "refactor"); err != nil {
		t.Fatal(err)
	}
	got, err := h.Who(ctx, "/repo/internal/daemon/server.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Owner != "a" || got[0].Note != "refactor" {
		t.Errorf("got %+v, want a's refactor hold", got)
	}
	if other, _ := h.Who(ctx, "/repo/docs/readme.md"); len(other) != 0 {
		t.Errorf("an unrelated path should be free, got %+v", other)
	}
}

// A session that ends holding files must not leave them locked until the lease
// runs out, because the next thing the human does is start another session.
func TestReleaseAllClearsASession(t *testing.T) {
	h := open(t)
	ctx := context.Background()
	if _, err := h.Claim(ctx, "ending", []string{"/repo/a.go", "/repo/b.go"}, min, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Claim(ctx, "other", []string{"/repo/c.go"}, min, ""); err != nil {
		t.Fatal(err)
	}
	n, err := h.ReleaseAll(ctx, "ending")
	if err != nil || n != 2 {
		t.Fatalf("released %d (%v), want 2", n, err)
	}
	live, _ := h.List(ctx)
	if len(live) != 1 || live[0].Owner != "other" {
		t.Errorf("only the ending session's holds should go, got %+v", live)
	}
}

// Paths are compared as strings, so they have to be one string before they get
// to the table or the same file is holdable several ways.
func TestPathsAreNormalisedBeforeComparison(t *testing.T) {
	h := open(t)
	ctx := context.Background()
	if _, err := h.Claim(ctx, "a", []string{"/repo/x.go"}, min, ""); err != nil {
		t.Fatal(err)
	}
	for _, spelling := range []string{"/repo/./x.go", "/repo//x.go", "/repo/sub/../x.go", "/repo/x.go "} {
		if _, err := h.Claim(ctx, "b", []string{spelling}, min, ""); !errors.Is(err, ErrHeld) {
			t.Errorf("%q should contend with /repo/x.go, got %v", spelling, err)
		}
	}
}
