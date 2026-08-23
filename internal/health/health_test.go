package health

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// The lateness test is the only reason this package can report a failure
// nobody pushed to it, so it gets the most coverage.
func TestRunFlagsSilence(t *testing.T) {
	cases := []struct {
		name string
		age  time.Duration
		want State
	}{
		{"fresh", time.Hour, OK},
		{"inside grace", 13 * time.Hour, OK}, // Every 12h + Grace 6h = 18h
		// Just inside Every+Grace. An exact 18h case is untestable: Run measures
		// time.Since, which is always a few nanoseconds past what we set up.
		{"just inside the limit", 18*time.Hour - time.Minute, OK},
		{"gone quiet", 19 * time.Hour, Late},
		{"long dead", 30 * 24 * time.Hour, Late},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			last := time.Now().Add(-tc.age)
			got := Run(context.Background(), []Automation{{
				Name: "x", Every: 12 * time.Hour, Grace: 6 * time.Hour,
				Check: func(context.Context) (Report, error) {
					return Report{State: OK, Last: last, Detail: "delivered"}, nil
				},
			}})
			if got[0].State != tc.want {
				t.Fatalf("age %s: got %s, want %s (%s)", tc.age, got[0].State, tc.want, got[0].Detail)
			}
		})
	}
}

// A probe with no idea when the automation last delivered must not be aged
// out into Late. Late means "it went quiet", and inventing that from a zero
// timestamp would page him for a gap in our own knowledge.
func TestUnknownLastIsNotLate(t *testing.T) {
	got := Run(context.Background(), []Automation{{
		Name: "x", Every: time.Hour, Grace: time.Minute,
		Check: func(context.Context) (Report, error) {
			return Report{State: OK, Detail: "no timestamp available"}, nil
		},
	}})
	if got[0].State != OK {
		t.Fatalf("got %s, want ok", got[0].State)
	}
}

// A broken probe is our bug, not his automation's. It must never read as OK
// (a false all-clear) and never as Failing (a false alarm).
func TestProbeErrorIsUnknown(t *testing.T) {
	got := Run(context.Background(), []Automation{{
		Name: "x", Every: time.Hour, Grace: time.Minute,
		Check: func(context.Context) (Report, error) {
			return Report{State: OK}, context.DeadlineExceeded
		},
	}})
	if got[0].State != Unknown {
		t.Fatalf("got %s, want unknown", got[0].State)
	}
	if got[0].Err == "" {
		t.Fatal("want the probe error preserved for the digest")
	}
}

func TestRunSortsWorstFirst(t *testing.T) {
	mk := func(name string, s State) Automation {
		return Automation{Name: name, Every: time.Hour, Grace: time.Hour,
			Check: func(context.Context) (Report, error) { return Report{State: s, Last: time.Now()}, nil }}
	}
	got := Run(context.Background(), []Automation{
		mk("fine", OK), mk("dunno", Unknown), mk("broken", Failing), mk("offline", Down),
	})
	want := []string{"broken", "offline", "dunno", "fine"}
	for i, w := range want {
		if got[i].Name != w {
			t.Fatalf("position %d: got %s, want %s", i, got[i].Name, w)
		}
	}
}

// Alerting on state rather than on presence is what keeps a persistent failure
// from re-DMing every fifteen minutes until he mutes the bot.
func TestAlertOnlyOnChange(t *testing.T) {
	broken := []Report{{Name: "a", State: Failing, Detail: "boom"}}
	healthy := []Report{{Name: "a", State: OK, Detail: "fine"}}

	if _, changed := Alert(broken, map[string]State{}); !changed {
		t.Fatal("a failure never reported before must alert")
	}
	if _, changed := Alert(broken, map[string]State{"a": Failing}); changed {
		t.Fatal("the same failure must stay quiet on the next sweep")
	}
	msg, changed := Alert(healthy, map[string]State{"a": Failing})
	if !changed {
		t.Fatal("recovery must alert")
	}
	if !contains(msg, "recovered") {
		t.Fatalf("recovery message should say so, got %q", msg)
	}
	if _, changed := Alert(healthy, map[string]State{"a": OK}); changed {
		t.Fatal("steady healthy must stay quiet")
	}
}

