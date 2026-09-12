package daemon

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lgoyal6/amac/internal/event"
	"github.com/lgoyal6/amac/internal/health"
)

// What happened while you were away, on one screen.
//
// Three things move on this machine without anyone watching: Claude sessions
// write transcripts, automations deliver or fail, and hackathon entries change
// state on the Worker. Each already has its own screen. This is the one read
// across all three, newest first, for a widget that answers "what changed since
// I last looked" without opening any of them.
//
// Nothing here decides anything. Sessions are read off the transcripts Claude
// Code already writes, automation runs are the same recorded events the run log
// reads, and the hackathon queue is the Worker's own answer. A source that
// cannot be read is named in warnings rather than left out, because a feed that
// silently drops a source reads exactly like a quiet day.

const (
	recentCap = 60
	// recentTail is how much of a transcript is read. Sessions run to hundreds
	// of megabytes and everything this needs is on the last few lines.
	recentTail = 64 << 10
	// recentDeadline is how far ahead a closing hackathon earns a line.
	recentDeadline = 48 * time.Hour
)

type recentItem struct {
	At      time.Time `json:"at"`
	Kind    string    `json:"kind"`
	Project string    `json:"project"`
	Title   string    `json:"title"`
	Detail  string    `json:"detail"`
	State   string    `json:"state,omitempty"`
}

func (s *Server) recent(w http.ResponseWriter, r *http.Request) {
	hours := 24
	if n, err := strconv.Atoi(r.URL.Query().Get("hours")); err == nil && n > 0 && n <= 24*30 {
		hours = n
	}
	now := time.Now()
	since := now.Add(-time.Duration(hours) * time.Hour)
	items := []recentItem{}
	warnings := []string{}

	got, warns := recentSessions(homePath(".claude/projects"), since)
	items = append(items, got...)
	warnings = append(warnings, warns...)

	runs, err := s.runsSince(r.Context(), since)
	if err != nil {
		warnings = append(warnings, "automation runs: "+err.Error())
	}
	reports, err := s.latestSweep(r.Context())
	if err != nil {
		warnings = append(warnings, "automation sweep: "+err.Error())
	}
	items = append(items, recentAutomations(runs, reports, health.All(s.log), since, now)...)

	// Shorter than the board's own timeout. A widget that waits thirty
	// seconds on the Worker is a widget that looks broken.
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if body, err := fetchQueue(ctx); err != nil {
		warnings = append(warnings, "hackqueue: "+err.Error())
	} else if got, err := recentHacks(body, since, now); err != nil {
		warnings = append(warnings, "hackqueue: "+err.Error())
	} else {
		items = append(items, got...)
	}

	writeJSON(w, 200, map[string]any{"since": since.UTC(), "items": newestFirst(items, recentCap), "warnings": warnings})
}

// newestFirst puts the merged feed on one clock and cuts it to size. Each
// source stamps in its own zone, launchd runs in local time and transcripts
// and the Worker in UTC, and a feed that mixes offsets reads out of order to
// a person even when it is not.
func newestFirst(items []recentItem, keep int) []recentItem {
	for i := range items {
		items[i].At = items[i].At.UTC()
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].At.After(items[j].At) })
	if len(items) > keep {
		items = items[:keep]
	}
	return items
}

// ---------------------------------------------------------------- sessions --

// transcriptLine is the handful of fields this reads off a Claude Code
// transcript line. Every line is a JSON object; most carry a timestamp and a
// cwd, and assistant lines name the model that answered.
type transcriptLine struct {
	Timestamp string `json:"timestamp"`
	Cwd       string `json:"cwd"`
	Message   struct {
		Model string `json:"model"`
	} `json:"message"`
}

// recentSessions lists every transcript under root touched since the window
// opened, one item each. root is ~/.claude/projects: one directory per working
// directory, one .jsonl per session inside it. Subagent transcripts nest
// deeper and belong to the session above them, so only the top level is read.
func recentSessions(root string, since time.Time) ([]recentItem, []string) {
	dirs, err := os.ReadDir(root)
	if err != nil {
		return nil, []string{"claude transcripts: " + err.Error()}
	}
	var items []recentItem
	var warnings []string
	for _, dir := range dirs {
		if !dir.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(root, dir.Name()))
		if err != nil {
			warnings = append(warnings, "claude transcripts: "+err.Error())
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			info, err := f.Info()
			if err != nil || info.ModTime().Before(since) {
				continue
			}
			tail, err := readTail(filepath.Join(root, dir.Name(), f.Name()), recentTail)
			if err != nil {
				warnings = append(warnings, "claude transcripts: "+err.Error())
				continue
			}
			if item := sessionItem(tail, dir.Name(), info.ModTime()); !item.At.Before(since) {
				items = append(items, item)
			}
		}
	}
	return items, warnings
}

