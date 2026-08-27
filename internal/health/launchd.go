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

// tailMarker returns the newest completion marker in a log.
//
// It reads only the last 64KB. These logs are append-only and never rotated
// (brew's is already 131KB and growing), and the marker we want is always at
// the end, so reading the whole file would cost more every day for no gain.
func tailMarker(path string) (time.Time, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}, "", err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return time.Time{}, "", err
	}
	const window = 64 << 10
	off, size := int64(0), fi.Size()
	if size > window {
		off, size = size-window, window
	}
	buf := make([]byte, size)
	if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
		return time.Time{}, "", err
	}

	m := markerRe.FindAllStringSubmatch(string(buf), -1)
	if len(m) == 0 {
		return time.Time{}, "", fmt.Errorf("no completion marker in the last 64KB of %s", path)
	}
	last := m[len(m)-1]
	// The logs stamp local time with no zone, which is what the job saw.
	ts, err := time.ParseInLocation("2006-01-02 15:04:05", last[1], time.Local)
	if err != nil {
		return time.Time{}, "", err
	}
	return ts, strings.TrimSuffix(strings.TrimSpace(last[2]), "==="), nil
}

// localJob is the shared shape of both launchd probes: loaded, then last
// completed run, then whatever that run reported.
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

// LocalPasses checks com.hacklist.local-passes, the nightly job that writes
// the Luma passes the hosted sweep cannot.
func LocalPasses(ctx context.Context) (Report, error) {
	r, _, err := localJob(ctx,
		"com.hacklist.local-passes",
		os.Getenv("HOME")+"/luma-hackathon-calendar/logs/local-passes.log")
	return r, err
}

var failuresRe = regexp.MustCompile(`done \((\d+) failures?\)`)

