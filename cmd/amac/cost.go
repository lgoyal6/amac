package main

import (
	"context"
	"flag"
	"fmt"
	"time"

	"github.com/lgoyal6/amac/internal/event"
	"github.com/lgoyal6/amac/internal/ledger"
)

func cmdCost(args []string) error {
	fs := flag.NewFlagSet("cost", flag.ExitOnError)
	days := fs.Int("days", 7, "look back this many days")
	dbPath := fs.String("db", defaultLogPath(), "event log path")
	if err := fs.Parse(args); err != nil {
		return err
	}

	log, err := event.Open(*dbPath, event.Full)
	if err != nil {
		return err
	}
	defer log.Close()

	since := time.Now().AddDate(0, 0, -*days)
	rep, err := ledger.Query(context.Background(), log.DB(), since)
	if err != nil {
		return err
	}
	if len(rep.Entries) == 0 {
		fmt.Printf("no sessions in the last %d days\n", *days)
		return nil
	}

	fmt.Printf("last %d days\n\n", *days)
	fmt.Printf("%-24s %-8s %-14s %-6s %10s %9s %6s %5s\n",
		"SESSION", "AGENT", "ACCOUNT", "WHEN", "COST", "CTX", "TURNS", "ASKS")
	for _, e := range rep.Entries {
		// A session started before accounts were recorded has no account, and
		// says so. Filling it in from today's environment would be a guess
		// about a process that exited weeks ago.
		acct := e.Account
		if acct == "" {
			acct = "-"
		}
		fmt.Printf("%-24s %-8s %-14s %-6s %10s %9s %6d %5d\n",
			clip(e.Session, 24), e.Agent, acct, e.Started.Local().Format("Jan02"),
			e.CostString(), ctx(e.Tokens, e.Window), e.Turns, e.Approvals)
	}

	fmt.Printf("\ntotal $%.4f across %d priced session(s)\n", rep.TotalCost, rep.Priced)
	if rep.Unpriced > 0 {
		// Never let an unpriced session read as a free one. Codex reports
		// tokens but no money, so the total is a floor, not the bill.
		fmt.Printf("%d session(s) reported no cost (agent does not expose it); total is a lower bound\n", rep.Unpriced)
	}
	return nil
}

// ctx renders context-window occupancy. This is not tokens billed, and is
// labelled CTX rather than TOKENS so the report cannot be misread as spend.
func ctx(used, size int64) string {
	if size == 0 {
		return "-"
	}
	return fmt.Sprintf("%d%%", used*100/size)
}

// clip keeps a long session name inside its column. Session names here run to
// "claude-job-discovery" and beyond, and one over-long name pushes every
// column on that row out of line with the rest of the table.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "\u2026"
}