// readTail returns the last n bytes of a file as whole lines: when the read
// starts mid-file, the partial first line is dropped.
func readTail(path string, n int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	start := max(info.Size()-n, 0)
	buf := make([]byte, info.Size()-start)
	if _, err := io.ReadFull(io.NewSectionReader(f, start, int64(len(buf))), buf); err != nil {
		return nil, err
	}
	if start > 0 {
		i := bytes.IndexByte(buf, '\n')
		buf = buf[i+1:] // i is -1 when no line completes, which empties the tail
	}
	return buf, nil
}

// sessionItem reads one session off the tail of its transcript. The newest
// timestamp is when it was last active, the cwd names the project, and the
// model is the last one that answered. Anything the tail does not say falls
// back to the file: its mtime for the time, its directory for the project.
func sessionItem(tail []byte, slug string, mtime time.Time) recentItem {
	var at time.Time
	var cwd, model string
	lines := bytes.Split(tail, []byte("\n"))
	for i := len(lines) - 1; i >= 0 && (at.IsZero() || cwd == "" || model == ""); i-- {
		var line transcriptLine
		if len(lines[i]) == 0 || json.Unmarshal(lines[i], &line) != nil {
			continue
		}
		if at.IsZero() {
			if t, err := time.Parse(time.RFC3339, line.Timestamp); err == nil {
				at = t
			}
		}
		if cwd == "" {
			cwd = line.Cwd
		}
		if model == "" {
			model = line.Message.Model
		}
	}
	if at.IsZero() {
		at = mtime
	}
	project := filepath.Base(cwd)
	if cwd == "" {
		project = slugProject(slug)
	}
	if model == "" {
		model = "claude"
	}
	return recentItem{At: at, Kind: "session", Project: project, Title: "session in " + project, Detail: model}
}

// slugProject recovers a project name from a transcript directory, which is
// the working directory with every separator turned into a dash.
func slugProject(slug string) string {
	slug = strings.TrimPrefix(slug, "-")
	if i := strings.LastIndex(slug, "-"); i >= 0 {
		return slug[i+1:]
	}
	return slug
}

// ------------------------------------------------------------- automations --

