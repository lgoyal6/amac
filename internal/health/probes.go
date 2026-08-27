package health

// The seven shapes, each taking its target from the roster.
//
// Four of the ten automations on this machine are a launchd job with a
// completion marker in a log, and they shared an implementation before the
// roster made that visible. The rest are one each, and the ones that stayed
// separate did so because how a pipeline reports itself is not a detail that
// generalises: a marker carrying a date and no time, timestamps encoded in
// filenames, and an n8n API are three different facts about three different
// systems. Flattening them would describe the world less accurately than
// naming them does.

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ------------------------------------------------------- launchd + marker ---

// counts describes a number the job writes into its own completion marker.
//
// Two jobs do this and mean opposite things by it. brew-autoupgrade counts its
// partial failures, where non-zero is a problem worth reporting even though the
// run exited clean. tmux-idle-reaper counts sessions it killed, where non-zero
// is the job working and zero is the ordinary case. One concept, two readings,
// so the roster states which.
type counts struct {
	re        *regexp.Regexp
	noun      string
	zero      string
	isFailure bool
}

func newLaunchdMarker(d Declaration) (func(context.Context) (Report, error), error) {
	p := paramsOf(d)
	label := p.str("label", true)
	logPath := p.path("log", true)

	var c *counts
	if raw, ok := d.With["counts"]; ok {
		m, ok := raw.(map[string]any)
		if !ok {
			p.errs = append(p.errs, "counts must be an object")
		} else {
			cp := &params{with: m}
			pattern := cp.str("pattern", true)
			re, err := regexp.Compile(pattern)
			if err != nil {
				cp.errs = append(cp.errs, "counts.pattern: "+err.Error())
			}
			c = &counts{
				re: re, noun: cp.str("noun", true),
				zero: cp.str("zero", false),
			}
			if v, ok := m["failure_when_nonzero"].(bool); ok {
				c.isFailure = v
			}
			if err := cp.err(); err != nil {
				p.errs = append(p.errs, err.Error())
			}
		}
	}
	if err := p.err(); err != nil {
		return nil, err
	}

	return func(ctx context.Context) (Report, error) {
		r, note, err := localJob(ctx, label, logPath)
		if err != nil || r.State != OK || c == nil {
			return r, err
		}
		n := 0
		if m := c.re.FindStringSubmatch(note); m != nil && len(m) > 1 {
			n, _ = strconv.Atoi(m[1])
		}
		switch {
		case n > 0 && c.isFailure:
			r.State = Failing
			r.Detail = fmt.Sprintf("%d %s %s ago, see %s", n, c.noun, short(time.Since(r.Last)), logPath)
		case n > 0:
			r.Detail = fmt.Sprintf("%d %s %s ago", n, c.noun, short(time.Since(r.Last)))
		case c.zero != "":
			r.Detail = "last swept " + short(time.Since(r.Last)) + " ago, " + c.zero
		}
		return r, nil
	}, nil
}

// ------------------------------------------------------------------ service --

func newService(d Declaration) (func(context.Context) (Report, error), error) {
	p := paramsOf(d)
	label := p.str("label", true)
	port := strconv.Itoa(int(p.num("port", 0)))
	if port == "0" {
		p.errs = append(p.errs, "missing port")
	}
	if err := p.err(); err != nil {
		return nil, err
	}
	return func(ctx context.Context) (Report, error) {
		return serviceOnTailnet(ctx, label, port)
	}, nil
}

// ------------------------------------------------------------ marker fields --

