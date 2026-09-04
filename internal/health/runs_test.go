package health

import (
	"strings"
	"testing"
	"time"
)

// morning-brief cannot be judged by GitHub's step conclusions: every step
// reports success whether the run delivered or found the slot already claimed,
// because the skip happens inside the steps. The delivery commit landing
// inside the run's own window is the only thing that separates them.
func TestDeliveredIn(t *testing.T) {
	start := time.Date(2026, 8, 22, 15, 0, 0, 0, time.UTC)
	end := start.Add(3 * time.Minute)

	if !deliveredIn([]time.Time{start.Add(2 * time.Minute)}, start, end) {
		t.Error("a commit inside the window is a delivery")
	}
	// A minute of slack each side: git stamps the commit inside the job while
	// GitHub stamps the window, and they disagree by seconds.
	if !deliveredIn([]time.Time{start.Add(-30 * time.Second)}, start, end) {
		t.Error("just before the window should still count")
	}
	if deliveredIn([]time.Time{start.Add(-time.Hour)}, start, end) {
		t.Error("yesterday's delivery must not mark this run as delivering")
	}
	if deliveredIn(nil, start, end) {
		t.Error("no commits means nothing was delivered")
	}
}

func TestFailureCount(t *testing.T) {
	for _, tc := range []struct {
		note string
		want int
	}{
		{"done (0 failures)", 0},
		{"done (3 failures)", 3},
		{"done (1 failure)", 1},
		{"local passes done", 0}, // the other job has no tally at all
		{"", 0},
	} {
		if got := markerCount(tc.note); got != tc.want {
			t.Errorf("markerCount(%q) = %d, want %d", tc.note, got, tc.want)
		}
	}
}

