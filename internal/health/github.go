package health

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// gh shells out to the GitHub CLI rather than signing requests here.
//
// The token is already in his keyring with the right scopes, and gh renews it.
// Reimplementing that in Go would mean a second copy of his credentials on
// disk for no capability we don't already have. The cost is that gh must be on
// PATH, which is why the launchd plist sets one explicitly.
func gh(ctx context.Context, path string, v any) error {
	bin, err := exec.LookPath("gh")
	if err != nil {
		return fmt.Errorf("gh not on PATH: %w", err)
	}
	cmd := exec.CommandContext(ctx, bin, "api", "-H", "Accept: application/vnd.github+json", path)
	out, err := cmd.Output()
	if err != nil {
		var stderr string
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = strings.TrimSpace(string(ee.Stderr))
		}
		if stderr != "" {
			return fmt.Errorf("gh api %s: %s", path, stderr)
		}
		return fmt.Errorf("gh api %s: %w", path, err)
	}
	if v == nil {
		return nil
	}
	return json.Unmarshal(out, v)
}

// ghFile fetches one file's decoded contents from the default branch.
func ghFile(ctx context.Context, repo, path string) ([]byte, error) {
	var res struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if err := gh(ctx, "repos/"+repo+"/contents/"+path, &res); err != nil {
		return nil, err
	}
	if res.Encoding != "base64" {
		return nil, fmt.Errorf("%s: unexpected encoding %q", path, res.Encoding)
	}
	// The API wraps base64 at 60 columns; the decoder rejects the newlines.
	return base64.StdEncoding.DecodeString(strings.ReplaceAll(res.Content, "\n", ""))
}

type ghRun struct {
	Name       string    `json:"name"`
	Conclusion string    `json:"conclusion"`
	Status     string    `json:"status"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	HTMLURL    string    `json:"html_url"`
	ID         int64     `json:"id"`
}

// lastRuns returns recent workflow runs, newest first.
func lastRuns(ctx context.Context, repo string, n int) ([]ghRun, error) {
	var res struct {
		Runs []ghRun `json:"workflow_runs"`
	}
	err := gh(ctx, fmt.Sprintf("repos/%s/actions/runs?per_page=%d", repo, n), &res)
	return res.Runs, err
}

// failureNote summarises the newest failed run, if the newest run failed.
//
// It deliberately only looks at the newest, because these pipelines are
// designed to retry: a failure two runs ago that the next run recovered from
// is history, not an incident.
func failureNote(runs []ghRun) (string, bool) {
	for _, r := range runs {
		if r.Status != "completed" {
			continue // still running; it hasn't failed yet
		}
		if r.Conclusion == "success" {
			return "", false
		}
		return fmt.Sprintf("last run %s (%s, %s ago) %s",
			r.Conclusion, r.Name, short(time.Since(r.UpdatedAt)), r.HTMLURL), true
	}
	return "", false
}

// ---------------------------------------------------------- morning brief ---

// MorningBrief reads briefs/.delivery.json, which the workflow commits only
// after Discord confirms the send. That file is the only artifact in the repo
// that means "he actually received today's brief"; every other signal (run
// green, PDF committed) can be true while delivery failed.
func MorningBrief(ctx context.Context) (Report, error) {
	const repo = "lgoyal6/morning-brief"
	r := Report{State: OK}

	raw, err := ghFile(ctx, repo, "briefs/.delivery.json")
	if err != nil {
		return r, err
	}
	var marker struct {
		ScheduledDate string `json:"scheduled_date"`
	}
	if err := json.Unmarshal(raw, &marker); err != nil {
		return r, fmt.Errorf("delivery marker: %w", err)
	}
	day, err := time.Parse("2006-01-02", marker.ScheduledDate)
	if err != nil {
		return r, fmt.Errorf("delivery marker date %q: %w", marker.ScheduledDate, err)
	}
	// The marker carries a date, not a timestamp. Anchor it at 16:00 UTC,
	// roughly when the brief lands, so lateness is measured from when delivery
	// was due rather than from midnight.
	r.Last = day.Add(16 * time.Hour)
	r.Detail = "delivered " + marker.ScheduledDate

	runs, err := lastRuns(ctx, repo, 8)
	if err != nil {
		// Delivery is established; failing the whole probe over the run list
		// would throw away the answer we already have.
		r.Notes = append(r.Notes, "could not read run history: "+err.Error())
		return r, nil
	}
	if note, bad := failureNote(runs); bad {
		r.State = Failing
		r.Detail = note
	}
	return r, nil
}

// -------------------------------------------------------------- hacklist ----

// Hacklist reads data/history/, where each real sweep writes one
// sweep-<ISO>.json. The workflow fires four crons a day but its gate lets one
// through, so a suppressed run exits green in about 15 seconds. Counting runs
// would report a healthy pipeline that had not swept in a week; counting sweep
// files cannot.
func Hacklist(ctx context.Context) (Report, error) {
	const repo = "lgoyal6/hacklist-sf"
	r := Report{State: OK}

	var files []struct {
		Name string `json:"name"`
	}
	if err := gh(ctx, "repos/"+repo+"/contents/data/history", &files); err != nil {
		return r, err
	}
	var newest time.Time
	for _, f := range files {
		ts, ok := sweepTime(f.Name)
		if ok && ts.After(newest) {
			newest = ts
		}
	}
	if newest.IsZero() {
		return r, fmt.Errorf("no sweep files in data/history (%d entries)", len(files))
	}
	r.Last = newest
	r.Detail = "last sweep " + short(time.Since(newest)) + " ago"

	runs, err := lastRuns(ctx, repo, 8)
	if err != nil {
		r.Notes = append(r.Notes, "could not read run history: "+err.Error())
	} else if note, bad := failureNote(runs); bad {
		r.State = Failing
		r.Detail = note
	}

	// The pipeline files its own issue when it goes red and closes it on
	// recovery, so an issue still open is an incident nobody closed.
	var issues []struct {
		Number    int       `json:"number"`
		Title     string    `json:"title"`
		CreatedAt time.Time `json:"created_at"`
	}
	if err := gh(ctx, "repos/"+repo+"/issues?labels=pipeline-red&state=open", &issues); err == nil {
		for _, is := range issues {
			r.Notes = append(r.Notes, fmt.Sprintf("open pipeline-red issue #%d %q, %s old",
				is.Number, is.Title, short(time.Since(is.CreatedAt))))
		}
	}
	return r, nil
}

// sweepTime parses "sweep-2026-08-23T00-44-21-214Z.json". The filename uses
// dashes where a timestamp uses colons and a dot, because colons are not legal
// in paths on every filesystem the repo gets cloned to.
func sweepTime(name string) (time.Time, bool) {
	if !strings.HasPrefix(name, "sweep-") || !strings.HasSuffix(name, ".json") {
		return time.Time{}, false
	}
	s := strings.TrimSuffix(strings.TrimPrefix(name, "sweep-"), ".json")
	date, clock, ok := strings.Cut(s, "T")
	if !ok {
		return time.Time{}, false
	}
	parts := strings.Split(strings.TrimSuffix(clock, "Z"), "-")
	if len(parts) != 4 {
		return time.Time{}, false
	}
	ts, err := time.Parse(time.RFC3339Nano,
		date+"T"+parts[0]+":"+parts[1]+":"+parts[2]+"."+parts[3]+"Z")
	if err != nil {
		return time.Time{}, false
	}
	return ts, true
}
