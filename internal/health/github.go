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

// ------------------------------------------------------------- delivery file --

// githubDeliveryFile reads a marker a workflow commits only once the work
// landed.
//
// morning-brief writes briefs/.delivery.json after Discord confirms the send.
// That file is the only artifact in the repo meaning "he actually received
// today's brief"; run green and PDF committed can both be true while delivery
// failed. The repo, the path and the field come from the roster, because this
// shape is not specific to one pipeline even though the interpretation is.
func githubDeliveryFile(ctx context.Context, repo, path, field string, anchorHour int) (Report, error) {
	r := Report{State: OK}

	raw, err := ghFile(ctx, repo, path)
	if err != nil {
		return r, err
	}
	var marker map[string]any
	if err := json.Unmarshal(raw, &marker); err != nil {
		return r, fmt.Errorf("delivery marker: %w", err)
	}
	v, ok := marker[field].(string)
	if !ok {
		return r, fmt.Errorf("delivery marker has no string %q", field)
	}

	// Either a full timestamp or a bare date. A bare date is anchored at the
	// hour delivery is due, so lateness is measured from then and not from
	// midnight, which would report every morning as late until lunchtime.
	ts, err := time.Parse(time.RFC3339, v)
	if err != nil {
		day, dayErr := time.Parse("2006-01-02", v)
		if dayErr != nil {
			return r, fmt.Errorf("delivery marker %s=%q: %w", field, v, err)
		}
		ts = day.Add(time.Duration(anchorHour) * time.Hour)
	}
	r.Last = ts
	r.Detail = "delivered " + v

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

// ------------------------------------------------------------- newest file ---

// githubNewestFile reads a directory where each real run writes one file, and
// takes the newest timestamp out of the filenames.
//
// hacklist fires four crons a day and a gate lets one through, so a
// suppressed run exits green in about fifteen seconds. Counting runs would
// report a healthy pipeline that had not swept in a week; counting the files a
// sweep leaves behind cannot.
func githubNewestFile(ctx context.Context, repo, dir, prefix, suffix, noun, issueLabel string) (Report, error) {
	r := Report{State: OK}

	var files []struct {
		Name string `json:"name"`
	}
	if err := gh(ctx, "repos/"+repo+"/contents/"+dir, &files); err != nil {
		return r, err
	}
	var newest time.Time
	for _, f := range files {
		if ts, ok := stampedName(f.Name, prefix, suffix); ok && ts.After(newest) {
			newest = ts
		}
	}
	if newest.IsZero() {
		return r, fmt.Errorf("no %s files in %s (%d entries)", noun, dir, len(files))
	}
	r.Last = newest
	r.Detail = "last " + noun + " " + short(time.Since(newest)) + " ago"

	runs, err := lastRuns(ctx, repo, 8)
	if err != nil {
		r.Notes = append(r.Notes, "could not read run history: "+err.Error())
	} else if note, bad := failureNote(runs); bad {
		r.State = Failing
		r.Detail = note
	}

	// A pipeline that files its own issue when it goes red and closes it on
	// recovery leaves an open issue as an incident nobody closed. The label
	// comes from the roster; empty means the pipeline has no such convention
	// and there is nothing to look for.
	if issueLabel != "" {
		var issues []struct {
			Number    int       `json:"number"`
			Title     string    `json:"title"`
			CreatedAt time.Time `json:"created_at"`
		}
		if err := gh(ctx, "repos/"+repo+"/issues?labels="+issueLabel+"&state=open", &issues); err == nil {
			for _, is := range issues {
				r.Notes = append(r.Notes, fmt.Sprintf("open %s issue #%d %q, %s old",
					issueLabel, is.Number, is.Title, short(time.Since(is.CreatedAt))))
			}
		}
	}
	return r, nil
}

// stampedName parses a filename whose middle is a timestamp, as in
// "sweep-2026-08-23T00-44-21-214Z.json". The filename uses dashes where a
// timestamp uses colons and a dot, because colons are not legal in paths on
// every filesystem the repo gets cloned to. Prefix and suffix come from the
// roster; the dashed shape does not, because it is what the writer produces.
func stampedName(name, prefix, suffix string) (time.Time, bool) {
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) {
		return time.Time{}, false
	}
	s := strings.TrimSuffix(strings.TrimPrefix(name, prefix), suffix)
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