// A failure has to be legible on its own, because it is sent on its own.
func TestRunFailureIsSelfContained(t *testing.T) {
	r := Run{
		Automation: "job-discovery", Status: RunFailed, Detail: "execution crashed",
		Started: time.Now().Add(-time.Hour), Duration: 31 * time.Second,
		URL: "https://n8n.example/execution/376",
	}
	got := RunFailure(r)
	for _, want := range []string{"Job discovery", "crashed", "31s"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// Angle brackets stop Discord expanding a preview card under every alert.
	if !strings.Contains(got, "<https://n8n.example/execution/376>") {
		t.Errorf("url should be wrapped:\n%s", got)
	}
}

func TestRunBatch(t *testing.T) {
	started := time.Date(2026, 8, 30, 9, 15, 0, 0, time.Local)
	runs := []Run{
		{Automation: "morning-brief", Status: RunOK, Detail: "delivered the brief", Started: started, Duration: 44 * time.Second},
		{Automation: "hacklist", Status: RunSkipped, Detail: "gate: swept recently enough", Duration: 8 * time.Second},
	}
	got := runBatchAt(runs, started.Add(time.Hour))
	if !strings.HasPrefix(got, "**Automation activity**") {
		t.Errorf("should say what the message is:\n%s", got)
	}
	for _, want := range []string{"Morning brief", "09:15", "delivered", "Hacklist discovery", "gate:", "44s", "8s"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	old := started.AddDate(0, 0, -1)
	if got := runBatchAt([]Run{{Automation: "morning-brief", Started: old}}, started); !strings.Contains(got, "Aug 29 09:15") {
		t.Errorf("an older run must include its date so it does not look like a future time:\n%s", got)
	}
}

func TestReaperRunIsPlainAndPressureIsVisible(t *testing.T) {
	r := Run{Automation: "tmux-idle-reaper", Status: RunOK, Detail: "done (0 reaped, swap 95%, disk 94%)"}
	got := runBatchAt([]Run{r}, time.Now())
	for _, want := range []string{"⚠️", "Session cleanup", "nothing closed", "swap 95%", "disk 94%"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestLaunchdWatermarkUsesNewestSeenMarker(t *testing.T) {
	seen := map[string]bool{
		"tmux-idle-reaper/2026-08-29T18:33:00Z": true,
		"tmux-idle-reaper/2026-08-30T01:13:00Z": true,
		"job-discovery/376":                     true,
	}
	got, ok := launchdWatermark(seen, "tmux-idle-reaper")
	if !ok || got.UTC().Format(time.RFC3339) != "2026-08-30T01:13:00Z" {
		t.Fatalf("got %v, %v", got, ok)
	}
}

// Skipped is not a failure and must not read like one. These pipelines skip by
// design many times a day; colouring that red would train him to ignore red.
func TestSkippedIsNotAlarming(t *testing.T) {
	if RunSkipped.Icon() == RunFailed.Icon() {
		t.Fatal("skipped and failed must look different")
	}
	if RunOK.Icon() == RunFailed.Icon() {
		t.Fatal("ok and failed must look different")
	}
}

// Discord is the activity journal for this installation, so every completed
// reaper tick is visible even when there was nothing to reap.
func TestEveryCompletedReaperTickIsReported(t *testing.T) {
	for _, tc := range []struct {
		name, note string
		want       bool
	}{
		{"tmux-idle-reaper", "done (0 reaped)", true},
		{"tmux-idle-reaper", "done (1 reaped)", true},
		{"tmux-idle-reaper", "done (12 reaped)", true},
		// The sweep writes a banner before it does any work, so a run that
		// died halfway still leaves a marker. Only the completion line counts.
		{"disk-sweep", "sweep --auto", false},
		{"disk-sweep", "done (2 sessions, 1.4GB reclaimed)", true},
		// Everything else reports every completion marker it has.
		{"brew-autoupgrade", "done (0 failures)", true},
		{"hacklist-local-passes", "local passes done", true},
	} {
		if got := reportableRun(tc.name, tc.note); got != tc.want {
			t.Errorf("reportableRun(%q, %q) = %v, want %v", tc.name, tc.note, got, tc.want)
		}
	}
}

// A start banner is not a run. hacklist-local-passes writes "local passes
// starting" and "local passes done" on the same marker line shape, and while
// the done-check was a disk-sweep carve-out it counted both: fourteen runs in a
// week from a job that runs once a day.
func TestOnlyCompletionMarkersAreRuns(t *testing.T) {
	for _, tc := range []struct {
		name, note string
		want       bool
	}{
		{"hacklist-local-passes", "local passes starting", false},
		{"hacklist-local-passes", "local passes done", true},
		{"agents-sync", "sync starting ===", false},
		{"agents-sync", "done (0 failures, no changes) ===", true},
		{"devspend-refresh", "refresh starting ===", false},
		{"devspend-refresh", "done (1 failures, gmail auth expired) ===", true},
		{"hackqueue", "hackqueue starting", false},
		{"hackqueue", "hackqueue done", true},
		{"disk-sweep", "starting", false},
		{"tmux-idle-reaper", "done (0 reaped)", true},
	} {
		if got := reportableRun(tc.name, tc.note); got != tc.want {
			t.Errorf("reportableRun(%q, %q) = %v, want %v", tc.name, tc.note, got, tc.want)
		}
	}
}

// The roster and the run sources have to agree about who is watched, or the
// board reports "no per-run history" for something that has been reporting.
func TestNewlyWatchedJobsAreReported(t *testing.T) {
	for _, name := range []string{
		"hackqueue", "devspend-refresh", "docker-idle-reaper", "agents-sync",
		"hacklist-local-passes", "brew-autoupgrade", "disk-sweep", "tmux-idle-reaper",
		"morning-brief", "hacklist", "job-discovery",
	} {
		if !ReportsRuns(name) {
			t.Errorf("%s should have per-run reporting", name)
		}
	}
	// amac-daemon is a long-running service, not a series of runs. Claiming
	// per-run history for it would be inventing one.
	if ReportsRuns("amac-daemon") {
		t.Error("a daemon has no runs to report")
	}
}
