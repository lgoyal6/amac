package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"sort"

	"github.com/lgoyal6/amac/internal/event"
	"github.com/lgoyal6/amac/internal/health"
)

// cmdHealth sweeps the declared automations, records the result, and decides
// whether it is worth interrupting him.
//
// Every sweep appends an event whether or not anything is wrong. The alert is
// derived from the difference between this sweep and the last one, which is
// why the log has to hold the healthy sweeps too: without them there is no
// baseline to diff against, and every restart would re-announce every existing
// failure.
func cmdHealth(args []string) error {
	fs := flag.NewFlagSet("health", flag.ExitOnError)
	digest := fs.Bool("digest", false, "DM the full roster, healthy or not")
	alert := fs.Bool("alert", false, "DM only what changed since the last sweep")
	quiet := fs.Bool("quiet", false, "no stdout, for launchd")
	runs := fs.Bool("runs", false, "also report every individual run since the last sweep")
	dry := fs.Bool("dry-run", false, "print the DM instead of sending it")
	dbPath := fs.String("db", defaultLogPath(), "event log path")
	timeout := fs.Duration("timeout", 90*time.Second, "overall probe timeout")
	if err := fs.Parse(args); err != nil {
		return err
	}

	log, err := event.Open(*dbPath, event.Full)
	if err != nil {
		return err
	}
	defer log.Close()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	prev, err := lastStates(ctx, log)
	if err != nil {
		return err
	}

	// The roster is loaded before anything else runs. A sweep over an empty
	// roster reports every automation as fine by never looking at one, which is
	// the exact failure this command exists to prevent, so a bad or missing
	// roster stops the command rather than producing a clean bill of health.
	roster, err := health.Roster(log)
	if err != nil {
		return err
	}
	reports := health.Sweep(ctx, roster)

	if !*quiet {
		for _, r := range reports {
			fmt.Printf("%s %-22s %-8s %s\n", r.State.Icon(), r.Name, r.State, r.Detail)
			for _, n := range r.Notes {
				fmt.Printf("  %-22s   · %s\n", "", n)
			}
			if r.Err != "" {
				fmt.Printf("  %-22s   · %s\n", "", r.Err)
			}
		}
	}

	// Record before notifying. If Discord is the thing that is down, the sweep
	// still has to survive in the log.
	ev, err := event.New(event.KindAutomationCheck, "health", "", map[string]any{
		"reports": reports,
	})
	if err != nil {
		return err
	}
	stored, err := log.Append(ctx, ev)
	if err != nil {
		return err
	}
	if !*quiet {
		fmt.Printf("\nevent seq=%d\n", stored.Seq)
	}

	// deliver prints or sends, so the wording of a DM can be iterated on
	// without putting a run of drafts on his phone.
	deliver := func(kind, msg string) error {
		if *dry {
			fmt.Printf("--- %s (dry run, %d chars) ---\n%s\n", kind, len(msg), msg)
			return nil
		}
		if err := health.Send(ctx, msg); err != nil {
			return fmt.Errorf("%s: %w", kind, err)
		}
		if !*quiet {
			fmt.Println(kind + " sent")
		}
		return nil
	}

	if *runs {
		if err := reportRuns(ctx, log, *quiet); err != nil {
			fmt.Fprintf(os.Stderr, "amac: runs: %v\n", err)
		}
	}

	switch {
	case *digest:
		return deliver("digest", health.Digest(reports))
	case *alert:
		msg, changed := health.Alert(reports, prev)
		if !changed {
			if !*quiet {
				fmt.Println("no change since last sweep, staying quiet")
			}
			return nil
		}
		return deliver("alert", msg)
	}

	// A failing automation is not a failing command. Exiting non-zero here
	// would make launchd treat a known-bad pipeline as a broken monitor and
	// start backing the monitor off, which is exactly backwards.
	return nil
}