// newMarkerFields reads named numbers out of another job's completion marker
// and reports whether each is inside its limit.
//
// This is how pressure detection rides on the reaper's thirty-minute tick
// rather than growing a schedule of its own, and why it is a separate line: the
// reaper being healthy and the machine being inside its limits are different
// questions with different fixes.
func newMarkerFields(d Declaration) (func(context.Context) (Report, error), error) {
	p := paramsOf(d)
	logPath := p.path("log", true)

	type field struct {
		name  string
		re    *regexp.Regexp
		limit int
		note  string
	}
	var fields []field
	raw, ok := d.With["fields"]
	if !ok {
		p.errs = append(p.errs, "missing fields")
	} else if list, ok := raw.([]any); !ok {
		p.errs = append(p.errs, "fields must be a list")
	} else {
		for _, item := range list {
			m, ok := item.(map[string]any)
			if !ok {
				p.errs = append(p.errs, "each field must be an object")
				continue
			}
			fp := &params{with: m}
			re, err := regexp.Compile(fp.str("pattern", true))
			if err != nil {
				p.errs = append(p.errs, "field pattern: "+err.Error())
				continue
			}
			fields = append(fields, field{
				name: fp.str("name", true), re: re,
				limit: int(fp.num("limit", 100)), note: fp.str("note", false),
			})
			if err := fp.err(); err != nil {
				p.errs = append(p.errs, err.Error())
			}
		}
	}
	if err := p.err(); err != nil {
		return nil, err
	}

	return func(ctx context.Context) (Report, error) {
		r := Report{State: OK}
		ms := allMarkers(logPath)
		if len(ms) == 0 {
			r.State = Unknown
			r.Detail = "no reading yet in " + logPath
			return r, nil
		}
		last := ms[len(ms)-1]
		r.Last = last.at

		var over, all []string
		for _, f := range fields {
			m := f.re.FindStringSubmatch(last.note)
			if m == nil || len(m) < 2 {
				continue
			}
			v, _ := strconv.Atoi(m[1])
			all = append(all, fmt.Sprintf("%s %d%%", f.name, v))
			if v >= f.limit {
				over = append(over, fmt.Sprintf("%s %d%%", f.name, v))
				if f.note != "" {
					r.Notes = append(r.Notes, f.note)
				}
			}
		}
		if len(all) == 0 {
			r.State = Unknown
			r.Detail = "the last run recorded none of the declared fields"
			r.Notes = nil
			return r, nil
		}
		if len(over) > 0 {
			r.State = Failing
			r.Detail = strings.Join(over, " and ") + ", read " + short(time.Since(last.at)) + " ago"
			return r, nil
		}
		r.Detail = strings.Join(all, ", ") + " (read " + short(time.Since(last.at)) + " ago)"
		return r, nil
	}, nil
}

// -------------------------------------------------------------- spend snapshot

// newSpendSnapshot reads looseapi's snapshot, which is written only after its
// mail scan, provider poll and usage read have all completed. That makes
// generatedAt a real delivery marker rather than an mtime, and it is why this
// probe knows the shape of another project's file: finding the artifact was
// cheaper than adding one.
func newSpendSnapshot(d Declaration) (func(context.Context) (Report, error), error) {
	p := paramsOf(d)
	label := p.str("label", true)
	if err := p.err(); err != nil {
		return nil, err
	}
	return func(ctx context.Context) (Report, error) {
		return spendSnapshot(ctx, label)
	}, nil
}

// ------------------------------------------------------------------- github ---

func newGitHubDeliveryFile(d Declaration) (func(context.Context) (Report, error), error) {
	p := paramsOf(d)
	repo := p.str("repo", true)
	path := p.str("path", true)
	field := p.str("date_field", true)
	// The marker carries a date and no time, so lateness has to be measured
	// from when delivery was due rather than from midnight.
	anchor := int(p.num("anchor_hour_utc", 0))
	if err := p.err(); err != nil {
		return nil, err
	}
	return func(ctx context.Context) (Report, error) {
		return githubDeliveryFile(ctx, repo, path, field, anchor)
	}, nil
}

func newGitHubNewestFile(d Declaration) (func(context.Context) (Report, error), error) {
	p := paramsOf(d)
	repo := p.str("repo", true)
	dir := p.str("dir", true)
	prefix := p.str("prefix", true)
	suffix := p.str("suffix", true)
	noun := p.str("noun", false)
	if noun == "" {
		noun = "sweep"
	}
	issueLabel := p.str("open_issue_label", false)
	if err := p.err(); err != nil {
		return nil, err
	}
	return func(ctx context.Context) (Report, error) {
		return githubNewestFile(ctx, repo, dir, prefix, suffix, noun, issueLabel)
	}, nil
}

// --------------------------------------------------------------------- n8n ---

func newN8N(d Declaration) (func(context.Context) (Report, error), error) {
	p := paramsOf(d)
	host := p.str("host", true)
	workflow := p.str("workflow_id", true)
	if err := p.err(); err != nil {
		return nil, err
	}
	return func(ctx context.Context) (Report, error) {
		return n8nPipeline(ctx, host, workflow)
	}, nil
}
