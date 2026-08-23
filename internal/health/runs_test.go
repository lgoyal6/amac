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
		if got := failureCount(tc.note); got != tc.want {
			t.Errorf("failureCount(%q) = %d, want %d", tc.note, got, tc.want)
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
	for _, want := range []string{"job-discovery", "crashed", "31s"} {
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
	runs := []Run{
		{Automation: "morning-brief", Status: RunOK, Detail: "delivered the brief", Duration: 44 * time.Second},
		{Automation: "hacklist-sf", Status: RunSkipped, Detail: "gate: swept recently enough", Duration: 8 * time.Second},
	}
	got := RunBatch(runs)
	if !strings.HasPrefix(got, "**Runs** · 2") {
		t.Errorf("should lead with the count:\n%s", got)
	}
	for _, want := range []string{"morning-brief", "delivered", "hacklist-sf", "gate:", "44s", "8s"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// A single run should not be announced as a plural list.
	if one := RunBatch(runs[:1]); !strings.HasPrefix(one, "**Run**\n") {
		t.Errorf("single run header wrong:\n%s", one)
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
