package health

import (
	"strings"
	"testing"
	"time"
)

var runNow = time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

func run(name string, status RunStatus, agoHours int) Run {
	return Run{
		Automation: name, ID: name + itoa(agoHours), Status: status,
		Started: runNow.Add(-time.Duration(agoHours) * time.Hour), Detail: "detail",
	}
}

func find(logs []RunLog, name string) RunLog {
	for _, l := range logs {
		if l.Automation == name {
			return l
		}
	}
	return RunLog{}
}

// The distinction this whole screen exists for. The sweep reads the newest
// piece of evidence, so an automation that broke and was rescued by its next
// run is green there and has to be something else here.
func TestRecoveredIsNotClean(t *testing.T) {
	logs := SummarizeRuns([]Run{
		run("job-discovery", RunOK, 1),
		run("job-discovery", RunFailed, 5),
		run("job-discovery", RunOK, 9),
	}, []string{"job-discovery"}, runNow, 40)

	got := find(logs, "job-discovery")
	if got.Verdict != Recovered {
		t.Fatalf("verdict = %q, want recovered", got.Verdict)
	}
	if got.Failed != 1 || got.Worked != 2 {
		t.Errorf("counts = %d failed, %d worked; want 1 and 2", got.Failed, got.Worked)
	}
	if !strings.Contains(got.Headline, "5h ago") {
		t.Errorf("headline should date the failure, got %q", got.Headline)
	}
}

// The newest run failing is the case that needs someone, and it must outrank
// a window that merely contains failures.
func TestNewestFailureIsBrokenAndSortsFirst(t *testing.T) {
	logs := SummarizeRuns([]Run{
		run("hacklist", RunFailed, 1),
		run("hacklist", RunOK, 4),
		run("job-discovery", RunFailed, 6),
		run("job-discovery", RunOK, 2),
		run("disk-sweep", RunOK, 3),
	}, []string{"hacklist", "job-discovery", "disk-sweep"}, runNow, 40)

	if logs[0].Automation != "hacklist" || logs[0].Verdict != Broken {
		t.Fatalf("worst first: got %s/%s", logs[0].Automation, logs[0].Verdict)
	}
	if logs[1].Verdict != Recovered {
		t.Errorf("recovered should follow broken, got %s", logs[1].Verdict)
	}
	if logs[2].Verdict != Clean {
		t.Errorf("clean should come last of the three, got %s", logs[2].Verdict)
	}
}

// "Nothing ran" and "nobody is counting" look identical on screen and mean
// opposite things: one is possibly a dead pipeline, the other is a gap in
// amac itself.
func TestQuietIsNotUnwatched(t *testing.T) {
	logs := SummarizeRuns(nil, []string{"disk-sweep", "devspend"}, runNow, 40)

	if v := find(logs, "disk-sweep").Verdict; v != Quiet {
		t.Errorf("a watched automation with no runs is quiet, got %q", v)
	}
	if v := find(logs, "devspend").Verdict; v != Unwatched {
		t.Errorf("devspend has no run source, so it is unwatched, got %q", v)
	}
	if h := find(logs, "disk-sweep").Headline; strings.Contains(strings.ToLower(h), "late") {
		t.Errorf("an empty window must not claim lateness, got %q", h)
	}
}

// Unwatched rows are here for honesty, not urgency. Six of them above the
// healthy automations is a phone screen spent saying nothing happened.
func TestUnwatchedSortsBelowHealthy(t *testing.T) {
	logs := SummarizeRuns([]Run{run("disk-sweep", RunOK, 2)},
		[]string{"devspend", "disk-sweep"}, runNow, 40)

	if logs[0].Automation != "disk-sweep" {
		t.Errorf("healthy automation should lead, got %s", logs[0].Automation)
	}
	if logs[len(logs)-1].Verdict != Unwatched {
		t.Errorf("unwatched should sort last, got %s", logs[len(logs)-1].Verdict)
	}
}

// hacklist was hacklist-sf until the board stopped being SF-only. Its history
// spans the rename, and two rows would show one automation apparently dying on
// the day it was renamed.
func TestRenameFoldsIntoOneHistory(t *testing.T) {
	logs := SummarizeRuns([]Run{
		run("hacklist", RunOK, 1),
		run("hacklist-sf", RunOK, 30),
		run("hacklist-sf", RunFailed, 40),
	}, []string{"hacklist"}, runNow, 40)

	if len(logs) != 1 {
		t.Fatalf("want one folded history, got %d rows", len(logs))
	}
	if logs[0].Total != 3 || logs[0].Automation != "hacklist" {
		t.Errorf("got %s with %d runs, want hacklist with 3", logs[0].Automation, logs[0].Total)
	}
}

// A retired automation still inside the window must not silently shorten the
// week it is being read for.
func TestUndeclaredHistoryStillAppears(t *testing.T) {
	logs := SummarizeRuns([]Run{run("retired-job", RunOK, 3)}, []string{"disk-sweep"}, runNow, 40)
	if find(logs, "retired-job").Total != 1 {
		t.Error("runs recorded for an undeclared automation should still be shown")
	}
}

// The reaper runs 48 times a day. The cap is the only thing bounding what
// crosses the wire, and the runs kept must be the newest ones.
func TestRunsAreCappedNewestFirst(t *testing.T) {
	var runs []Run
	for h := 1; h <= 10; h++ {
		runs = append(runs, run("tmux-idle-reaper", RunOK, h))
	}
	got := find(SummarizeRuns(runs, nil, runNow, 3), "tmux-idle-reaper")

	if got.Total != 10 {
		t.Errorf("counts cover the whole window, got %d", got.Total)
	}
	if len(got.Runs) != 3 {
		t.Fatalf("kept %d runs, want 3", len(got.Runs))
	}
	if !got.Runs[0].At.After(got.Runs[1].At) {
		t.Error("kept runs should be newest first")
	}
	if got.Runs[0].At != runNow.Add(-time.Hour) {
		t.Error("the cap must keep the newest runs, not the oldest")
	}
}

// The reaper's marker gained swapram and disk24h, and an exact-match regex
// that stops matching fails silently: it falls through to the raw log line on
// both this screen and the Discord digest.
func TestReaperDetailSurvivesNewMarkerFields(t *testing.T) {
	for _, tc := range []struct{ detail, want string }{
		{"done (0 reaped, swap 96%, swapram 102%, disk 87%, disk24h +0%)", "nothing closed · swap 96% · disk 87%"},
		{"done (1 reaped, swap 40%, disk 50%)", "1 session closed · swap 40% · disk 50%"},
		{"done (3 reaped)", "3 sessions closed"},
	} {
		got := runDetail(Run{Automation: "tmux-idle-reaper", Detail: tc.detail})
		if got != tc.want {
			t.Errorf("runDetail(%q)\n got %q\nwant %q", tc.detail, got, tc.want)
		}
	}
}

// Every run carries the link to its own transcript, which is what keeps this a
// summary rather than a log viewer.
func TestEntriesKeepTheirURL(t *testing.T) {
	r := run("hacklist", RunOK, 1)
	r.URL, r.Duration = "https://github.com/lgoyal6/hacklist/actions/runs/1", 90*time.Second
	got := find(SummarizeRuns([]Run{r}, nil, runNow, 40), "hacklist")
	if got.Runs[0].URL != r.URL {
		t.Error("the run's own URL should survive summarizing")
	}
	if got.Runs[0].Duration != "1m" {
		t.Errorf("duration = %q, want 1m", got.Runs[0].Duration)
	}
}
