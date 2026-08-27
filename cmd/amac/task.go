package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/lgoyal6/amac/internal/crew"
	"github.com/lgoyal6/amac/internal/event"
	"github.com/lgoyal6/amac/internal/queue"
)

// cmdTask drives the work queue.
//
// The queue exists because the org was a chain: planner, then executor, then
// verifier, each waiting on the one before. That is right when a task has
// stages and wrong when there are simply several unrelated things to do, which
// is most days. A queue lets as many agents work as there are tasks, and the
// hard part is not the parallelism but making sure no two of them take the same
// one, and that a task held by an agent that dies is picked up rather than lost.
func cmdTask(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: amac task add|list|claim|renew|done|fail|release ...")
	}

	fs := flag.NewFlagSet("task", flag.ExitOnError)
	dbPath := fs.String("db", defaultLogPath(), "event log path")
	dir := fs.String("dir", "", "working directory for the task")
	owner := fs.String("owner", defaultOwner(), "who is claiming")
	lease := fs.Duration("lease", 15*time.Minute, "how long a claim is held without renewal")
	state := fs.String("state", "", "filter: ready|claimed|done|failed|canceled")
	token := fs.Int64("token", 0, "the fencing token from the claim")
	open := fs.Bool("open", false, "claim: also open a crew session for it")
	sub := args[0]
	// Flags first, whatever order they were typed in. Go's flag package stops
	// parsing at the first positional argument, so `task done some-id -token 1`
	// silently ignores the token and fails asking for the token that was right
	// there. That is a bad enough error message on its own; it is worse here,
	// because the token is the one argument nobody can guess.
	if err := fs.Parse(hoistFlags(args[1:])); err != nil {
		return err
	}
	rest := strings.TrimSpace(strings.Join(fs.Args(), " "))

	log, err := event.Open(*dbPath, event.Full)
	if err != nil {
		return err
	}
	defer log.Close()
	q, err := queue.Open(log)
	if err != nil {
		return err
	}
	ctx := context.Background()

	switch sub {
	case "add":
		if rest == "" {
			return fmt.Errorf("usage: amac task add [-dir PATH] <title...>")
		}
		workdir := *dir
		if workdir == "" {
			workdir, _ = os.Getwd()
		}
		t, err := q.File(ctx, queue.Task{ID: crew.Slug(rest), Title: rest, Dir: workdir})
		if err != nil {
			return err
		}
		fmt.Printf("%-8s %s\n", t.State, t.ID)
		return nil

	case "list":
		list, err := q.List(ctx, queue.State(*state))
		if err != nil {
			return err
		}
		if len(list) == 0 {
			fmt.Println("nothing queued")
			return nil
		}
		for _, t := range list {
			held := ""
			if t.State == queue.Claimed {
				held = fmt.Sprintf("  %s, lease %s", t.Owner, until(t.Lease))
			}
			tries := ""
			if t.Attempt > 1 {
				// Worth showing: a task on its third attempt is one that two
				// agents have already failed to finish.
				tries = fmt.Sprintf("  attempt %d", t.Attempt)
			}
			fmt.Printf("%-9s %-34s %s%s%s\n", t.State, t.ID, t.Title, held, tries)
		}
		return nil

	case "claim":
		t, err := q.Claim(ctx, *owner, *lease)
		if err == queue.ErrNoWork {
			fmt.Println("nothing claimable")
			return nil
		}
		if err != nil {
			return err
		}
		fmt.Printf("claimed %s (token %d, lease %s)\n  %s\n", t.ID, t.Token, until(t.Lease), t.Title)
		if *open {
			s := crew.Session{
				Name: crew.Name(t.ID, "worker"), Role: "worker", Agent: "claude",
				Dir: t.Dir, Output: crew.RunDir(t.ID) + "/worker.md",
			}
			if err := crew.Open(s, crew.Brief(s, "Do this task.", t.Title)); err != nil {
				return err
			}
			fmt.Printf("  %s\n", s.Attach())
		}
		// The token is printed because every later call needs it. It is the
		// whole fencing mechanism: without it a worker whose lease expired
		// could still report a result for work someone else now holds.
		fmt.Printf("\n  amac task done -token %d %s\n", t.Token, t.ID)
		return nil

	case "done", "fail", "cancel":
		if rest == "" || *token == 0 {
			return fmt.Errorf("usage: amac task %s -token N <id>", sub)
		}
		final := map[string]queue.State{
			"done": queue.Done, "fail": queue.Failed, "cancel": queue.Canceled,
		}[sub]
		if err := q.Finish(ctx, rest, *token, final, ""); err != nil {
			return err
		}
		fmt.Printf("%s %s\n", final, rest)
		return nil

	case "renew":
		// Without this the lease is a trap: a task that legitimately takes
		// longer than the lease gets taken away from the agent still doing it,
		// and two agents end up on the same work for entirely correct reasons.
		if rest == "" || *token == 0 {
			return fmt.Errorf("usage: amac task renew -token N [-lease D] <id>")
		}
		if err := q.Renew(ctx, rest, *token, *lease); err != nil {
			return err
		}
		fmt.Printf("renewed %s for %s\n", rest, *lease)
		return nil

	case "release":
		if rest == "" || *token == 0 {
			return fmt.Errorf("usage: amac task release -token N <id>")
		}
		if err := q.Release(ctx, rest, *token); err != nil {
			return err
		}
		fmt.Printf("released %s\n", rest)
		return nil
	}
	return fmt.Errorf("unknown subcommand %q", sub)
}

// defaultOwner names the claimer after the tmux session when there is one, so a
// queue listing says which terminal is holding what rather than which unix user
// is, which on a one-person machine is the same answer every time.
func defaultOwner() string {
	if s := callerSession(); s != "" {
		return s
	}
	host, _ := os.Hostname()
	return host
}

func until(t time.Time) string {
	d := time.Until(t)
	if d < 0 {
		return "expired"
	}
	return d.Round(time.Second).String()
}

// hoistFlags moves flags and their values ahead of positional arguments.
//
// It only understands the flags this command actually has, which is what makes
// it safe: -token and the rest take a value, -open does not, and a bare word is
// positional. Guessing at unknown flags would be how a task id starting with a
// dash quietly becomes a parse error.
func hoistFlags(args []string) []string {
	takesValue := map[string]bool{
		"-db": true, "-dir": true, "-owner": true, "-lease": true,
		"-state": true, "-token": true,
	}
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case strings.Contains(a, "=") && strings.HasPrefix(a, "-"):
			flags = append(flags, a)
		case takesValue[a] && i+1 < len(args):
			flags = append(flags, a, args[i+1])
			i++
		case strings.HasPrefix(a, "-"):
			flags = append(flags, a)
		default:
			positional = append(positional, a)
		}
	}
	return append(flags, positional...)
}
