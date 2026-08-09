package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/lgoyal6/amac/internal/event"
	"github.com/lgoyal6/amac/internal/supervisor"
)

// cmdRun starts a session, sends one prompt, and answers whatever the agent
// asks along the way. It is the smallest thing that exercises the full loop:
// spawn, handshake, session/new, session/prompt, session/update streaming, and
// session/request_permission answered by a human.
//
// -auto approves everything, for unattended verification. Interactive is the
// default because approving without reading is the failure this exists to
// prevent.
func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	agentName := fs.String("agent", "claude", "agent to run")
	dir := fs.String("dir", "", "working directory (default: cwd)")
	auto := fs.Bool("auto", false, "auto-approve every permission request")
	timeout := fs.Duration("timeout", 10*time.Minute, "overall timeout")
	dbPath := fs.String("db", defaultLogPath(), "event log path")
	if err := fs.Parse(args); err != nil {
		return err
	}

	prompt := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if prompt == "" {
		return fmt.Errorf("usage: amac run [-agent NAME] [-dir PATH] [-auto] <prompt...>")
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	sup := supervisor.New(log)
	defer sup.Shutdown()

	fmt.Printf("starting %s in %s\n", *agentName, workdir)
	start := time.Now()
	sess, err := sup.Start(ctx, *agentName, workdir)
	if err != nil {
		return err
	}
	fmt.Printf("session %s ready in %s (acp id %s)\n\n", sess.ID, time.Since(start).Round(time.Millisecond), sess.ACPID)

	// Watch the log rather than the session object: this is exactly the feed
	// the dashboard will consume, so driving the CLI from it proves the
	// subscription path works.
	events, unsub := log.Subscribe(256)
	defer unsub()
	go renderEvents(events, sess, *auto)

	res, err := sess.Prompt(ctx, prompt)
	if err != nil {
		return err
	}

	fmt.Printf("\nturn ended: %s\n", res.StopReason)
	head, _ := log.Head(context.Background())
	fmt.Printf("event log head=%d\n", head)
	return nil
}

func renderEvents(events <-chan event.Event, sess *supervisor.Session, auto bool) {
	in := bufio.NewReader(os.Stdin)
	for e := range events {
		switch e.Kind {
		case event.KindSessionUpdate:
			line := compact(string(e.Payload))
			if line != "" {
				fmt.Printf("  %s\n", line)
			}

		case event.KindPermissionRequested:
			p := sess.Pending()
			if p == nil {
				continue
			}
			fmt.Printf("\n  BLOCKED: %s\n", p.Title)
			for i, o := range p.Options {
				fmt.Printf("    %d) %-28s %s\n", i+1, o.OptionID, o.Name)
			}

			if auto {
				choice := preferred(p)
				fmt.Printf("  auto-approving: %s\n\n", choice)
				if err := sess.Answer(choice); err != nil {
					fmt.Printf("  answer failed: %v\n", err)
				}
				continue
			}

			fmt.Print("  choose (number, or blank to reject): ")
			text, _ := in.ReadString('\n')
			text = strings.TrimSpace(text)
			choice := ""
			for i, o := range p.Options {
				if text == fmt.Sprint(i+1) {
					choice = o.OptionID
				}
			}
			if err := sess.Answer(choice); err != nil {
				fmt.Printf("  answer failed: %v\n", err)
			}
			fmt.Println()

		case event.KindActuation:
			fmt.Printf("  [actuation] %s\n", compact(string(e.Payload)))
		}
	}
}

// preferred picks the least permissive affirmative option. Adapters name these
// differently, so match on kind first and fall back to the first option: the
// spec orders options with the conservative choice first.
func preferred(p *supervisor.Pending) string {
	for _, o := range p.Options {
		if o.Kind == "allow_once" {
			return o.OptionID
		}
	}
	if len(p.Options) > 0 {
		return p.Options[0].OptionID
	}
	return ""
}

func compact(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 160 {
		s = s[:160] + "..."
	}
	return s
}
