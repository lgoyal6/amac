// Package observer watches what you are actually working on.
//
// Two tiers, and the split is the entire privacy design.
//
// Tier 1, implemented here, is metadata only: which app is frontmost, its
// window title, and how long you stayed. That is enough to learn a workflow
// ("terminal for 40 minutes, then Safari on a job board, then Notion") without
// ever reading a pixel, and it needs no screen-recording permission.
//
// Tier 2 is pixels, and it is deliberately NOT implemented here. If it is ever
// added it should wrap screenpipe rather than reinvent capture, and it must
// respect one macOS fact: the screen-recording permission is global, so the OS
// will not enforce per-app denial for us. Per-app deny has to be enforced in
// our capture layer, by matching the bundle id BEFORE a frame is stored.
//
// Default deny throughout. An app not on the allowlist is not observed, and
// the kill switch stops everything without needing the daemon to cooperate.
package observer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lgoyal6/amac/internal/event"
)

// Policy decides what may be observed. Empty allowlist means observe nothing:
// the safe default is the one you get by forgetting to configure it.
type Policy struct {
	Allow []string `json:"allow"`
	// Titles controls whether window titles are recorded at all. Titles leak
	// far more than app names (document names, URLs, subject lines), so it is
	// a separate decision from allowing the app.
	Titles map[string]bool `json:"titles"`
}

func (p Policy) allowed(app string) bool {
	for _, a := range p.Allow {
		if strings.EqualFold(a, app) {
			return true
		}
	}
	return false
}

func (p Policy) titlesFor(app string) bool { return p.Titles[app] }

func PolicyPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".amac", "observer.json")
}

// KillSwitchPath is checked on every tick. Creating the file stops observation
// immediately, without a daemon restart and without trusting the daemon to
// honour a request. `touch ~/.amac/observer.off` is a control you can use when
// the thing you do not want observed is on screen right now.
func KillSwitchPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".amac", "observer.off")
}

func LoadPolicy() (Policy, error) {
	var p Policy
	b, err := os.ReadFile(PolicyPath())
	if err != nil {
		if os.IsNotExist(err) {
			return Policy{Titles: map[string]bool{}}, nil // deny all
		}
		return p, err
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return p, fmt.Errorf("parse %s: %w", PolicyPath(), err)
	}
	if p.Titles == nil {
		p.Titles = map[string]bool{}
	}
	return p, nil
}

// ---------------------------------------------------------------- watching --

type Observer struct {
	log    *event.Log
	policy Policy

	mu      sync.Mutex
	current string
	since   time.Time
}

func New(log *event.Log, p Policy) *Observer {
	return &Observer{log: log, policy: p}
}

// frontmost asks System Events for the active app. This needs no
// screen-recording permission; on a locked-down Mac it needs Automation
// permission for the calling process, which macOS prompts for once.
func frontmost() (app, title string, err error) {
	out, err := exec.Command("osascript", "-e",
		`tell application "System Events" to name of first application process whose frontmost is true`).Output()
	if err != nil {
		return "", "", err
	}
	app = strings.TrimSpace(string(out))

	// Title is fetched separately and only when policy allows it, so a denied
	// app never has its title read at all, not even transiently.
	return app, "", nil
}

func windowTitle(app string) string {
	out, err := exec.Command("osascript", "-e", fmt.Sprintf(
		`tell application "System Events" to tell process %q to get name of front window`, app)).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Run polls until ctx ends. Poll rather than subscribe to workspace
// notifications because that would need a cgo/Objective-C bridge for a signal
// that changes on human timescales; 2s resolution is far finer than the
// question "what was he working on" needs.
func (o *Observer) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			// Flush on a fresh context, never the one that just cancelled.
			// The final span is written during shutdown, so passing the dead
			// context means every clean exit silently drops the last
			// observation, and a session where you never switched apps
			// records nothing at all.
			flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			o.flush(flushCtx)
			cancel()
			return ctx.Err()
		case <-t.C:
			if _, err := os.Stat(KillSwitchPath()); err == nil {
				o.flush(ctx)
				continue
			}
			app, _, err := frontmost()
			if err != nil || app == "" {
				continue
			}
			if !o.policy.allowed(app) {
				// Not observed. Close any open span so a denied app cannot
				// even extend the previous app's recorded duration.
				o.flush(ctx)
				continue
			}
			o.mark(ctx, app)
		}
	}
}

func (o *Observer) mark(ctx context.Context, app string) {
	o.mu.Lock()
	if o.current == app {
		o.mu.Unlock()
		return
	}
	prev, since := o.current, o.since
	o.current, o.since = app, time.Now()
	o.mu.Unlock()

	if prev != "" {
		o.emit(ctx, prev, time.Since(since))
	}
}

func (o *Observer) flush(ctx context.Context) {
	o.mu.Lock()
	prev, since := o.current, o.since
	o.current = ""
	o.mu.Unlock()
	if prev != "" {
		o.emit(ctx, prev, time.Since(since))
	}
}

func (o *Observer) emit(ctx context.Context, app string, dur time.Duration) {
	// Sub-second spans are switching noise, not work.
	if dur < time.Second {
		return
	}
	payload := map[string]any{"app": app, "seconds": int(dur.Seconds())}
	if o.policy.titlesFor(app) {
		if t := windowTitle(app); t != "" {
			payload["title"] = t
		}
	}
	if ev, err := event.New(event.KindObservation, "observer", "", payload); err == nil {
		_, _ = o.log.Append(ctx, ev)
	}
}
