package spend

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A transcript line the way Claude Code writes one: compact JSON, UTC stamp,
// the usage nested under message. Lines sharing an id are one response.
func assistantLine(at time.Time, id, model string, in, out, cacheRead, cacheWrite int64) string {
	return fmt.Sprintf(`{"parentUuid":"p","type":"assistant","timestamp":%q,"requestId":"req-%s","message":{"id":%q,"model":%q,"role":"assistant","usage":{"input_tokens":%d,"cache_creation_input_tokens":%d,"cache_read_input_tokens":%d,"output_tokens":%d}}}`,
		at.UTC().Format(time.RFC3339Nano), id, id, model, in, cacheWrite, cacheRead, out)
}

func writeTranscript(t *testing.T, path string, mtime time.Time, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func hourlyFixture(t *testing.T) (string, time.Time) {
	t.Helper()
	dir := t.TempDir()
	day := time.Date(2026, 9, 12, 0, 0, 0, 0, time.Local)
	now := day.Add(15 * time.Hour)

	// A long session: yesterday's turns first, then today's. The same response
	// appears twice (two content blocks, the second with the usage grown by
	// five output tokens), a user line mentions "usage" in its tool output,
	// and a summary line has no timestamp at all.
	writeTranscript(t, filepath.Join(dir, "-Users-me-repo", "s1.jsonl"), now.Add(time.Hour),
		`{"type":"summary","summary":"old work","leafUuid":"x"}`,
		assistantLine(day.Add(-3*time.Hour), "m0", "claude-opus-5", 1000, 1000, 0, 0),
		assistantLine(day.Add(-1*time.Hour), "m1", "claude-opus-5", 1000, 1000, 0, 0),
		assistantLine(day.Add(1*time.Hour+30*time.Minute), "m2", "claude-opus-5", 10, 20, 300, 400),
		assistantLine(day.Add(1*time.Hour+31*time.Minute), "m2", "claude-opus-5", 10, 25, 300, 400),
		`{"type":"user","timestamp":"`+day.Add(2*time.Hour).UTC().Format(time.RFC3339Nano)+`","message":{"role":"user","content":[{"type":"tool_result","content":"grep usage: {\"usage\":1}"}]}}`,
		`this line is not json`,
		assistantLine(day.Add(13*time.Hour), "m3", "claude-mystery", 1, 2, 3, 4),
	)
	// A second project, and a subagent log nested under it, both today.
	writeTranscript(t, filepath.Join(dir, "-Users-me-other", "s2.jsonl"), now,
		assistantLine(day.Add(13*time.Hour+59*time.Minute), "m4", "claude-opus-5", 100, 0, 0, 0),
	)
	writeTranscript(t, filepath.Join(dir, "-Users-me-other", "s2", "subagents", "agent-1.jsonl"), now,
		assistantLine(day.Add(13*time.Hour+5*time.Minute), "m5", "claude-opus-5", 0, 50, 0, 0),
	)
	// Modified two days ago: not today's file, even though the stamp says so.
	writeTranscript(t, filepath.Join(dir, "-Users-me-stale", "s3.jsonl"), day.Add(-40*time.Hour),
		assistantLine(day.Add(5*time.Hour), "m6", "claude-opus-5", 999999, 0, 0, 0),
	)
	return dir, now
}

func TestHourlyBucketsTodayByLocalHourAndCountsEachMessageOnce(t *testing.T) {
	dir, now := hourlyFixture(t)
	got := Hourly(dir, now, nil)

	if got.Date != "2026-09-12" || len(got.Buckets) != 24 {
		t.Fatalf("date=%s buckets=%d", got.Date, len(got.Buckets))
	}
	// m2 once, at its final usage; m3, m4 and the subagent's m5 in hour 13.
	want := map[int]int64{1: 735, 13: 10 + 100 + 50}
	for h, b := range got.Buckets {
		if b.Hour != h {
			t.Errorf("bucket %d labelled %d", h, b.Hour)
		}
		if b.Tokens != want[h] {
			t.Errorf("hour %d: %d tokens, want %d", h, b.Tokens, want[h])
		}
		if b.AgentUSD != nil {
			t.Errorf("hour %d priced without a rate table: %v", h, *b.AgentUSD)
		}
	}
	if got.Source != "tokens only, no rate table in amac" {
		t.Errorf("source %q", got.Source)
	}
	if got.Warning != "" {
		t.Errorf("unexpected warning %q", got.Warning)
	}
}

func TestHourlyPricesOnlyModelsItHasARateFor(t *testing.T) {
	dir, now := hourlyFixture(t)
	rates := map[string]Rate{"claude-opus-5": {Input: 5, Output: 25, CacheRead: 0.5, CacheWrite: 6.25}}
	got := Hourly(dir, now, rates)

	// m2 once, at its final usage: 10 in, 25 out, 300 cache read, 400 write.
	wantOne := (10*5 + 25*25 + 300*0.5 + 400*6.25) / 1e6
	if b := got.Buckets[1]; b.AgentUSD == nil || *b.AgentUSD != wantOne {
		t.Errorf("hour 1: %v, want %v", b.AgentUSD, wantOne)
	}
	// Hour 13 mixes a priced model with an unpriced one: the priced part is
	// reported and the gap is named, rather than a silently short number.
	wantTwo := (100*5 + 50*25) / 1e6
	if b := got.Buckets[13]; b.AgentUSD == nil || *b.AgentUSD != wantTwo {
		t.Errorf("hour 13: %v, want %v", b.AgentUSD, wantTwo)
	}
	if got.Buckets[5].AgentUSD != nil {
		t.Errorf("an empty hour must stay null")
	}
	if !strings.Contains(got.Warning, "claude-mystery") {
		t.Errorf("warning must name the unpriced model: %q", got.Warning)
	}
	if !strings.Contains(got.Source, "rate table") || got.Source == "tokens only, no rate table in amac" {
		t.Errorf("source %q", got.Source)
	}
}

func TestHourlyWithNoTranscriptsSaysSo(t *testing.T) {
	got := Hourly(t.TempDir(), time.Now(), nil)
	if len(got.Buckets) != 24 || !strings.Contains(got.Warning, "no Claude Code transcripts") {
		t.Errorf("buckets=%d warning=%q", len(got.Buckets), got.Warning)
	}
	b, _ := json.Marshal(got.Buckets[0])
	if string(b) != `{"hour":0,"agentUsd":null,"tokens":0}` {
		t.Errorf("wire shape: %s", b)
	}
}

// The seek is what keeps a 300MB week-long session cheap. It must land at a
// line boundary no later than today's first line, step over unstamped lines,
// and cope with a file that is entirely before or entirely inside the day.
func TestTailFromLandsOnOrBeforeTodaysFirstLine(t *testing.T) {
	day := time.Date(2026, 9, 12, 0, 0, 0, 0, time.Local)
	var sb strings.Builder
	sb.WriteString(`{"type":"summary","summary":"no stamp here"}` + "\n")
	for i := 0; i < 500; i++ {
		sb.WriteString(assistantLine(day.Add(-time.Duration(500-i)*time.Minute), fmt.Sprintf("old%d", i), "claude-opus-5", 1, 1, 1, 1) + "\n")
	}
	firstToday := sb.Len()
	for i := 0; i < 7; i++ {
		sb.WriteString(assistantLine(day.Add(time.Duration(i)*time.Hour), fmt.Sprintf("new%d", i), "claude-opus-5", 1, 1, 1, 1) + "\n")
	}
	data := sb.String()
	f := strings.NewReader(data)

	got := tailFrom(f, int64(len(data)), day)
	if got > int64(firstToday) {
		t.Fatalf("seek landed at %d, past today's first line at %d", got, firstToday)
	}
	if got != 0 && data[got-1] != '\n' {
		t.Fatalf("seek landed mid-line at %d", got)
	}
	// Close enough that the old lines are not re-read: within a line or two.
	if int64(firstToday)-got > 400 {
		t.Errorf("seek stopped %d bytes early", int64(firstToday)-got)
	}

	// Past the end of the file the seek may stop one line short of EOF; that
	// line is read and rejected by the caller, and nothing before it is.
	lastLine := strings.LastIndex(strings.TrimSuffix(data, "\n"), "\n") + 1
	if got := tailFrom(f, int64(len(data)), day.AddDate(0, 0, 1)); got < int64(lastLine) {
		t.Errorf("a day after the file ends should seek into the last line (%d), got %d", lastLine, got)
	}
	if got := tailFrom(f, int64(len(data)), day.AddDate(0, 0, -30)); got != 0 {
		t.Errorf("a day before the file starts should seek to 0, got %d", got)
	}
}
