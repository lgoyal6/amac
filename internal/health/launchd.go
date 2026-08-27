package health

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/lgoyal6/amac/internal/spend"
)

// The two local automations both close a run by appending a marker line to
// their log:
//
//	=== 2026-08-22 16:10:38 done (0 failures) ===
//	=== 2026-08-21 21:39:23 local passes done
//
// That marker is the delivery signal, and it is strictly better than the
// file's mtime for the same reason the hosted probes read committed artifacts
// instead of run history: a job that dies halfway still writes to its log, so
// mtime says "it ran" when what happened was "it started". Only a completed
// run reaches the marker.
var markerRe = regexp.MustCompile(`(?m)^=== (\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}) (.*)$`)

// delivery is what a job's log says about it, once the difference between
// starting and finishing is taken seriously.
//
// These logs carry both: "local passes starting" and "local passes done". The
// newest marker was being read as the last completed run, which is wrong in the
// two states that matter. While a run is in flight it reported the job as
// having completed at the moment it began, and after a crash it reported the
// start of the run that died as a delivery. A job that dies halfway still
// writes to its log, which is the whole reason this package reads committed
// artifacts elsewhere rather than run history.
type delivery struct {
	at         time.Time // newest completed run
	note       string    // its marker text
	found      bool
	unfinished time.Time // a start with no completion after it, zero if none
}

func readDelivery(path string) delivery {
	var d delivery
	for _, m := range allMarkers(path) {
		if strings.Contains(m.note, "done") {
			d.at, d.note, d.found = m.at, m.note, true
			d.unfinished = time.Time{}
			continue
		}
		// A start marker. It only means something if nothing completes after
		// it, which the loop settles by clearing this on the next done.
		d.unfinished = m.at
	}
	return d
}

func localJob(ctx context.Context, label, logPath string) (Report, string, error) {
	r := Report{State: OK}

	loaded, exit, running, err := launchdStatus(ctx, label)
	if err != nil {
		return r, "", err
	}
	if !loaded {
		r.State = Down
		r.Detail = label + " is not loaded in launchd"
		return r, "", nil
	}

	d := readDelivery(logPath)
	if !d.found {
		// Loaded but never completed a run we can see. Unknown, not OK: the
		// job may be fine and merely new, but we have not proved it.
		r.State = Unknown
		r.Detail = "loaded, but no completed run in " + logPath
		return r, "", nil
	}

	// Last stays the newest real delivery whatever else is going on, so the
	// lateness test upstream keeps measuring deliveries rather than attempts.
	r.Last = d.at
	r.Detail = "last completed " + short(time.Since(d.at)) + " ago"

	if !d.unfinished.IsZero() {
		if running {
			r.Detail = "running since " + short(time.Since(d.unfinished)) + " ago, last completed " +
				short(time.Since(d.at)) + " ago"
			return r, d.note, nil
		}
		// Started, not running, never finished. That is a death mid-run, and
		// it is invisible to anything that reads only the newest marker.
		r.State = Failing
		r.Detail = fmt.Sprintf("started %s ago and never finished, see %s",
			short(time.Since(d.unfinished)), logPath)
		return r, d.note, nil
	}

	if exit != 0 {
		r.State = Failing
		r.Detail = fmt.Sprintf("last run exited %d (%s ago), see %s", exit, short(time.Since(d.at)), logPath)
	}
	return r, d.note, nil
}

var lastExitRe = regexp.MustCompile(`"LastExitStatus"\s*=\s*(-?\d+)`)

// launchdStatus returns whether the job is loaded and its last exit status.
// `launchctl list <label>` prints a plist-ish block, not JSON, and exits
// non-zero when the label is unknown, which is the "not loaded" answer rather
// than an error.
func launchdStatus(ctx context.Context, label string) (loaded bool, exit int, running bool, err error) {
	bin, err := exec.LookPath("launchctl")
	if err != nil {
		return false, 0, false, err
	}
	out, err := exec.CommandContext(ctx, bin, "list", label).Output()
	if err != nil {
		return false, 0, false, nil
	}
	if m := lastExitRe.FindSubmatch(out); m != nil {
		exit, _ = strconv.Atoi(string(m[1]))
	}
	// launchd prints a PID key only while the job is actually executing, which
	// is how an in-flight run is told from one that died at the same point.
	return strings.Contains(string(out), label), exit, pidRe.Match(out), nil
}

var pidRe = regexp.MustCompile(`"PID"\s*=\s*\d+`)

// marker is one completed run recorded in a job's log.
type marker struct {
	at   time.Time
	note string
}

// allMarkers returns every completion marker in the tail of a log, oldest
// first. Reporting each run individually needs all of them, not just the last:
// two brew runs can land between two sweeps and the earlier one is exactly the
// kind of failure the state sweep was blind to.
func allMarkers(path string) []marker {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil
	}
	const window = 64 << 10
	off, size := int64(0), fi.Size()
	if size > window {
		off, size = size-window, window
	}
	buf := make([]byte, size)
	if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
		return nil
	}
	var out []marker
	for _, m := range markerRe.FindAllStringSubmatch(string(buf), -1) {
		ts, err := time.ParseInLocation("2006-01-02 15:04:05", m[1], time.Local)
		if err != nil {
			continue
		}
		out = append(out, marker{at: ts, note: strings.TrimSuffix(strings.TrimSpace(m[2]), "===")})
	}
	return out
}

// spendSnapshot reads looseapi's snapshot for a delivery time.
//
// It used to read the log's mtime, which cannot tell a finished run from one
// that died after its first line. The snapshot is written only after the mail
// scan, the provider poll and the usage read have all completed, so
// generatedAt is a real marker. looseapi's own findings ride along: a credit
// balance falling to zero is the case no card statement can see, which is the
// entire reason that project exists.
func spendSnapshot(ctx context.Context, label string) (Report, error) {
	r := Report{State: OK}

	loaded, exit, _, err := launchdStatus(ctx, label)
	if err != nil {
		return r, err
	}
	if !loaded {
		r.State = Down
		r.Detail = label + " is not loaded in launchd"
		return r, nil
	}

	snap, err := spend.Read()
	if err != nil {
		r.State = Unknown
		r.Detail = "loaded, but no snapshot at " + spend.Path()
		r.Err = err.Error()
		return r, nil
	}
	r.Last = snap.GeneratedAt
	r.Detail = fmt.Sprintf("last scan %s ago, tracking %s/mo",
		short(time.Since(r.Last)), spend.USD(int64(snap.MonthlyCents)))
	if exit != 0 {
		r.State = Failing
		r.Detail = fmt.Sprintf("last run exited %d", exit)
		return r, nil
	}
	for _, a := range snap.Worst(2) {
		r.Notes = append(r.Notes, a.Message)
	}
	return r, nil
}

var countRe = regexp.MustCompile(`\((\d+) (?:failures?|errors?)\)`)

// markerCount reads a failure tally a job wrote into its own completion marker.
//
// brew-autoupgrade counts its partial failures there, and a run where two casks
// failed and the rest succeeded is worth surfacing: depending on the exit code
// alone would miss it, because the script exits clean either way.
func markerCount(note string) int {
	m := countRe.FindStringSubmatch(note)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}
