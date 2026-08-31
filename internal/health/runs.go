package health

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Run reporting answers a different question from the health sweep.
//
// The sweep asks "is this automation delivering?", which is a question about
// the newest state. That is the right question for waking someone up, and it
// is why the sweep looked at the most recent run only. It also means a failure
// that the next run recovered from was never mentioned: job-discovery crashed
// three times in twenty hours and the sweep reported it green the whole time,
// because each crash was followed by a success before anyone looked.
//
// This is the other half. Every individual run gets reported exactly once,
// whatever happened after it.
type RunStatus string

const (
	RunOK      RunStatus = "ok"
	RunSkipped RunStatus = "skipped" // it ran and correctly decided to do nothing
	RunFailed  RunStatus = "failed"
)

func (s RunStatus) Icon() string {
	switch s {
	case RunFailed:
		return "🔴"
	case RunSkipped:
		return "⚪"
	default:
		return "🟢"
	}
}

type Run struct {
	Automation string        `json:"automation"`
	ID         string        `json:"id"` // stable, and the dedupe key
	Status     RunStatus     `json:"status"`
	Started    time.Time     `json:"started"`
	Duration   time.Duration `json:"duration"`
	Detail     string        `json:"detail"`
	URL        string        `json:"url,omitempty"`
}

// NewRuns collects every run not already reported.
//
// Each source lists cheaply first and only fetches detail for runs that are
// actually new. That ordering matters: establishing whether a job-discovery
// run sent anything costs a 535KB download, and doing it for runs already
// reported would be most of them.
func NewRuns(ctx context.Context, seen map[string]bool) []Run {
	var out []Run
	for _, f := range []func(context.Context, map[string]bool) ([]Run, error){
		morningBriefRuns, hacklistRuns, jobDiscoveryRuns, launchdRuns,
	} {
		runs, err := f(ctx, seen)
		if err != nil {
			// One broken source must not hide the others. The failure is
			// visible in the sweep's own report.
			continue
		}
		out = append(out, runs...)
	}
	return out
}

// ------------------------------------------------------------ github ---

// ghRunsSince lists recent workflow runs that have finished and are unseen.
func ghRunsSince(ctx context.Context, repo, automation string, seen map[string]bool) ([]ghRun, error) {
	runs, err := lastRuns(ctx, repo, 15)
	if err != nil {
		return nil, err
	}
	var fresh []ghRun
	for _, r := range runs {
		if r.Status != "completed" {
			continue // still going; it is not a run yet
		}
		if seen[key(automation, fmt.Sprint(r.ID))] {
			continue
		}
		fresh = append(fresh, r)
	}
	return fresh, nil
}

func key(automation, id string) string { return automation + "/" + id }

func base(r ghRun, automation string, status RunStatus, detail string) Run {
	return Run{
		Automation: automation, ID: fmt.Sprint(r.ID), Status: status,
		Started: r.CreatedAt, Duration: r.UpdatedAt.Sub(r.CreatedAt),
		Detail: detail, URL: r.HTMLURL,
	}
}

// morningBriefRuns distinguishes the run that delivered from the three that
// found the day's slot already claimed.
//
// GitHub cannot tell them apart: every step reports success either way,
// because the skip happens inside the steps (the sender sees the marker, the
// commit finds nothing to commit) rather than as a skipped step. So the test
// is whether a delivery commit landed inside the run's own window, which is
// the same artifact the health sweep already trusts.
func morningBriefRuns(ctx context.Context, seen map[string]bool) ([]Run, error) {
	repo, name := withOf("morning-brief", "repo"), "morning-brief"
	fresh, err := ghRunsSince(ctx, repo, name, seen)
	if err != nil || len(fresh) == 0 {
		return nil, err
	}
	commits, err := deliveryCommits(ctx, repo)
	if err != nil {
		commits = nil // fall back to reporting the run without the verdict
	}

	out := make([]Run, 0, len(fresh))
	for _, r := range fresh {
		switch {
		case r.Conclusion != "success":
			out = append(out, base(r, name, RunFailed, "run "+r.Conclusion))
		case commits == nil:
			out = append(out, base(r, name, RunOK, "completed (delivery unverified)"))
		case deliveredIn(commits, r.CreatedAt, r.UpdatedAt):
			out = append(out, base(r, name, RunOK, "delivered the brief"))
		default:
			out = append(out, base(r, name, RunSkipped, "slot already claimed today"))
		}
	}
	return out, nil
}

