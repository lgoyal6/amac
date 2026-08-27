package health

// The Linux half of "a job on this machine".
//
// launchd and systemd answer the same question and answer it differently.
// launchd tells you a job's last exit status and nothing about when it
// happened, which is why the launchd probe reads a completion marker out of a
// log to find the time. systemd records the timestamp itself, in
// ExecMainExitTimestamp, along with the exit code and its own verdict in
// Result. That is strictly more, so this probe needs no log at all and takes
// one only when a job distinguishes running from delivering.
//
// Nothing here is behind a build tag. systemctl is a shell-out, so it compiles
// on any platform and simply is not there on a Mac, and a probe that reports
// honestly when its tool is missing is better than a binary that cannot be
// built for a machine someone wanted to try it on.

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// systemdShow reads the properties that describe a unit's last run.
func systemdShow(ctx context.Context, unit, scope string) (map[string]string, error) {
	bin, err := exec.LookPath("systemctl")
	if err != nil {
		return nil, fmt.Errorf("systemctl is not on this machine, so a systemd unit cannot be read here")
	}
	args := []string{"show", unit,
		"--property=ActiveState,SubState,Result,ExecMainStatus,ExecMainExitTimestamp,LoadState"}
	if scope != "system" {
		args = append([]string{"--user"}, args...)
	}
	out, err := exec.CommandContext(ctx, bin, args...).Output()
	if err != nil {
		return nil, fmt.Errorf("systemctl show %s: %w", unit, err)
	}

	props := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			props[k] = v
		}
	}
	return props, nil
}

// systemdTime parses the format systemd prints, "Tue 2026-08-27 09:30:01 UTC".
// An empty value means the unit has never run, which is not a parse failure and
// must not be reported as one.
func systemdTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" || s == "n/a" {
		return time.Time{}, false
	}
	for _, layout := range []string{
		"Mon 2006-01-02 15:04:05 MST",
		"Mon 2006-01-02 15:04:05 -0700",
		"2006-01-02 15:04:05 MST",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func newSystemdUnit(d Declaration) (func(context.Context) (Report, error), error) {
	p := paramsOf(d)
	unit := p.str("unit", true)
	scope := p.str("scope", false) // "user" by default, or "system"
	logPath := p.path("log", false)
	if err := p.err(); err != nil {
		return nil, err
	}
	return func(ctx context.Context) (Report, error) {
		return systemdUnit(ctx, unit, scope, logPath)
	}, nil
}

func systemdUnit(ctx context.Context, unit, scope, logPath string) (Report, error) {
	r := Report{State: OK}

	props, err := systemdShow(ctx, unit, scope)
	if err != nil {
		return r, err
	}
	if props["LoadState"] != "loaded" {
		r.State = Down
		r.Detail = unit + " is not loaded (" + props["LoadState"] + ")"
		return r, nil
	}

	last, ok := systemdTime(props["ExecMainExitTimestamp"])
	running := props["SubState"] == "running" || props["ActiveState"] == "activating"

	if !ok {
		if running {
			r.Detail = "running now, and has not completed a run yet"
			return r, nil
		}
		// Loaded and never run. Unknown, not ok: it may be fine and merely new,
		// and nothing here has proved otherwise.
		r.State = Unknown
		r.Detail = "loaded, but systemd has no record of a completed run"
		return r, nil
	}
	r.Last = last
	r.Detail = "last run " + short(time.Since(last)) + " ago"

	// systemd's own verdict first, then the exit code. Result carries cases the
	// code cannot: a unit killed by its timeout says "timeout" and exits zero.
	if result := props["Result"]; result != "" && result != "success" {
		r.State = Failing
		r.Detail = fmt.Sprintf("last run %s (%s ago), systemctl status %s",
			result, short(time.Since(last)), unit)
		return r, nil
	}
	if code, err := strconv.Atoi(props["ExecMainStatus"]); err == nil && code != 0 {
		r.State = Failing
		r.Detail = fmt.Sprintf("last run exited %d (%s ago), journalctl -u %s",
			code, short(time.Since(last)), unit)
		return r, nil
	}
	if running {
		r.Detail = "running since " + short(time.Since(last)) + " ago, last completed then"
	}

	// A log is optional here and means the same as it does for launchd: this
	// job fires more often than it delivers, so a green run says nothing.
	if logPath != "" {
		d := readDelivery(logPath)
		if !d.found {
			r.State = Unknown
			r.Detail = "the unit ran, but no completed delivery in " + logPath
			return r, nil
		}
		r.Last = d.at
		r.Detail = "last delivered " + short(time.Since(d.at)) + " ago"
	}
	return r, nil
}