// runsSince reads every recorded run in the window. It is the read healthRuns
// makes, and deliberately only a read: the run log already decided what each
// run was, and a second opinion here would be the one that disagrees with the
// Discord digest.
func (s *Server) runsSince(ctx context.Context, since time.Time) ([]health.Run, error) {
	rows, err := s.log.DB().QueryContext(ctx,
		`SELECT payload FROM events WHERE kind = ? AND at >= ? ORDER BY seq DESC`,
		string(event.KindAutomationRun), since.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var runs []health.Run
	for rows.Next() {
		var payload []byte
		if rows.Scan(&payload) != nil {
			continue
		}
		var run health.Run
		if json.Unmarshal(payload, &run) == nil && run.Automation != "" && !run.Started.Before(since) {
			runs = append(runs, run)
		}
	}
	return runs, rows.Err()
}

// latestSweep reads the newest sweep's reports, the way health does. No sweep
// on record is not an error; it means the monitor has not run yet.
func (s *Server) latestSweep(ctx context.Context) ([]health.Report, error) {
	var payload []byte
	err := s.log.DB().QueryRowContext(ctx,
		`SELECT payload FROM events WHERE kind = ? ORDER BY seq DESC LIMIT 1`,
		string(event.KindAutomationCheck)).Scan(&payload)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var body struct {
		Reports []health.Report `json:"reports"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		return nil, err
	}
	return body.Reports, nil
}

// recentAutomations turns the window's runs into items, and falls back to the
// sweep for automations that have no per-run reporting.
//
// Runs of one automation with the same outcome fold into one line. The two
// reapers and agents-sync each run every half hour and each run counts as a
// delivery, so read one per line the last day is a hundred and thirty
// identical green rows and nothing else fits on the screen. A failure still
// breaks the fold, so "delivered ×20, failed, delivered ×25" keeps its shape.
// Skipped runs are not deliveries and do not appear; that is what skipped means.
func recentAutomations(runs []health.Run, reports []health.Report, roster []health.Automation, since, now time.Time) []recentItem {
	// The project is where someone would go to fix it, which the roster
	// declares as Home. An automation with nothing local to open is filed
	// under its own name.
	project := map[string]string{}
	var declared []string
	for _, a := range roster {
		if a.Category == "machine" {
			continue
		}
		declared = append(declared, a.Name)
		if a.Home != "" {
			project[health.Canonical(a.Name)] = filepath.Base(a.Home)
		}
	}
	filedUnder := func(name string) string {
		if p, ok := project[name]; ok {
			return p
		}
		return name
	}

	var items []recentItem
	covered := map[string]bool{}
	for _, log := range health.SummarizeRuns(runs, declared, now, len(runs)) {
		streak, count := -1, 0
		for _, run := range log.Runs {
			if run.Status == health.RunSkipped || run.At.Before(since) {
				continue
			}
			detail, state := "delivered", "ok"
			if run.Status == health.RunFailed {
				detail, state = "failed", "failing"
			}
			if streak >= 0 && items[streak].State == state {
				count++
				items[streak].Detail = fmt.Sprintf("%s ×%d", detail, count)
				continue
			}
			items = append(items, recentItem{At: run.At, Kind: "automation", Project: filedUnder(log.Automation), Title: log.Automation, Detail: detail, State: state})
			streak, count = len(items)-1, 1
			covered[log.Automation] = true
		}
	}

	// The sweep's Last is the last real delivery, so an automation the run log
	// does not count still gets its line. One that the run log did count is
	// left alone, or the same delivery would appear twice under two dates.
	for _, rep := range reports {
		name := health.Canonical(rep.Name)
		if covered[name] || rep.Category == "machine" || rep.Last.IsZero() || rep.Last.Before(since) {
			continue
		}
		detail, state := string(rep.State), "unknown"
		switch rep.State {
		case health.OK:
			detail, state = "delivered", "ok"
		case health.Failing:
			detail, state = "failed", "failing"
		}
		items = append(items, recentItem{At: rep.Last, Kind: "automation", Project: filedUnder(name), Title: name, Detail: detail, State: state})
	}
	return items
}

// ------------------------------------------------------------------- hacks --

// fetchQueue reads the Worker's queue with the bearer from the file hackqueue
// already keeps it in.
func fetchQueue(ctx context.Context) ([]byte, error) {
	base, secret, err := hackqueueEnv()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/queue", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("authorization", "Bearer "+secret)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("queue answered %d", res.StatusCode)
	}
	return io.ReadAll(io.LimitReader(res.Body, 8<<20))
}

// hackWords says each queue status the way the board does.
var hackWords = map[string]string{
	"pending":                "offered, waiting for a direction",
	"research_requested":     "research requested",
	"approved":               "direction chosen, ideas being researched",
	"denied":                 "passed",
	"selecting":              "choosing an idea",
	"idea_pending":           "ideas ready, waiting for a pick",
	"idea_changes_requested": "idea changes requested",
	"build_approved":         "build approved",
	"building":               "building",
	"ready":                  "built and waiting to submit",
	"rejected":               "rejected",
	"failed":                 "build failed",
	"done":                   "submitted",
	"expired":                "expired undecided",
}

// hackClosed is every status a deadline no longer matters to.
var hackClosed = map[string]bool{"denied": true, "rejected": true, "done": true, "expired": true}

func hackWord(status string) string {
	if w, ok := hackWords[status]; ok {
		return w
	}
	return strings.ReplaceAll(status, "_", " ")
}

// recentHacks reads the Worker's queue: one item for every entry that changed
// in the window, and one for every open entry closing inside the next two
// days. A deadline is dated at the deadline, so it sorts above everything that
// already happened, which is where the one thing you can still act on belongs.
func recentHacks(body []byte, since, now time.Time) ([]recentItem, error) {
	var queue map[string]json.RawMessage
	if err := json.Unmarshal(body, &queue); err != nil {
		return nil, err
	}
	var items []recentItem
	for status, raw := range queue {
		// Alongside one key per status the queue carries `seen`, a list of ids
		// rather than entries. It, and anything else that is not a list of
		// entries, is skipped rather than failed on.
		if status == "seen" {
			continue
		}
		var entries []struct {
			ID        string `json:"id"`
			Title     string `json:"title"`
			Status    string `json:"status"`
			UpdatedAt string `json:"updatedAt"`
			DecidedAt string `json:"decidedAt"`
			Closes    string `json:"closes"`
		}
		if json.Unmarshal(raw, &entries) != nil {
			continue
		}
		for _, e := range entries {
			if e.ID == "" {
				continue
			}
			if e.Status == "" {
				e.Status = status
			}
			title := e.Title
			if title == "" {
				title = e.ID
			}
			if at := firstTime(e.UpdatedAt, e.DecidedAt); !at.IsZero() && !at.Before(since) {
				items = append(items, recentItem{At: at, Kind: "hack", Project: "hackqueue", Title: title, Detail: hackWord(e.Status)})
			}
			closes, err := time.Parse(time.RFC3339, e.Closes)
			if err != nil || hackClosed[e.Status] || !closes.After(now) || !closes.Before(now.Add(recentDeadline)) {
				continue
			}
			detail := "closes in under an hour"
			if h := int(math.Round(closes.Sub(now).Hours())); h >= 1 {
				detail = fmt.Sprintf("closes in %dh", h)
			}
			items = append(items, recentItem{At: closes, Kind: "hack", Project: "hackqueue", Title: title, Detail: detail})
		}
	}
	return items, nil
}

// firstTime parses the first candidate that is a time.
func firstTime(candidates ...string) time.Time {
	for _, c := range candidates {
		if t, err := time.Parse(time.RFC3339, c); err == nil {
			return t
		}
	}
	return time.Time{}
}
