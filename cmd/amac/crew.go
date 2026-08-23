package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/lgoyal6/amac/internal/crew"
	"github.com/lgoyal6/amac/internal/event"
	"github.com/lgoyal6/amac/internal/model"
	"github.com/lgoyal6/amac/internal/orchestrator"
	"github.com/lgoyal6/amac/internal/router"
	"github.com/lgoyal6/amac/internal/supervisor"
)

// cmdCrew opens the org as sessions you can take over.
//
// `amac do` runs the same roles headless and returns when they are finished.
// This exists for the other case, which is most of them: work where you want
// to watch the plan being written, disagree with it, and carry on from inside
// the session rather than re-running the whole thing with a better prompt.
func cmdCrew(args []string) error {
	fs := flag.NewFlagSet("crew", flag.ExitOnError)
	dir := fs.String("dir", "", "working directory (default: cwd)")
	sizeFlag := fs.String("size", "", "force solo|pair|team instead of triaging")
	next := fs.Bool("next", false, "open the next role whose input is ready")
	all := fs.Bool("all", false, "open every role now, not just the first")
	planOnly := fs.Bool("plan", false, "print the chain and open nothing")
	dbPath := fs.String("db", defaultLogPath(), "event log path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	task := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if task == "" {
		return fmt.Errorf("usage: amac crew [-size solo|pair|team] [-next|-all|-plan] <task...>")
	}

	workdir := *dir
	if workdir == "" {
		var err error
		if workdir, err = os.Getwd(); err != nil {
			return err
		}
	}

	log, err := event.Open(*dbPath, event.Full)
	if err != nil {
		return err
	}
	defer log.Close()

	reg, _ := model.FromEnv()
	sup := supervisor.New(log)
	defer sup.Shutdown()
	orch := orchestrator.New(sup, router.New(reg, log), log)

	ctx := context.Background()

	size := orchestrator.Size(*sizeFlag)
	reason := "forced by caller"
	if *sizeFlag == "" {
		size, reason = orch.Triage(ctx, task)
	}

	sessions := orch.Attachable(task, workdir, size)
	fmt.Printf("%s  (%s)\n", size, reason)
	fmt.Printf("handoff  %s\n\n", crew.RunDir(crew.Slug(task)))

	for i, s := range sessions {
		state := statusOf(s)
		fmt.Printf("  %d. %-9s %-7s %-9s %s\n", i+1, s.Role, s.Agent, state, s.Name)
	}
	fmt.Println()

	if *planOnly {
		return nil
	}

	opened := 0
	for _, s := range sessions {
		if crew.Exists(s.Name) {
			continue
		}
		// A role whose input has not been written yet has nothing to read. In
		// -all it would sit there burning context waiting for a file, so the
		// chain only advances as far as the artifacts allow.
		if s.Input != "" && !fileExists(s.Input) {
			if !*all {
				break
			}
			continue
		}
		if err := openOne(ctx, log, orch, s, task); err != nil {
			return err
		}
		opened++
		if !*all && !*next {
			break
		}
	}

	if opened == 0 {
		fmt.Println("nothing to open: every role is either running or waiting on the one before it")
		return nil
	}
	fmt.Printf("\n%d session(s) opened. Attach to any of them, or take one over mid-run.\n", opened)
	return nil
}

func openOne(ctx context.Context, log *event.Log, orch *orchestrator.Orchestrator, s crew.Session, task string) error {
	brief := orchestrator.BriefFor(s, task)
	if err := crew.Open(s, brief); err != nil {
		return fmt.Errorf("%s: %w", s.Role, err)
	}
	ev, err := event.New(event.KindSessionStarted, "crew", s.Name, map[string]any{
		"role": s.Role, "agent": s.Agent, "task": task,
		"dir": s.Dir, "input": s.Input, "output": s.Output,
	})
	if err != nil {
		return err
	}
	if _, err := log.Append(ctx, ev); err != nil {
		return err
	}
	fmt.Printf("  opened %-9s %s\n", s.Role, s.Attach())
	return nil
}

func statusOf(s crew.Session) string {
	switch {
	case crew.Exists(s.Name):
		return "running"
	case fileExists(s.Output):
		return "done"
	case s.Input != "" && !fileExists(s.Input):
		return "waiting"
	default:
		return "ready"
	}
}

func fileExists(p string) bool {
	if p == "" {
		return false
	}
	st, err := os.Stat(p)
	return err == nil && st.Size() > 0
}
