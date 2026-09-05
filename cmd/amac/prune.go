package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/lgoyal6/amac/internal/event"
)

// cmdPrune applies the log's retention policy, and prints it by default rather
// than doing it.
//
// The default is a dry run because this is the one command here that removes
// information. Everything else in amac either observes or appends, so a
// mistyped flag costs a wasted minute; a mistyped flag on this one costs
// history. Printing first also makes the policy arguable: the plan says which
// rule fires, on how many rows, for how many bytes, and why that rule exists.
func cmdPrune(args []string) error {
	fs := flag.NewFlagSet("prune", flag.ExitOnError)
	apply := fs.Bool("apply", false, "actually do it (default is to print the plan)")
	vacuum := fs.Bool("vacuum", false, "reclaim file space afterwards; rewrites the whole database")
	dbPath := fs.String("db", defaultLogPath(), "event log path")
	if err := fs.Parse(args); err != nil {
		return err
	}

	log, err := event.Open(*dbPath, event.Relaxed)
	if err != nil {
		return err
	}
	defer log.Close()

	ctx := context.Background()
	rules := event.DefaultRetention()
	before, _ := fileSize(*dbPath)

	plan, err := log.Plan(ctx, rules, time.Now())
	if err != nil {
		return err
	}
	if len(plan) == 0 {
		fmt.Printf("nothing to prune in %s (%s)\n", *dbPath, humanSize(before))
		return nil
	}

	fmt.Printf("%s, %s on disk\n\n", *dbPath, humanSize(before))
	var rows int
	var bytes int64
	for _, c := range plan {
		fmt.Printf("  %s\n", c)
		rows += c.Rows
		bytes += c.Bytes
	}
	fmt.Printf("\n  %d rows, about %s of payload\n", rows, humanSize(bytes))
	// Named rather than implied. Somebody reading this output should not have
	// to infer what is protected from what is listed.
	fmt.Printf("  untouched: permission requests and answers, actuations, session\n")
	fmt.Printf("             starts and ends, automation runs, applications, routes\n")

	if !*apply {
		fmt.Printf("\nthis was a plan. re-run with -apply to carry it out\n")
		return nil
	}

	if _, err := log.Apply(ctx, rules, time.Now()); err != nil {
		return err
	}
	fmt.Printf("\napplied\n")

	if !*vacuum {
		// Said explicitly, because otherwise the file not shrinking reads as a
		// failure. SQLite keeps the freed pages for reuse until a VACUUM.
		fmt.Printf("the file will not shrink until the freed pages are reclaimed: -vacuum\n")
		return nil
	}
	fmt.Printf("vacuuming, which rewrites the database and needs room for a second copy...\n")
	if err := log.Vacuum(ctx); err != nil {
		return err
	}
	after, _ := fileSize(*dbPath)
	fmt.Printf("%s to %s\n", humanSize(before), humanSize(after))
	return nil
}

func fileSize(path string) (int64, error) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

func humanSize(b int64) string {
	switch {
	case b >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.0fKB", float64(b)/(1<<10))
	}
	return fmt.Sprintf("%dB", b)
}
