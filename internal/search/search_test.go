package search

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lgoyal6/amac/internal/event"
)

func testLog(t *testing.T) *event.Log {
	t.Helper()
	l, err := event.Open(filepath.Join(t.TempDir(), "s.db"), event.Relaxed)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func add(t *testing.T, l *event.Log, kind event.Kind, session string, payload map[string]any) int64 {
	t.Helper()
	e, err := event.New(kind, "test", session, payload)
	if err != nil {
		t.Fatal(err)
	}
	got, err := l.Append(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}
	return got.Seq
}

// The question the whole package exists for, and the one amac could not answer
// yesterday except by scrolling.
func TestFindsWhatAnAgentWasDoingWhenItAsked(t *testing.T) {
	l := testLog(t)
	add(t, l, event.KindSessionState, "am-claude-1", map[string]any{
		"detail": "Rebasing the release branch onto main before tagging."})
	want := add(t, l, event.KindPermissionRequested, "am-claude-1", map[string]any{
		"title": "git branch -D stale-migration-experiment"})
	add(t, l, event.KindAttention, "am-codex-2", map[string]any{
		"message": "The build is green and nothing needs you."})

	hits, err := Query(context.Background(), l, "stale AND branch", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("got %d hits, want the one permission request: %+v", len(hits), hits)
	}
	if hits[0].Seq != want {
		t.Errorf("hit is event %d, want %d", hits[0].Seq, want)
	}
	if !strings.Contains(hits[0].Snippet, "stale-migration-experiment") {
		t.Errorf("snippet does not carry the match: %q", hits[0].Snippet)
	}
	if hits[0].Session != "am-claude-1" {
		t.Errorf("session = %q, so a hit cannot be opened in context", hits[0].Session)
	}
}

// The property the contentless index is chosen for. Retention rewrites old
// rows in place, and an index holding its own copy would go on answering with
// text the log no longer has. A stale index is fine; a stale answer is not.
func TestARedactedRowStopsBeingFound(t *testing.T) {
	l := testLog(t)
	seq := add(t, l, event.KindAttention, "am-claude-1", map[string]any{
		"message": "The deployment pipeline collapsed spectacularly overnight."})

	if hits, err := Query(context.Background(), l, "spectacularly", 10); err != nil {
		t.Fatal(err)
	} else if len(hits) != 1 {
		t.Fatalf("the row was not findable before redaction: %d hits", len(hits))
	}

	// Redaction as retention performs it: the field goes, the row stays. The
	// index still carries the word, and is deliberately not rebuilt here.
	if _, err := l.DB().Exec(
		`UPDATE events SET payload = ? WHERE seq = ?`, `{"redacted":true}`, seq); err != nil {
		t.Fatal(err)
	}

	hits, err := Query(context.Background(), l, "spectacularly", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Errorf("a redacted row was returned with text the log no longer holds: %+v", hits)
	}
}

// Indexing is incremental because the log is append-only. Re-running it must
// cost nothing and must not duplicate what is already there.
func TestIndexingIsIncrementalAndIdempotent(t *testing.T) {
	l := testLog(t)
	for range 3 {
		add(t, l, event.KindAttention, "s", map[string]any{"message": "the quick brown fox jumped"})
	}
	n, err := Update(context.Background(), l)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("indexed %d rows, want 3", n)
	}
	if n, err := Update(context.Background(), l); err != nil || n != 0 {
		t.Errorf("a second pass indexed %d rows (err %v), want 0", n, err)
	}

	add(t, l, event.KindAttention, "s", map[string]any{"message": "a fourth message arrives later"})
	if n, err := Update(context.Background(), l); err != nil || n != 1 {
		t.Errorf("the delta pass indexed %d rows (err %v), want 1", n, err)
	}
	hits, err := Query(context.Background(), l, "fox", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 3 {
		t.Errorf("got %d hits, want 3: re-indexing duplicated rows", len(hits))
	}
}

// A search box that returns nothing for a broken query and nothing for a query
// with no matches has told the user the same thing about two different
// situations.
func TestAnUnparseableQueryIsAnErrorNotAnEmptyResult(t *testing.T) {
	l := testLog(t)
	add(t, l, event.KindAttention, "s", map[string]any{"message": "something worth finding here"})

	err := func() error { _, e := Query(context.Background(), l, `unbalanced "quote`, 10); return e }()
	if err == nil {
		t.Fatal("a malformed query returned success")
	}
	// And it says so in the language the person was typing in. "SQL logic
	// error: unterminated string (1)" names a language they did not write
	// anything in, and appears in a search box on a phone.
	if !strings.Contains(err.Error(), "unclosed quote") {
		t.Errorf("error %q does not say what is wrong with the search", err)
	}
	for _, leak := range []string{"SQL", "sqlite", "fts5", "(1)"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error %q leaks %q at the user", err, leak)
		}
	}
	if _, err := Query(context.Background(), l, "   ", 10); err == nil {
		t.Error("an empty query returned success")
	}
	// And a well-formed query that matches nothing is an empty result, not an
	// error, so the two stay distinguishable.
	hits, err := Query(context.Background(), l, "nothinghereatall", 10)
	if err != nil {
		t.Fatalf("a valid query with no matches errored: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("got %d hits for a word that is not there", len(hits))
	}
}

// Payloads are mostly identifiers and enums. Indexing those would bury the
// prose that is the only reason to search, so Text is deliberately narrow, and
// the indexer and the confirmer must agree on it or a row can be indexed under
// words it will never be confirmed to hold.
func TestOnlyProseIsIndexed(t *testing.T) {
	got := Text([]byte(`{
		"message": "the retry loop keeps firing after cancel",
		"model": "deepseek-ai/DeepSeek-V4-Flash",
		"detail": "completed",
		"outcome": {"why": "the coalesce window had not elapsed yet"},
		"seq": 4471
	}`))
	if !strings.Contains(got, "retry loop") {
		t.Errorf("prose was not indexed: %q", got)
	}
	if !strings.Contains(got, "coalesce window") {
		t.Errorf("nested prose was not indexed: %q", got)
	}
	if strings.Contains(got, "DeepSeek") {
		t.Errorf("a model id was indexed as prose: %q", got)
	}
	if strings.Contains(got, "completed") {
		t.Errorf("a one-word status was indexed: %q", got)
	}
	if Text([]byte("not json")) != "" {
		t.Error("a payload that is not an object produced text")
	}
}