func deliveryCommits(ctx context.Context, repo string) ([]time.Time, error) {
	var res []struct {
		Commit struct {
			Message   string `json:"message"`
			Committer struct {
				Date time.Time `json:"date"`
			} `json:"committer"`
		} `json:"commit"`
	}
	if err := gh(ctx, "repos/"+repo+"/commits?path=briefs/.delivery.json&per_page=15", &res); err != nil {
		return nil, err
	}
	out := make([]time.Time, 0, len(res))
	for _, c := range res {
		out = append(out, c.Commit.Committer.Date)
	}
	return out, nil
}

// deliveredIn allows a minute of slack on each side: the commit is timestamped
// by git inside the job, and the run's own window is recorded by GitHub.
func deliveredIn(commits []time.Time, start, end time.Time) bool {
	lo, hi := start.Add(-time.Minute), end.Add(time.Minute)
	for _, c := range commits {
		if c.After(lo) && c.Before(hi) {
			return true
		}
	}
	return false
}

// hacklistRuns reads the gate directly. Unlike morning-brief, this workflow
// suppresses by skipping a whole job, which GitHub reports as such.
func hacklistRuns(ctx context.Context, seen map[string]bool) ([]Run, error) {
	repo, name := withOf("hacklist-sf", "repo"), "hacklist-sf"
	fresh, err := ghRunsSince(ctx, repo, name, seen)
	if err != nil || len(fresh) == 0 {
		return nil, err
	}
	out := make([]Run, 0, len(fresh))
	for _, r := range fresh {
		if r.Conclusion != "success" {
			out = append(out, base(r, name, RunFailed, "run "+r.Conclusion))
			continue
		}
		var jobs struct {
			Jobs []struct {
				Name       string `json:"name"`
				Conclusion string `json:"conclusion"`
			} `json:"jobs"`
		}
		if err := gh(ctx, fmt.Sprintf("repos/%s/actions/runs/%d/jobs", repo, r.ID), &jobs); err != nil {
			out = append(out, base(r, name, RunOK, "completed (gate unread)"))
			continue
		}
		status, detail := RunOK, "swept"
		for _, j := range jobs.Jobs {
			if j.Name == "discover" && j.Conclusion == "skipped" {
				status, detail = RunSkipped, "gate: swept recently enough"
			}
		}
		out = append(out, base(r, name, status, detail))
	}
	return out, nil
}

// ------------------------------------------------------------ n8n ---

// jobDiscoveryRuns reports each execution, and whether it had anything to send.
//
// A run that finds no new roles still succeeds, so status alone cannot tell
// the two apart. The pipeline's own report carries shouldSend and the counts
// behind it, which is the honest signal and also the interesting one: "61,268
// scanned, 0 accepted" says more than "success".
func jobDiscoveryRuns(ctx context.Context, seen map[string]bool) ([]Run, error) {
	const name = "job-discovery"
	k := n8nKey()
	if k == "" {
		return nil, nil
	}
	list, err := n8nGet(ctx, k, fmt.Sprintf("/api/v1/executions?workflowId=%s&limit=15", withOf("job-discovery", "workflow_id")))
	if err != nil {
		return nil, err
	}
	var body struct {
		Data []struct {
			ID        json.Number `json:"id"`
			Status    string      `json:"status"`
			StartedAt time.Time   `json:"startedAt"`
			StoppedAt *time.Time  `json:"stoppedAt"`
			Finished  bool        `json:"finished"`
		} `json:"data"`
	}
	if err := json.Unmarshal(list, &body); err != nil {
		return nil, err
	}

	var out []Run
	for _, e := range body.Data {
		id := e.ID.String()
		if seen[key(name, id)] || (!e.Finished && e.StoppedAt == nil) {
			continue
		}
		end := e.StartedAt
		if e.StoppedAt != nil {
			end = *e.StoppedAt
		}
		r := Run{
			Automation: name, ID: id, Started: e.StartedAt,
			Duration: end.Sub(e.StartedAt),
			URL:      fmt.Sprintf("https://%s/execution/%s", withOf("job-discovery", "host"), id),
		}
		if e.Status != "success" {
			r.Status, r.Detail = RunFailed, "execution "+e.Status
			out = append(out, r)
			continue
		}
		r.Status, r.Detail = RunOK, "completed"
		// Only now, and only for a run we are about to report, pay for the
		// full execution payload.
		if sent, detail, err := digestSent(ctx, k, id); err == nil {
			if sent {
				r.Detail = detail
			} else {
				r.Status, r.Detail = RunSkipped, detail
			}
		}
		out = append(out, r)
	}
	return out, nil
}

