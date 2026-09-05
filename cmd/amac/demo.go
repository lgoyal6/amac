package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/lgoyal6/amac/internal/apply"
	"github.com/lgoyal6/amac/internal/event"
	"github.com/lgoyal6/amac/internal/health"
)

// cmdDemo runs the board against a seeded throwaway log.
//
// Until this existed, the only way to evaluate amac was to adopt it: install
// Tailscale, run tmux, hand-write a roster of your own automations, install
// hooks into your agents. That is a lot to ask of somebody deciding whether the
// idea is interesting, and it is the difference between a project people can
// look at and one they have to commit to first.
//
// So: a temporary event log, a roster nobody has to write, and a week of
// invented history that shows the thing worth seeing. It binds to localhost,
// never the tailnet, and it never touches ~/.amac. Deleting the directory it
// prints is the whole uninstall.
func cmdDemo(args []string) error {
	dir, err := os.MkdirTemp("", "amac-demo-")
	if err != nil {
		return err
	}
	dbPath := filepath.Join(dir, "events.db")
	cfgPath := filepath.Join(dir, "health.json")

	if err := os.WriteFile(cfgPath, []byte(demoRoster), 0o600); err != nil {
		return err
	}
	log, err := event.Open(dbPath, event.Relaxed)
	if err != nil {
		return err
	}
	if err := seedDemo(log); err != nil {
		log.Close()
		return err
	}
	log.Close()

	os.Setenv("AMAC_HEALTH_CONFIG", cfgPath)
	fmt.Printf("amac demo\n")
	fmt.Printf("  seeded     %s\n", dir)
	fmt.Printf("  data       invented, and thrown away when you delete that directory\n")
	fmt.Printf("  note       automations and the run log are seeded; the machine card is\n")
	fmt.Printf("             your real Mac, and agents shows your real tmux sessions\n\n")

	return cmdDaemon(append([]string{"-localhost", "-db", dbPath}, args...))
}

// demoRoster is a plausible set of automations rather than this machine's.
// Every field is one a real roster carries, so the schedule tab teaches the
// format by showing a filled-in version of it.
const demoRoster = `{"automations":[
 {"name":"nightly-backup","what":"copies the database offsite","every":"24h","grace":"6h",
  "schedule":"daily 02:00","host":"This Mac · launchd","probe":"launchd_marker",
  "with":{"label":"com.demo.backup","log":"/tmp/amac-demo/backup.log"}},
 {"name":"digest-email","what":"sends the morning digest","every":"24h","grace":"4h",
  "schedule":"daily 07:00","host":"GitHub Actions","probe":"launchd_marker",
  "with":{"label":"com.demo.digest","log":"/tmp/amac-demo/digest.log"}},
 {"name":"link-checker","what":"checks every published link still resolves","every":"12h","grace":"6h",
  "schedule":"twice daily","host":"GitHub Actions","probe":"launchd_marker",
  "with":{"label":"com.demo.links","log":"/tmp/amac-demo/links.log"}},
 {"name":"cache-warmer","what":"rebuilds the search index","every":"1h","grace":"2h",
  "schedule":"hourly","host":"This Mac · launchd","probe":"launchd_marker",
  "with":{"label":"com.demo.cache","log":"/tmp/amac-demo/cache.log"}},
 {"name":"invoice-sync","what":"pulls invoices from the billing provider","every":"6h","grace":"3h",
  "schedule":"every 6 hours","host":"Railway","probe":"launchd_marker",
  "with":{"label":"com.demo.invoice","log":"/tmp/amac-demo/invoice.log"}}
]}`

