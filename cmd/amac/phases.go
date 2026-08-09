package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/lgoyal6/amac/internal/eval"
	"github.com/lgoyal6/amac/internal/event"
	"github.com/lgoyal6/amac/internal/miner"
	"github.com/lgoyal6/amac/internal/model"
	"github.com/lgoyal6/amac/internal/observer"
	"github.com/lgoyal6/amac/internal/orchestrator"
	"github.com/lgoyal6/amac/internal/router"
	"github.com/lgoyal6/amac/internal/supervisor"
)

// ---------------------------------------------------------------- models ----

func cmdModels(args []string) error {
	fs := flag.NewFlagSet("models", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

	reg, missing := model.FromEnv()
	tiers := reg.Tiers()
	if len(tiers) == 0 {
		fmt.Println("no model providers configured")
	}
	for _, t := range tiers {
		p, _ := reg.Get(t)
		fmt.Printf("  %-7s %-24s %s\n", t, p.Model(), p.Name())
	}
	if len(missing) > 0 {
		fmt.Printf("\nnot configured:\n")
		for _, m := range missing {
			fmt.Printf("  %s\n", m)
		}
		fmt.Printf("\nAny OpenAI-compatible host works for the cheap tier, for example:\n")
		fmt.Printf("  export AMAC_CHEAP_BASE_URL=https://api.gmi-serving.com/v1\n")
		fmt.Printf("  export AMAC_CHEAP_API_KEY=...\n")
		fmt.Printf("  export AMAC_CHEAP_MODEL=deepseek-ai/DeepSeek-V4-Flash\n")
	}
	return nil
}

// ---------------------------------------------------------------- route -----

func cmdRoute(args []string) error {
	fs := flag.NewFlagSet("route", flag.ExitOnError)
	dbPath := fs.String("db", defaultLogPath(), "event log path")
	dry := fs.Bool("dry", false, "classify only, do not call a model")
	if err := fs.Parse(args); err != nil {
		return err
	}
	prompt := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if prompt == "" {
		return fmt.Errorf("usage: amac route [-dry] <prompt...>")
	}

	log, err := event.Open(*dbPath, event.Full)
	if err != nil {
		return err
	}
	defer log.Close()

	reg, missing := model.FromEnv()
	r := router.New(reg, log)

	if *dry {
		// Show what the classifier decided, not what the cascade would do
		// after applying the no-verifier floor. Conflating the two makes the
		// audit path lie about its own reasoning.
		tier, reason := r.Classify(model.Request{Prompt: prompt})
		fmt.Printf("classifier: %s (%s)\n", tier, reason)
		if tier < model.TierMid {
			fmt.Printf("            without a verifier this would be floored to mid\n")
		}
		return nil
	}
	if len(reg.Tiers()) == 0 {
		return fmt.Errorf("no providers configured; run `amac models`. Missing: %s", strings.Join(missing, ", "))
	}

	resp, dec, err := r.Call(context.Background(), model.Request{Prompt: prompt, MaxTokens: 512},
		router.NonEmptyVerifier(1))
	if err != nil {
		return err
	}
	fmt.Printf("%s\n\n", strings.TrimSpace(resp.Text))
	fmt.Printf("tier=%s reason=%q escalated=%v cost=$%.6f\n", dec.Tier, dec.Reason, dec.Escalated, dec.TotalCost())
	for _, a := range dec.Attempts {
		status := "kept"
		if !a.Kept {
			status = "rejected: " + a.Err
		}
		fmt.Printf("  %-7s %-22s %8s  %s\n", a.Tier, a.Model, a.Latency.Round(time.Millisecond), status)
	}
	return nil
}

// ---------------------------------------------------------------- eval ------

func cmdEval(args []string) error {
	fs := flag.NewFlagSet("eval", flag.ExitOnError)
	tasksPath := fs.String("tasks", "evals/tasks.json", "task set")
	dbPath := fs.String("db", defaultLogPath(), "event log path")
	out := fs.String("out", "", "write full results as JSON here")
	if err := fs.Parse(args); err != nil {
		return err
	}

	tasks, err := eval.LoadTasks(*tasksPath)
	if err != nil {
		return err
	}
	log, err := event.Open(*dbPath, event.Full)
	if err != nil {
		return err
	}
	defer log.Close()

	reg, missing := model.FromEnv()
	if len(reg.Tiers()) == 0 {
		return fmt.Errorf("no providers configured, nothing to compare. Missing: %s", strings.Join(missing, ", "))
	}

	runner := &eval.Runner{Reg: reg, Router: router.New(reg, log)}
	fmt.Printf("running %d tasks across %d arms...\n\n", len(tasks), len(reg.Tiers())+1)

	rep, err := runner.Run(context.Background(), tasks)
	if err != nil {
		return err
	}
	fmt.Print(rep.Table())

	if *out != "" {
		b, _ := json.MarshalIndent(rep, "", "  ")
		if err := os.WriteFile(*out, b, 0o644); err != nil {
			return err
		}
		fmt.Printf("\nfull results: %s\n", *out)
	}
	return nil
}

// ---------------------------------------------------------------- do --------

func cmdDo(args []string) error {
	fs := flag.NewFlagSet("do", flag.ExitOnError)
	dir := fs.String("dir", "", "working directory (default: cwd)")
	budget := fs.Float64("budget", 0, "max USD for this task (0 = no ceiling)")
	dbPath := fs.String("db", defaultLogPath(), "event log path")
	sizeFlag := fs.String("size", "", "force solo|pair|team instead of triaging")
	if err := fs.Parse(args); err != nil {
		return err
	}
	task := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if task == "" {
		return fmt.Errorf("usage: amac do [-size solo|pair|team] [-budget USD] <task...>")
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *sizeFlag != "" {
		fmt.Printf("size: %s (forced)\n\n", *sizeFlag)
	} else {
		size, reason := orch.Triage(ctx, task)
		fmt.Printf("size: %s (%s)\n\n", size, reason)
	}

	run, err := orch.Execute(ctx, task, workdir, *budget)
	if err != nil {
		return err
	}

	fmt.Printf("\n%s in %s\n", run.Size, run.Elapsed.Round(time.Second))
	for _, r := range run.Results {
		if r.Skipped != "" {
			fmt.Printf("  %-9s SKIPPED  %s\n", r.Role, r.Skipped)
			continue
		}
		fmt.Printf("  %-9s %-8s %-14s %s\n", r.Role, r.Agent, r.Session, r.Elapsed.Round(time.Second))
	}
	return nil
}

// ---------------------------------------------------------------- watch -----

func cmdWatch(args []string) error {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	dbPath := fs.String("db", defaultLogPath(), "event log path")
	interval := fs.Duration("interval", 2*time.Second, "poll interval")
	if err := fs.Parse(args); err != nil {
		return err
	}

	policy, err := observer.LoadPolicy()
	if err != nil {
		return err
	}
	if len(policy.Allow) == 0 {
		fmt.Printf("observer is denying everything: no apps allowlisted.\n\n")
		fmt.Printf("Create %s, for example:\n", observer.PolicyPath())
		fmt.Printf(`  {
    "allow": ["Terminal", "Code", "Safari"],
    "titles": {"Terminal": true}
  }

`)
		fmt.Printf("Apps not listed are never observed. Titles are a separate opt-in\nbecause they leak far more than app names.\n")
		fmt.Printf("Kill switch: touch %s\n", observer.KillSwitchPath())
		return nil
	}

	log, err := event.Open(*dbPath, event.Full)
	if err != nil {
		return err
	}
	defer log.Close()

	fmt.Printf("observing %d app(s): %s\n", len(policy.Allow), strings.Join(policy.Allow, ", "))
	fmt.Printf("kill switch: touch %s\n\n", observer.KillSwitchPath())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	obs := observer.New(log, policy)
	if err := obs.Run(ctx, *interval); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

// ---------------------------------------------------------------- learn -----

func cmdLearn(args []string) error {
	fs := flag.NewFlagSet("learn", flag.ExitOnError)
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

	rep, err := miner.Mine(context.Background(), log.DB(), time.Now().AddDate(0, 0, -*days))
	if err != nil {
		return err
	}

	fmt.Printf("patterns over the last %d days\n\n", *days)
	if len(rep.Suggestions) == 0 {
		fmt.Println("  nothing worth suggesting yet. The miner needs repeated behaviour;")
		fmt.Println("  come back after a week of real use.")
	}
	for _, s := range rep.Suggestions {
		fmt.Printf("  [%s] %s\n", s.Kind, s.Title)
		fmt.Printf("      %s (confidence %.0f%%)\n", s.Evidence, s.Confidence*100)
		fmt.Printf("      -> %s\n\n", s.Action)
	}
	if len(rep.Stats) > 0 {
		fmt.Printf("  counted:")
		for k, v := range rep.Stats {
			fmt.Printf(" %s=%d", k, v)
		}
		fmt.Println()
	}
	return nil
}