// A state transition between two different bad states is still news: Late
// (gone quiet) and Failing (ran and broke) call for different responses.
func TestAlertOnBadToBadTransition(t *testing.T) {
	now := []Report{{Name: "a", State: Failing, Detail: "boom"}}
	if _, changed := Alert(now, map[string]State{"a": Late}); !changed {
		t.Fatal("late -> failing is a change worth sending")
	}
}

func TestSweepTime(t *testing.T) {
	ts, ok := sweepTime("sweep-2026-08-23T00-44-21-214Z.json")
	if !ok {
		t.Fatal("failed to parse a real sweep filename")
	}
	want := time.Date(2026, 8, 23, 0, 44, 21, 214000000, time.UTC)
	if !ts.Equal(want) {
		t.Fatalf("got %s, want %s", ts, want)
	}
	for _, bad := range []string{
		"events.json",                        // the directory holds other files
		"sweep-not-a-time.json",              // must not panic on garbage
		"sweep-2026-08-23T00-44-21Z.json",    // missing millis field
		"sweep-2026-08-23T00-44-21-214Z.txt", // wrong extension
		"",
	} {
		if _, ok := sweepTime(bad); ok {
			t.Fatalf("%q parsed but should not have", bad)
		}
	}
}

func TestShort(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "30s"},
		{90 * time.Second, "1m"},
		{2 * time.Hour, "2h"},
		{47 * time.Hour, "47h"},
		{72 * time.Hour, "3d"},
	} {
		if got := short(tc.d); got != tc.want {
			t.Errorf("short(%s) = %s, want %s", tc.d, got, tc.want)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// The local jobs are judged by their completion marker rather than the log's
// mtime, because a job that dies halfway still writes to its log.
func TestTailMarker(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/job.log"

	write := func(s string) {
		if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Both real formats, and the newest must win even though an older marker
	// and a pile of un-marked output sit after it in the file.
	write(strings.Join([]string{
		"=== 2026-08-20 09:30:00 done (3 failures) ===",
		"noise",
		"=== 2026-08-22 16:10:38 done (0 failures) ===",
		"upgrading something",
		"a stack trace that never finished",
	}, "\n"))
	ts, note, err := tailMarker(path)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 8, 22, 16, 10, 38, 0, time.Local)
	if !ts.Equal(want) {
		t.Fatalf("got %s, want %s", ts, want)
	}
	if !contains(note, "0 failures") {
		t.Fatalf("note %q lost the failure count", note)
	}

	// local-passes uses a different tail and no closing ===.
	write("=== 2026-08-21 21:39:23 local passes done\n")
	if ts, _, err = tailMarker(path); err != nil {
		t.Fatalf("local-passes format: %v", err)
	}
	if ts.Hour() != 21 || ts.Minute() != 39 {
		t.Fatalf("got %s", ts)
	}

	// A log with output but no completed run must be an error, which the probe
	// turns into Unknown. Reporting the file's mtime here would claim a
	// delivery that never happened.
	write("started\nstill going\n")
	if _, _, err = tailMarker(path); err == nil {
		t.Fatal("a log with no marker must not yield a timestamp")
	}
	if _, _, err = tailMarker(dir + "/missing.log"); err == nil {
		t.Fatal("a missing log must error")
	}
}

// Only the last 64KB is read, so a marker buried behind megabytes of output
// still has to be found.
func TestTailMarkerReadsOnlyTheTail(t *testing.T) {
	path := t.TempDir() + "/big.log"
	var b strings.Builder
	b.WriteString("=== 2020-01-01 00:00:00 done (9 failures) ===\n") // far past the window
	for b.Len() < 200<<10 {
		b.WriteString("filler line that pushes the old marker out of the window\n")
	}
	b.WriteString("=== 2026-08-22 16:10:38 done (0 failures) ===\n")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	ts, note, err := tailMarker(path)
	if err != nil {
		t.Fatal(err)
	}
	if ts.Year() != 2026 {
		t.Fatalf("read the wrong marker: %s", ts)
	}
	if contains(note, "9 failures") {
		t.Fatal("returned the stale marker from outside the window")
	}
}