// seedDemo writes a week that makes the point.
//
// The interesting state is not "everything is broken", which any dashboard can
// draw, but the one this project exists for: a pipeline that failed repeatedly
// and was rescued each time by the run after it, so every status check ever
// taken of it was green and every one of them was useless.
func seedDemo(log *event.Log) error {
	ctx := context.Background()
	now := time.Now().UTC()

	type plan struct {
		name    string
		every   time.Duration
		failAt  []int // which runs, counting back from the newest, failed
		noOpAt  []int
		detail  string
		failing bool
	}
	plans := []plan{
		// Failed six times in the middle of the week, green ever since. This is
		// the row the whole run log exists to show.
		{name: "invoice-sync", every: 6 * time.Hour, failAt: []int{12, 13, 15, 18, 19, 22},
			detail: "synced 41 invoices"},
		// Broken right now, so the two states are visibly different.
		{name: "link-checker", every: 12 * time.Hour, failAt: []int{0, 1},
			detail: "checked 208 links"},
		// Chatty and healthy, which is what most of a roster looks like.
		{name: "cache-warmer", every: time.Hour, detail: "index rebuilt in 4s"},
		// Over-scheduled on purpose: mostly no-ops, so "last run green" is
		// almost always true and almost meaningless.
		{name: "digest-email", every: 24 * time.Hour, noOpAt: []int{1, 2, 3, 5},
			detail: "digest sent to 3 addresses"},
		{name: "nightly-backup", every: 24 * time.Hour, detail: "2.1GB copied"},
	}

	in := func(xs []int, i int) bool {
		for _, x := range xs {
			if x == i {
				return true
			}
		}
		return false
	}

	for _, p := range plans {
		runs := int(7 * 24 * time.Hour / p.every)
		if runs > 60 {
			runs = 60
		}
		for i := runs - 1; i >= 0; i-- {
			started := now.Add(-time.Duration(i) * p.every)
			status, detail := "ok", p.detail
			switch {
			case in(p.failAt, i):
				status, detail = "failed", "exit status 1"
			case in(p.noOpAt, i):
				status, detail = "skipped", "nothing new to send"
			}
			payload := map[string]any{
				"automation": p.name,
				"id":         fmt.Sprintf("%s-%d", p.name, i),
				"status":     status,
				"started":    started.Format(time.RFC3339),
				"duration":   int64(90 * time.Second),
				"detail":     detail,
			}
			ev, err := event.New(event.KindAutomationRun, "demo", p.name, payload)
			if err != nil {
				return err
			}
			ev.At = started
			if _, err := log.Append(ctx, ev); err != nil {
				return err
			}
		}
	}

	// One sweep, so the status tab has a verdict to render. link-checker is the
	// only one failing now; the rest are ok, which is exactly the disagreement
	// with the run log that the run log is for.
	reports := []health.Report{}
	for _, p := range plans {
		r := health.Report{Name: p.name, Category: "automation", State: health.OK,
			Last: now.Add(-p.every / 2), Detail: "last delivery " + short(p.every/2) + " ago"}
		if p.name == "link-checker" {
			r.State, r.Detail = health.Failing, "last run exited 1"
		}
		reports = append(reports, r)
	}
	sweep, err := event.New(event.KindAutomationCheck, "demo", "", map[string]any{"reports": reports})
	if err != nil {
		return err
	}
	if _, err := log.Append(ctx, sweep); err != nil {
		return err
	}

	return seedDemoApplications(log, now)
}

func short(d time.Duration) string {
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh", int(d.Hours()))
}

// seedDemoApplications fills the jobs tab, which is otherwise an empty table
// and reads as broken rather than as unused.
func seedDemoApplications(log *event.Log, now time.Time) error {
	repo, err := apply.NewRepository(log.DB())
	if err != nil {
		return err
	}
	rows := []struct {
		company, role, status, tier string
		daysAgo                     int
	}{
		{"Northwind", "Backend Engineer Intern", "Interview", "T1", 3},
		{"Contoso", "Platform Engineer Intern", "Applied", "T1", 5},
		{"Fabrikam", "Site Reliability Intern", "In Review", "T2", 6},
		{"Tailspin", "Distributed Systems Intern", "Applied", "T1", 9},
		{"Litware", "Infrastructure Intern", "Rejected", "T3", 14},
		{"Adventure Works", "Data Platform Intern", "Applied", "T2", 17},
		{"Proseware", "Systems Engineer Intern", "Applied", "", 21},
		{"Wingtip", "Developer Tools Intern", "Applied", "T2", 26},
	}
	for i, r := range rows {
		_, err := repo.UpsertLocal(context.Background(), apply.Application{
			ID: fmt.Sprintf("demo-%d", i), Company: r.company, Role: r.role,
			Status: r.status, Tier: r.tier, Location: "Remote",
			AppliedAt: now.AddDate(0, 0, -r.daysAgo),
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// demoJSON keeps the roster honest: it has to parse as the real loader parses
// it, or the demo teaches a format that does not work.
func init() {
	var probe struct {
		Automations []map[string]any `json:"automations"`
	}
	if err := json.Unmarshal([]byte(demoRoster), &probe); err != nil {
		panic("demo roster is not valid JSON: " + err.Error())
	}
}