// BrewUpgrade checks com.user.brew-upgrade, the daily unattended upgrade of
// Homebrew, npm globals and pipx.
//
// It has one signal the others lack: the script counts its own partial
// failures and prints them in the marker. A run where two casks failed and the
// rest succeeded is worth surfacing, and depending on the exit code alone
// would miss it if the script ever stops propagating that.
func BrewUpgrade(ctx context.Context) (Report, error) {
	r, note, err := localJob(ctx,
		"com.user.brew-upgrade",
		os.Getenv("HOME")+"/Library/Logs/brew-upgrade.log")
	if err != nil || r.State == Down || r.State == Unknown {
		return r, err
	}
	if m := failuresRe.FindStringSubmatch(note); m != nil {
		if n, _ := strconv.Atoi(m[1]); n > 0 {
			r.State = Failing
			r.Detail = fmt.Sprintf("%d step(s) failed %s ago, see ~/Library/Logs/brew-upgrade.log",
				n, short(time.Since(r.Last)))
		}
	}
	return r, nil
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

// failureCount reads brew-autoupgrade's own tally out of its marker.
func failureCount(note string) int {
	m := failuresRe.FindStringSubmatch(note)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

var reapedRe = regexp.MustCompile(`done \((\d+) reaped\)`)

// TmuxReaper checks com.laksh.tmux-idle-reaper, the job that keeps agent
// sessions from accumulating.
//
// This is the first automation here whose normal run does nothing, and that
// changes what the probe has to read. The others deliver an artifact; this one
// delivers the absence of a problem. Before it wrote a marker on every run,
// "no log" meant either "nothing needed reaping" or "the reaper stopped", and
// there was no way to tell which from outside. Since a prevention that has
// quietly stopped preventing is exactly the failure it exists to avoid, it now
// records every run and the count with it.
//
// The count is worth surfacing when it is non-zero. A reap is a session that
// was killed on this machine without anyone asking, which is a thing to know
// about even when it was correct.
func TmuxReaper(ctx context.Context) (Report, error) {
	r, note, err := localJob(ctx, "com.laksh.tmux-idle-reaper",
		os.Getenv("HOME")+"/Library/Logs/tmux-idle-reaper.log")
	if err != nil || r.State != OK {
		return r, err
	}
	if m := reapedRe.FindStringSubmatch(note); m != nil {
		if n, _ := strconv.Atoi(m[1]); n > 0 {
			r.Detail = fmt.Sprintf("%d session(s) reaped %s ago", n, short(time.Since(r.Last)))
			return r, nil
		}
	}
	r.Detail = "last swept " + short(time.Since(r.Last)) + " ago, nothing to reap"
	return r, nil
}

// DiskSweep checks com.user.sweep, the weekly cache and idle-session clean.
//
// Its log carries two kinds of marker: a banner written before any work starts
// and a completion line written after it. Reading the newest one blindly would
// report a run that died halfway as a delivery, which is the exact mistake the
// hosted probes avoid by reading committed artifacts instead of run history. So
// the newest marker has to actually say the run finished.
func DiskSweep(ctx context.Context) (Report, error) {
	r, note, err := localJob(ctx, "com.user.sweep",
		os.Getenv("HOME")+"/Library/Logs/sweep.log")
	if err != nil || r.State != OK {
		return r, err
	}
	// The started-and-never-finished case is handled in localJob now, for every
	// job rather than only this one: hacklist-local-passes writes the same pair
	// of markers and was reporting a run that died as a delivery.
	r.Detail = "last completed " + short(time.Since(r.Last)) + " ago: " + strings.TrimSpace(note)
	return r, nil
}

// DevSpend checks com.laksh.devspend, the daily spend scan.
//
// This was the weakest probe here and no longer is. It used to read the log's
// mtime, which cannot tell a run that finished from one that died after its
// first line of output. looseapi turns out to write its snapshot only after the
// mail scan, the provider poll and the usage read have all completed, so
// generatedAt inside it is a real delivery marker: a half-dead run leaves the
// previous snapshot in place rather than a partial one.
//
// That is the same rule the hosted probes follow, and finding the artifact was
// cheaper than adding one.
func DevSpend(ctx context.Context) (Report, error) {
	const label = "com.laksh.devspend"
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
		r.Detail = fmt.Sprintf("last run exited %d, see /tmp/devspend.log", exit)
		return r, nil
	}
	// looseapi's own findings ride along. They are its judgement, not a second
	// one made here: a credit balance falling to zero is the case no card
	// statement can see, which is the entire reason that repo exists.
	for _, a := range snap.Worst(2) {
		r.Notes = append(r.Notes, a.Message)
	}
	return r, nil
}

var pressureRe = regexp.MustCompile(`swap (\d+)%, disk (\d+)%`)

// Pressure thresholds. Deliberately the same numbers the weekly sweep already
// gates its own notification on: two definitions of "this machine is
// struggling" that can disagree is one more than is useful.
const (
	swapLimit = 80
	diskLimit = 85
)

// MachinePressure reports whether the machine is inside its own limits.
//
// It shares a source with TmuxReaper and answers a different question, which is
// why it is a separate line rather than a note on that one. "The reaper script
// is healthy" and "the machine is drowning" have different fixes, and a single
// red line covering both would send you to read a shell script when what is
// needed is closing something.
//
// The reading rides on the reaper's tick because that tick already exists and
// runs every thirty minutes. The numbers were previously computed only by the
// weekly cache job, which is the wrong clock for a condition that arrives over
// an afternoon: swap crossed 90% here on a Tuesday and the next machine that
// would have noticed was Sunday's.
//
// If the reaper stops, this goes Late rather than silent, which is correct.
// Pressure detection having stopped is itself worth knowing.
func MachinePressure(ctx context.Context) (Report, error) {
	r := Report{State: OK}

	ms := allMarkers(os.Getenv("HOME") + "/Library/Logs/tmux-idle-reaper.log")
	if len(ms) == 0 {
		r.State = Unknown
		r.Detail = "no reading yet: the 30-minute sweep has not written one"
		return r, nil
	}
	last := ms[len(ms)-1]
	m := pressureRe.FindStringSubmatch(last.note)
	if m == nil {
		// Markers written before the reaper carried these numbers. Not a
		// failure, just nothing to report yet.
		r.State = Unknown
		r.Detail = "the last sweep did not record swap or disk"
		return r, nil
	}
	swap, _ := strconv.Atoi(m[1])
	disk, _ := strconv.Atoi(m[2])
	r.Last = last.at
	r.Detail = fmt.Sprintf("swap %d%%, disk %d%% (read %s ago)", swap, disk, short(time.Since(last.at)))

	var over []string
	if swap >= swapLimit {
		over = append(over, fmt.Sprintf("swap %d%%", swap))
	}
	if disk >= diskLimit {
		over = append(over, fmt.Sprintf("disk %d%%", disk))
	}
	if len(over) > 0 {
		r.State = Failing
		r.Detail = strings.Join(over, " and ") + ", read " + short(time.Since(last.at)) + " ago"
		// Said explicitly because the obvious response to disk pressure is to
		// run the cache job, and the obvious response to swap pressure is not.
		// Caches are on disk. Nothing the sweep deletes is in memory.
		if swap >= swapLimit {
			r.Notes = append(r.Notes, "swap is memory, not disk: clearing caches will not move it, closing sessions will")
		}
	}
	return r, nil
}
