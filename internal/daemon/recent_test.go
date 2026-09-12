package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lgoyal6/amac/internal/event"
	"github.com/lgoyal6/amac/internal/health"
)

func writeTranscript(t *testing.T, root, slug, name string, lines []string, mtime time.Time) {
	t.Helper()
	dir := filepath.Join(root, slug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func closeTo(t *testing.T, got, want time.Time, what string) {
	t.Helper()
	if d := got.Sub(want); d > time.Second || d < -time.Second {
		t.Fatalf("%s: got %v, want %v", what, got, want)
	}
}

// A transcript can be hundreds of megabytes and everything this needs is on
// the last few lines, so only the tail is read. The head of this one names a
// different project and a different model, and neither may be believed.
func TestRecentSessionsReadTheTailOfEachTranscript(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	since := now.Add(-24 * time.Hour)

	lines := []string{`{"type":"user","timestamp":"2026-09-10T00:00:00Z","cwd":"/old/head","message":{"model":"head-model"}}`}
	pad := `{"type":"progress","data":"` + strings.Repeat("x", 1000) + `"}`
	for i := 0; i < 100; i++ {
		lines = append(lines, pad)
	}
	lines = append(lines,
		`{"type":"assistant","timestamp":"`+stamp(now.Add(-2*time.Hour))+`","cwd":"/Users/x/amac","message":{"model":"claude-opus-5"}}`,
		`{"type":"user","timestamp":"`+stamp(now.Add(-time.Hour))+`","cwd":"/Users/x/amac","message":{"role":"user"}}`,
		`{"type":"last-prompt","lastPrompt":"hi"}`,
	)
	writeTranscript(t, root, "-Users-x-amac", "a.jsonl", lines, now.Add(-time.Hour))
	// Untouched since before the window, however new its lines claim to be.
	writeTranscript(t, root, "-Users-x-stale", "b.jsonl", lines, now.Add(-48*time.Hour))
	// Nothing on its lines to go by: the file's own mtime and directory stand in.
	writeTranscript(t, root, "-Users-x-bare", "c.jsonl", []string{`{"type":"summary"}`}, now.Add(-30*time.Minute))
	// Not a transcript.
	writeTranscript(t, root, "-Users-x-amac", "notes.txt", []string{"x"}, now)
	// A subagent transcript belongs to the session above it.
	writeTranscript(t, root, "-Users-x-amac/sess/subagents", "agent.jsonl", lines, now)

	items, warnings := recentSessions(root, since)
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if len(items) != 2 {
		t.Fatalf("want 2 sessions, got %d: %+v", len(items), items)
	}
	byProject := map[string]recentItem{}
	for _, it := range items {
		byProject[it.Project] = it
	}
	amac := byProject["amac"]
	if amac.Kind != "session" || amac.Title != "session in amac" || amac.Detail != "claude-opus-5" {
		t.Fatalf("the tail was not read: %+v", amac)
	}
	closeTo(t, amac.At, now.Add(-time.Hour), "last timestamp on the tail")
	bare := byProject["bare"]
	if bare.Detail != "claude" || bare.Title != "session in bare" {
		t.Fatalf("the fallbacks did not hold: %+v", bare)
	}
	closeTo(t, bare.At, now.Add(-30*time.Minute), "mtime fallback")
}

func TestRecentSessionsNameAnUnreadableRoot(t *testing.T) {
	items, warnings := recentSessions(filepath.Join(t.TempDir(), "gone"), time.Now())
	if len(items) != 0 || len(warnings) != 1 || !strings.Contains(warnings[0], "claude transcripts") {
		t.Fatalf("want one named warning and nothing else, got %v / %v", items, warnings)
	}
}

func TestReadTailDropsThePartialFirstLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(path, []byte("first line\nsecond\nthird\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readTail(path, 10)
	if err != nil || string(got) != "third\n" {
		t.Fatalf("want the whole lines inside the tail, got %q, %v", got, err)
	}
	got, err = readTail(path, 1000)
	if err != nil || string(got) != "first line\nsecond\nthird\n" {
		t.Fatalf("a short file is read whole, got %q, %v", got, err)
	}
	// One line longer than the tail leaves nothing complete to read.
	got, err = readTail(path, 3)
	if err != nil || len(got) != 0 {
		t.Fatalf("want an empty tail, got %q, %v", got, err)
	}
}

// Every half-hourly reaper run is a delivery, and a day of them would be the
// whole feed. Same outcome in a row folds into one line; a failure breaks it.
// An automation the run log does not count is read from the sweep instead, and
// one it does count is not read twice.
func TestRecentAutomationsFoldStreaksAndFallBackToTheSweep(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	since := now.Add(-24 * time.Hour)
	at := func(h int) time.Time { return now.Add(-time.Duration(h) * time.Hour) }
	runs := []health.Run{
		{Automation: "reaper", ID: "1", Status: health.RunOK, Started: at(1)},
		{Automation: "reaper", ID: "2", Status: health.RunOK, Started: at(2)},
		{Automation: "reaper", ID: "3", Status: health.RunFailed, Started: at(3)},
		{Automation: "reaper", ID: "4", Status: health.RunOK, Started: at(4)},
		{Automation: "reaper", ID: "5", Status: health.RunSkipped, Started: at(5)},
		{Automation: "reaper", ID: "6", Status: health.RunOK, Started: at(30)},
		{Automation: "hacklist-sf", ID: "7", Status: health.RunOK, Started: at(6)},
	}
	reports := []health.Report{
		{Name: "hacklist", State: health.OK, Last: at(2)},
		{Name: "n8n-thing", State: health.Failing, Last: at(7)},
		{Name: "quiet", State: health.OK, Last: at(40)},
		{Name: "disk", Category: "machine", State: health.OK, Last: at(1)},
		{Name: "never", State: health.Unknown},
	}
	roster := []health.Automation{
		{Name: "hacklist", Home: "/Users/x/hacklist"},
		{Name: "reaper"},
		{Name: "disk", Category: "machine", Home: "/nowhere"},
	}

	items := newestFirst(recentAutomations(runs, reports, roster, since, now), recentCap)
	var got []string
	for _, it := range items {
		got = append(got, it.Title+" | "+it.Project+" | "+it.Detail+" | "+it.State+" | "+it.At.Format("15h"))
	}
	want := []string{
		"reaper | reaper | delivered ×2 | ok | 11h",
		"reaper | reaper | failed | failing | 09h",
		"reaper | reaper | delivered | ok | 08h",
		"hacklist | hacklist | delivered | ok | 06h",
		"n8n-thing | n8n-thing | failed | failing | 05h",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, it := range items {
		if it.Kind != "automation" {
			t.Fatalf("wrong kind: %+v", it)
		}
	}
}

// Each transition in the window is one line, in words rather than status
// codes, and an open entry closing inside two days is a line of its own dated
// at the deadline. Closed entries do not get a deadline, blank rows and keys
// that are not lists are skipped rather than failed on.
func TestRecentHacksWordEachTransitionAndFlagTheDeadlines(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	since := now.Add(-24 * time.Hour)
	iso := func(d time.Duration) string { return stamp(now.Add(d)) }
	body := `{
	  "seen": ["a","b","c","d"],
	  "ready": [{"id":"a","title":"Frontier Hacks","status":"ready","updatedAt":"` + iso(-time.Hour) + `","closes":"` + iso(30*time.Hour) + `"}],
	  "expired": [{"id":"b","title":"Old One","status":"expired","decidedAt":"` + iso(-2*time.Hour) + `","closes":"` + iso(-time.Hour) + `"}],
	  "building": [{"id":"c","title":"Slow Build","status":"building","updatedAt":"` + iso(-40*time.Hour) + `","closes":"` + iso(6*time.Hour) + `"}],
	  "done": [{"id":"d","title":"Shipped","status":"done","updatedAt":"` + iso(-3*time.Hour) + `","closes":"` + iso(10*time.Hour) + `"}],
	  "idea_pending": [{"id":"e","updatedAt":"` + iso(-4*time.Hour) + `","closes":null}],
	  "pending": [{"id":"","title":"blank"}],
	  "denied": "not a list"
	}`
	items, err := recentHacks([]byte(body), since, now)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, it := range newestFirst(items, recentCap) {
		got = append(got, it.Kind+" | "+it.Project+" | "+it.Title+" | "+it.Detail+" | "+it.At.Format("02T15"))
	}
	want := []string{
		"hack | hackqueue | Frontier Hacks | closes in 30h | 13T18",
		"hack | hackqueue | Slow Build | closes in 6h | 12T18",
		"hack | hackqueue | Frontier Hacks | built and waiting to submit | 12T11",
		"hack | hackqueue | Old One | expired undecided | 12T10",
		"hack | hackqueue | Shipped | submitted | 12T09",
		"hack | hackqueue | e | ideas ready, waiting for a pick | 12T08",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if _, err := recentHacks([]byte("<html>"), since, now); err == nil {
		t.Fatal("a body that is not the queue must be an error, not an empty day")
	}
}

func TestNewestFirstSortsAndCaps(t *testing.T) {
	now := time.Now()
	var items []recentItem
	for i := 0; i < 5; i++ {
		items = append(items, recentItem{At: now.Add(time.Duration(i) * time.Minute), Title: string(rune('a' + i))})
	}
	got := newestFirst(items, 3)
	if len(got) != 3 || got[0].Title != "e" || got[1].Title != "d" || got[2].Title != "c" {
		t.Fatalf("got %+v", got)
	}
}

// The handler against all three sources at once: a transcript under HOME, a
// run in the log, and a queue behind the bearer. Then the Worker goes away,
// and the feed says so by name while the other two carry on.
func TestRecentMergesTheThreeSourcesAndNamesTheOneItCannotRead(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	now := time.Now()
	writeTranscript(t, filepath.Join(home, ".claude", "projects"), "-Users-x-amac", "s.jsonl",
		[]string{`{"type":"assistant","timestamp":"` + stamp(now.Add(-time.Hour)) + `","cwd":"/Users/x/amac","message":{"model":"claude-opus-5"}}`},
		now.Add(-time.Hour))

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/queue" || r.Header.Get("authorization") != "Bearer sekrit" {
			w.WriteHeader(401)
			return
		}
		_, _ = w.Write([]byte(`{"seen":["a"],"ready":[{"id":"a","title":"Frontier Hacks","status":"ready","updatedAt":"` + stamp(now.Add(-2*time.Hour)) + `","closes":null}]}`))
	}))
	t.Cleanup(upstream.Close)
	envFile := filepath.Join(home, "bot.env")
	if err := os.WriteFile(envFile, []byte("HACKQUEUE_URL="+upstream.URL+"\nQUEUE_SECRET=sekrit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HACKQUEUE_ENV_FILE", envFile)

	srv := testServer(t)
	run := health.Run{Automation: "hacklist", ID: "r1", Status: health.RunFailed, Started: now.Add(-3 * time.Hour), Detail: "run failure"}
	ev, err := event.New(event.KindAutomationRun, "test", "", run)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.log.Append(context.Background(), ev); err != nil {
		t.Fatal(err)
	}

	call := func() (out struct {
		Since    time.Time    `json:"since"`
		Items    []recentItem `json:"items"`
		Warnings []string     `json:"warnings"`
	}) {
		w := httptest.NewRecorder()
		srv.recent(w, httptest.NewRequest("GET", "/api/recent?hours=abc", nil))
		if w.Code != 200 {
			t.Fatalf("want 200, got %d: %s", w.Code, w.Body)
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	out := call()
	closeTo(t, out.Since, now.Add(-24*time.Hour), "an unparseable hours falls back to a day")
	if len(out.Warnings) != 0 {
		t.Fatalf("every source was readable, got warnings %v", out.Warnings)
	}
	var kinds []string
	for _, it := range out.Items {
		kinds = append(kinds, it.Kind+":"+it.Title+":"+it.Detail)
	}
	want := "session:session in amac:claude-opus-5, hack:Frontier Hacks:built and waiting to submit, automation:hacklist:failed"
	if strings.Join(kinds, ", ") != want {
		t.Fatalf("got %q\nwant %q", strings.Join(kinds, ", "), want)
	}

	upstream.Close()
	out = call()
	if len(out.Warnings) != 1 || !strings.HasPrefix(out.Warnings[0], "hackqueue: ") {
		t.Fatalf("the unreachable Worker must be named: %v", out.Warnings)
	}
	if len(out.Items) != 2 {
		t.Fatalf("the other two sources must carry on, got %+v", out.Items)
	}
}
