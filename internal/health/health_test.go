package health

import (
	"context"
	"os"
	"path/filepath"
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
		// Just inside Every+Grace. An exact 18h case is untestable: Sweep measures
		// time.Since, which is always a few nanoseconds past what we set up.
		{"just inside the limit", 18*time.Hour - time.Minute, OK},
		{"gone quiet", 19 * time.Hour, Late},
		{"long dead", 30 * 24 * time.Hour, Late},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			last := time.Now().Add(-tc.age)
			got := Sweep(context.Background(), []Automation{{
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
	got := Sweep(context.Background(), []Automation{{
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
	got := Sweep(context.Background(), []Automation{{
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

func TestSweepCarriesCategoryIntoLiveReport(t *testing.T) {
	got := Sweep(context.Background(), []Automation{{
		Name: "pressure", Category: "machine",
		Check: func(context.Context) (Report, error) {
			return Report{State: Failing, Detail: "disk 90%"}, nil
		},
	}})
	if got[0].Category != "machine" {
		t.Fatalf("category = %q, want machine", got[0].Category)
	}
	msg := Digest(got)
	if strings.Contains(msg, "Automations · 1 of 1 need attention") || !strings.Contains(msg, "Machine status") {
		t.Fatalf("machine pressure was presented as an automation failure: %q", msg)
	}
}

func TestRunSortsWorstFirst(t *testing.T) {
	mk := func(name string, s State) Automation {
		return Automation{Name: name, Every: time.Hour, Grace: time.Hour,
			Check: func(context.Context) (Report, error) { return Report{State: s, Last: time.Now()}, nil }}
	}
	got := Sweep(context.Background(), []Automation{
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

func TestStampedName(t *testing.T) {
	ts, ok := stampedName("sweep-2026-08-23T00-44-21-214Z.json", "sweep-", ".json")
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
		if _, ok := stampedName(bad, "sweep-", ".json"); ok {
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
func TestReadDelivery(t *testing.T) {
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
	d := readDelivery(path)
	want := time.Date(2026, 8, 22, 16, 10, 38, 0, time.Local)
	if !d.found || !d.at.Equal(want) {
		t.Fatalf("got %s (found=%v), want %s", d.at, d.found, want)
	}
	if !contains(d.note, "0 failures") {
		t.Fatalf("note %q lost the failure count", d.note)
	}

	// local-passes uses a different tail and no closing ===.
	write("=== 2026-08-21 21:39:23 local passes done\n")
	d = readDelivery(path)
	if !d.found || d.at.Hour() != 21 || d.at.Minute() != 39 {
		t.Fatalf("local-passes format: %s (found=%v)", d.at, d.found)
	}

	// A log with output but no completed run reports no delivery, which the
	// probe turns into Unknown. Reporting the file's mtime here would claim a
	// delivery that never happened.
	write("started\nstill going\n")
	if readDelivery(path).found {
		t.Fatal("a log with no marker must not yield a delivery")
	}
	if readDelivery(dir + "/missing.log").found {
		t.Fatal("a missing log must not yield a delivery")
	}
}

// Only the last 64KB is read, so a marker buried behind megabytes of output
// still has to be found.
func TestReadDeliveryReadsOnlyTheTail(t *testing.T) {
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
	d := readDelivery(path)
	ts, note := d.at, d.note
	if !d.found {
		t.Fatal("no delivery found")
	}
	if ts.Year() != 2026 {
		t.Fatalf("read the wrong marker: %s", ts)
	}
	if contains(note, "9 failures") {
		t.Fatal("returned the stale marker from outside the window")
	}
}

// The digest is read on a phone, where Discord's column is about forty
// characters. Nothing here can enforce how Discord wraps, but it can keep the
// lines we control short enough to survive it, and it can keep a URL from
// being buried inside a sentence.
func TestDigestFitsAPhoneScreen(t *testing.T) {
	reports := []Report{
		{Name: "hacklist-sf", State: Failing, Last: time.Now().Add(-2 * time.Hour),
			Detail: "last run failure (SF discovery, 51m ago) https://github.com/lgoyal6/hacklist-sf/actions/runs/32608022200",
			Notes:  []string{`open pipeline-red issue #13, 15d old`}},
		{Name: "morning-brief", State: OK, Detail: "delivered 2026-08-22"},
		{Name: "brew-autoupgrade", State: OK, Detail: "last completed 2h ago"},
	}
	got := Digest(reports)

	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "http") {
			continue // a URL is as long as it is
		}
		// Bold markers render to nothing, so they do not count against width.
		if w := len(strings.ReplaceAll(line, "**", "")); w > 56 {
			t.Errorf("line too wide for a phone (%d chars): %q", w, line)
		}
	}
	if strings.Contains(got, "    ") {
		t.Error("digest indents; indentation wastes a phone's width")
	}
	// The URL must be on its own line, not inside the failure sentence.
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "http") && !strings.HasPrefix(line, "<") {
			t.Errorf("URL not lifted onto its own line: %q", line)
		}
	}
}

func TestDigestAllHealthyLeadsWithReassurance(t *testing.T) {
	got := Digest([]Report{
		{Name: "a", State: OK, Detail: "delivered"},
		{Name: "b", State: OK, Detail: "delivered"},
	})
	if !strings.HasPrefix(got, "✅ **Automations** · all 2 delivering") {
		t.Errorf("all-healthy digest should open with the all-clear, got:\n%s", got)
	}
	if strings.Contains(got, "Healthy") {
		t.Error("a Healthy subheading is redundant when nothing is broken")
	}
}

func TestSplitURLKeepsTrailingProse(t *testing.T) {
	text, link := splitURL("broke https://example.com/run/1 during the sweep")
	if text != "broke during the sweep" {
		t.Errorf("text = %q", text)
	}
	if link != "<https://example.com/run/1>" {
		t.Errorf("link = %q", link)
	}
	if text, link := splitURL("no link here"); text != "no link here" || link != "" {
		t.Errorf("plain detail mangled: %q %q", text, link)
	}
}

// Pressure is read off the reaper's marker, and the two conditions it can
// report have different fixes. Saying "over limit" without saying which would
// send you to delete caches when the number that moved was memory.
func TestMachinePressure(t *testing.T) {
	for _, tc := range []struct {
		marker    string
		wantState State
		wantNote  bool
	}{
		{"done (0 reaped, swap 12%, disk 40%)", OK, false},
		{"done (0 reaped, swap 92%, disk 91%)", Failing, true},
		// Disk alone is the case the cache job actually fixes, so it must not
		// carry the note telling him caches will not help.
		{"done (2 reaped, swap 10%, disk 91%)", Failing, false},
		{"done (0 reaped, swap 92%, disk 40%)", Failing, true},
		// Written before the reaper recorded these numbers: nothing to report,
		// which is not the same as nothing wrong.
		{"done (0 reaped)", Unknown, false},
	} {
		log := filepath.Join(t.TempDir(), "reaper.log")
		line := "=== 2026-08-26 19:42:14 " + tc.marker + " ===\n"
		if err := os.WriteFile(log, []byte(line), 0o644); err != nil {
			t.Fatal(err)
		}

		check, err := newMarkerFields(pressureDecl(log))
		if err != nil {
			t.Fatal(err)
		}
		r, err := check(context.Background())
		if err != nil {
			t.Fatalf("%s: %v", tc.marker, err)
		}
		if r.State != tc.wantState {
			t.Errorf("%s: state = %s, want %s", tc.marker, r.State, tc.wantState)
		}
		if got := len(r.Notes) > 0; got != tc.wantNote {
			t.Errorf("%s: swap note present = %v, want %v (%v)", tc.marker, got, tc.wantNote, r.Notes)
		}
	}
}

// No reading at all is unknown, never ok. The reaper being gone is exactly when
// a green line would be worst.
func TestMachinePressureWithNoReadingIsUnknown(t *testing.T) {
	check, err := newMarkerFields(pressureDecl(filepath.Join(t.TempDir(), "absent.log")))
	if err != nil {
		t.Fatal(err)
	}
	r, err := check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r.State != Unknown {
		t.Fatalf("state = %s, want unknown", r.State)
	}
}

// These logs carry a start marker and a completion marker, and the difference
// between them is the difference between "it ran" and "it delivered". Reading
// only the newest reported a run in flight as having completed at the moment it
// began, and reported a run that died as a delivery.
func TestReadDeliveryTellsStartingFromFinishing(t *testing.T) {
	write := func(lines ...string) string {
		p := filepath.Join(t.TempDir(), "job.log")
		if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	// A clean history: the newest done is the delivery, nothing outstanding.
	d := readDelivery(write(
		"=== 2026-08-24 20:30:00 local passes starting",
		"=== 2026-08-24 20:33:00 local passes done",
		"=== 2026-08-25 20:30:00 local passes starting",
		"=== 2026-08-25 20:34:00 local passes done",
	))
	if !d.found || d.at.Day() != 25 {
		t.Errorf("newest completion = %v, want the 25th", d.at)
	}
	if !d.unfinished.IsZero() {
		t.Errorf("nothing should be outstanding, got %v", d.unfinished)
	}

	// A run that started and never finished. The delivery is still the earlier
	// one, and the start is outstanding: the lateness test upstream must keep
	// measuring deliveries, not attempts.
	d = readDelivery(write(
		"=== 2026-08-25 20:30:00 local passes starting",
		"=== 2026-08-25 20:34:00 local passes done",
		"=== 2026-08-26 20:30:04 local passes starting",
	))
	if !d.found || d.at.Day() != 25 {
		t.Errorf("delivery = %v, want the 25th, not the unfinished run", d.at)
	}
	if d.unfinished.IsZero() {
		t.Error("the unfinished run must be visible")
	}

	// Nothing has ever completed. Unknown, never ok.
	if d := readDelivery(write("=== 2026-08-26 20:30:04 local passes starting")); d.found {
		t.Error("a log with no completion must not report a delivery")
	}
}

// pressureDecl is the roster entry this machine actually uses, pointed at a
// temporary log. Building the probe the way Load does means these tests cover
// the declaration as well as the reading.
func pressureDecl(log string) Declaration {
	return Declaration{
		Name: "machine-pressure", Probe: "marker_fields",
		With: map[string]any{
			"log": log,
			"fields": []any{
				map[string]any{
					"name": "swap", "pattern": `swap (\d+)%`, "limit": float64(80),
					"note": "swap is memory, not disk: clearing caches will not move it, closing sessions will",
				},
				map[string]any{"name": "disk", "pattern": `disk (\d+)%`, "limit": float64(85)},
			},
		},
	}
}