// lastStates reads the previous sweep's verdicts, so an alert can be a diff.
// A missing or unreadable previous sweep yields an empty map, which makes the
// first run treat every current failure as new. That is the correct behaviour
// on a fresh install: he has not been told about them yet.
func lastStates(ctx context.Context, log *event.Log) (map[string]health.State, error) {
	row := log.DB().QueryRowContext(ctx,
		`SELECT payload FROM events WHERE kind = ? ORDER BY seq DESC LIMIT 1`,
		string(event.KindAutomationCheck))
	var payload []byte
	switch err := row.Scan(&payload); {
	case err == sql.ErrNoRows:
		return map[string]health.State{}, nil
	case err != nil:
		return nil, err
	}
	var body struct {
		Reports []health.Report `json:"reports"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		fmt.Fprintf(os.Stderr, "amac: previous sweep unreadable (%v), treating as first run\n", err)
		return map[string]health.State{}, nil
	}
	out := make(map[string]health.State, len(body.Reports))
	for _, r := range body.Reports {
		out[r.Name] = r.State
	}
	return out, nil
}

// reportRuns reports each individual run exactly once.
//
// The state sweep answers "is this delivering?" from the newest run, which is
// the right question for waking someone up and the wrong one for noticing a
// failure that was recovered from. job-discovery crashed three times in twenty
// hours while the sweep reported it green throughout, because a success
// followed each crash before anyone looked.
func reportRuns(ctx context.Context, log *event.Log, quiet bool) error {
	seen, first, err := seenRuns(ctx, log)
	if err != nil {
		return err
	}

	fresh := health.NewRuns(ctx, seen)
	if len(fresh) == 0 {
		if !quiet {
			fmt.Println("no new runs")
		}
		return nil
	}
	// Phone activity reads newest-first. Oldest-first made a catch-up batch look
	// random once the date was hidden behind a bare clock time.
	sort.Slice(fresh, func(i, j int) bool { return fresh[i].Started.After(fresh[j].Started) })

	for _, r := range fresh {
		ev, err := event.New(event.KindAutomationRun, "health", r.Automation, r)
		if err != nil {
			return err
		}
		if _, err := log.Append(ctx, ev); err != nil {
			return err
		}
		if !quiet {
			fmt.Printf("%s %-22s %-8s %s\n", r.Status.Icon(), r.Automation, r.Status, r.Detail)
		}
	}

	// The very first sweep sees every run the APIs still remember, which is
	// dozens. Announcing history he has already lived through would bury the
	// one thing this exists to surface, so the first pass only establishes the
	// baseline.
	if first {
		if !quiet {
			fmt.Printf("\nbaseline: %d past run(s) recorded, not sent\n", len(fresh))
		}
		return nil
	}

	// Failures go out on their own so they are never a line in a list he
	// skims. The batch then carries only what they did not, because a lone
	// failure otherwise arrives twice: once as the alert and once as a batch
	// of one saying the same thing.
	var rest []health.Run
	for _, r := range fresh {
		if r.Status != health.RunFailed {
			rest = append(rest, r)
			continue
		}
		if err := health.Send(ctx, health.RunFailure(r)); err != nil {
			return err
		}
	}
	if len(rest) == 0 {
		return nil
	}
	return health.Send(ctx, health.RunBatch(rest))
}

// seenRuns returns the run ids already reported, and whether this is the first
// sweep to look.
func seenRuns(ctx context.Context, log *event.Log) (map[string]bool, bool, error) {
	rows, err := log.DB().QueryContext(ctx,
		`SELECT session, payload FROM events WHERE kind = ? ORDER BY seq DESC LIMIT 2000`,
		string(event.KindAutomationRun))
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	seen := map[string]bool{}
	for rows.Next() {
		var automation string
		var payload []byte
		if err := rows.Scan(&automation, &payload); err != nil {
			continue
		}
		var r struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(payload, &r) == nil && r.ID != "" {
			seen[automation+"/"+r.ID] = true
		}
	}
	return seen, len(seen) == 0, nil
}