func digestSent(ctx context.Context, key, id string) (bool, string, error) {
	raw, err := n8nGet(ctx, key, "/api/v1/executions/"+id+"?includeData=true")
	if err != nil {
		return false, "", err
	}
	var body struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return false, "", err
	}
	// n8n has shipped this field as both an object and a JSON string.
	inner := body.Data
	var asString string
	if json.Unmarshal(inner, &asString) == nil {
		inner = json.RawMessage(asString)
	}
	var d struct {
		ResultData struct {
			RunData map[string][]struct {
				Data struct {
					Main [][]struct {
						JSON struct {
							ShouldSend bool `json:"shouldSend"`
							Counts     struct {
								Raw      int `json:"raw"`
								Accepted int `json:"accepted"`
							} `json:"counts"`
						} `json:"json"`
					} `json:"main"`
				} `json:"data"`
			} `json:"runData"`
		} `json:"resultData"`
	}
	if err := json.Unmarshal(inner, &d); err != nil {
		return false, "", err
	}
	runs := d.ResultData.RunData["Parse Pipeline Report"]
	if len(runs) == 0 || len(runs[0].Data.Main) == 0 || len(runs[0].Data.Main[0]) == 0 {
		return false, "", fmt.Errorf("no pipeline report")
	}
	j := runs[0].Data.Main[0][0].JSON
	if j.ShouldSend {
		return true, fmt.Sprintf("sent a digest, %d accepted of %d scanned", j.Counts.Accepted, j.Counts.Raw), nil
	}
	return false, fmt.Sprintf("nothing to send, 0 accepted of %d scanned", j.Counts.Raw), nil
}

func n8nGet(ctx context.Context, key, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", "https://"+withOf("job-discovery", "host")+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-N8N-API-KEY", key)
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("n8n %s: http %d", path, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// ------------------------------------------------------------ launchd ---

// launchdRuns treats each completion marker in the log as one run.
func launchdRuns(ctx context.Context, seen map[string]bool) ([]Run, error) {
	home := os.Getenv("HOME")
	jobs := []struct{ name, log string }{
		{"hacklist-local-passes", home + "/luma-hackathon-calendar/logs/local-passes.log"},
		{"brew-autoupgrade", home + "/Library/Logs/brew-upgrade.log"},
		{"disk-sweep", home + "/Library/Logs/sweep.log"},
		{"tmux-idle-reaper", home + "/Library/Logs/tmux-idle-reaper.log"},
	}
	var out []Run
	for _, j := range jobs {
		for _, m := range allMarkers(j.log) {
			if !reportableRun(j.name, m.note) {
				continue
			}
			id := m.at.UTC().Format(time.RFC3339)
			if seen[key(j.name, id)] {
				continue
			}
			r := Run{Automation: j.name, ID: id, Started: m.at, Status: RunOK, Detail: "completed"}
			if n := markerCount(m.note); n > 0 {
				r.Status = RunFailed
				r.Detail = fmt.Sprintf("%d step(s) failed", n)
			}
			if note := strings.TrimSpace(m.note); j.name == "tmux-idle-reaper" || j.name == "disk-sweep" {
				r.Detail = note
			}
			out = append(out, r)
		}
	}
	return out, nil
}

// reportableRun decides whether a marker is a completed run. Reaper ticks are
// deliberately all reportable, including "0 reaped": this installation uses
// Discord as an activity journal and the operator explicitly wants to see each
// thirty-minute tick. The disk sweep also writes a start banner, which is not
// a completion and must still be filtered out.
func reportableRun(name, note string) bool {
	switch name {
	case "disk-sweep":
		return strings.Contains(note, "done")
	}
	return true
}
