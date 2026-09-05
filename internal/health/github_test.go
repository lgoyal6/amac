package health

import (
	"strings"
	"testing"
	"time"
)

// failureNote decides whether a repo reads as broken, and it is the only piece
// of the GitHub probes that is judgement rather than transport. The judgement
// is that these pipelines retry, so a failure the next run recovered from is
// history and not an incident. Every case below is a way that could be got
// wrong in an obvious-looking direction.

func ghr(name, status, concl string, ago time.Duration) ghRun {
	return ghRun{
		Name: name, Status: status, Conclusion: concl,
		UpdatedAt: time.Now().Add(-ago),
		HTMLURL:   "https://github.com/lgoyal6/x/actions/runs/1",
	}
}

func TestFailureNoteReportsOnlyTheNewestCompletedRun(t *testing.T) {
	for _, tc := range []struct {
		name string
		runs []ghRun
		bad  bool
	}{
		{
			name: "nothing to say about a repo with no runs",
			runs: nil,
		},
		{
			name: "the newest run passed",
			runs: []ghRun{ghr("ci", "completed", "success", time.Hour)},
		},
		{
			// The whole point. These retry, and the recovery is the current
			// state of the world.
			name: "an older failure the next run recovered from is history",
			runs: []ghRun{
				ghr("ci", "completed", "success", time.Hour),
				ghr("ci", "completed", "failure", 5*time.Hour),
			},
		},
		{
			name: "the newest run failed",
			runs: []ghRun{ghr("ci", "completed", "failure", time.Hour)},
			bad:  true,
		},
		{
			// A run still going has not failed, so it is skipped rather than
			// treated as either outcome. The failure behind it is still the
			// last thing that finished, and is still the truth right now.
			name: "a retry in flight does not clear the failure behind it",
			runs: []ghRun{
				ghr("ci", "in_progress", "", time.Minute),
				ghr("ci", "completed", "failure", time.Hour),
			},
			bad: true,
		},
		{
			name: "nothing has finished yet",
			runs: []ghRun{ghr("ci", "in_progress", "", time.Minute)},
		},
		{
			// cancelled and timed_out are not success, and reporting only on
			// "failure" would call a cancelled pipeline healthy.
			name: "a cancelled run is not a passing run",
			runs: []ghRun{ghr("ci", "completed", "cancelled", time.Hour)},
			bad:  true,
		},
		{
			name: "a timed out run is not a passing run",
			runs: []ghRun{ghr("ci", "completed", "timed_out", time.Hour)},
			bad:  true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			note, bad := failureNote(tc.runs)
			if bad != tc.bad {
				t.Fatalf("bad = %v, want %v (note %q)", bad, tc.bad, note)
			}
			if !bad {
				if note != "" {
					t.Errorf("a healthy repo produced a note: %q", note)
				}
				return
			}
			// A note nobody can act on is a note nobody reads. It has to name
			// the outcome, the workflow, how stale it is and where to look.
			for _, want := range []string{"ci", "ago", "https://github.com/"} {
				if !strings.Contains(note, want) {
					t.Errorf("note %q does not carry %q", note, want)
				}
			}
		})
	}
}
