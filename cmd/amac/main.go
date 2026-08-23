// Command amac is the control plane for agent sessions on this machine.
//
// Phase 1 scope: prove that one client can drive heterogeneous agents over
// ACP, and that everything it learns lands in the event log. Supervisor,
// router, dashboard and sensors all hang off these two pieces.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/lgoyal6/amac/internal/acp"
	"github.com/lgoyal6/amac/internal/agent"
	"github.com/lgoyal6/amac/internal/event"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	var err error
	switch cmd {
	case "setup":
		err = cmdSetup(args)
	case "models":
		err = cmdModels(args)
	case "route":
		err = cmdRoute(args)
	case "eval":
		err = cmdEval(args)
	case "do":
		err = cmdDo(args)
	case "observe":
		err = cmdWatch(args)
	case "learn":
		err = cmdLearn(args)
	case "apply":
		err = cmdApply(args)
	case "cost":
		err = cmdCost(args)
	case "health":
		err = cmdHealth(args)
	case "daemon":
		err = cmdDaemon(args)
	case "run":
		err = cmdRun(args)
	case "probe":
		err = cmdProbe(args)
	case "log":
		err = cmdLog(args)
	case "agents":
		for _, n := range agent.Names() {
			a, _ := agent.Get(n)
			fmt.Printf("%-8s %s\n         %s\n", a.Name, strings.Join(a.Argv(), " "), a.Note)
		}
	case "help", "-h", "--help":
		usage()
	default:
		err = fmt.Errorf("unknown command %q", cmd)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "amac: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `amac - control plane for agent sessions

  amac setup
        Install the pinned ACP adapters locally (run once).
  amac daemon [-port N] [-localhost]
        Run the control plane and dashboard (tailnet only).
  amac run [-agent NAME] [-dir PATH] [-auto] <prompt...>
        Start a session, send a prompt, answer what it asks.
  amac probe [-agent claude|codex] [-all] [-dir PATH]
        Handshake with an agent adapter and record what it can do.
  amac do [-size solo|pair|team] [-budget USD] <task...>
        Triage a task and run the right sized team of agents.
  amac route [-dry] <prompt...>
        Route one prompt through the cascade.
  amac eval [-tasks FILE]
        Measure cost and quality per tier. Run this before trusting the router.
  amac models
        Which model providers are configured.
  amac observe
        Watch allowlisted apps (metadata only, default deny).
  amac learn [-days N]
        Patterns in the log, and automations worth adding.
  amac apply -email FILE | -company X -role Y
        Record a job application.
  amac cost [-days N]
        What sessions have cost, from the event log.
  amac health [-digest|-alert] [-quiet]
        Check every declared automation and record the sweep.
        -alert DMs only what changed; -digest DMs the whole roster.
  amac log [-n N] [-since SEQ]
        Show recent events.
  amac agents
        List known adapters.
`)
}

func defaultLogPath() string {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".amac")
	_ = os.MkdirAll(dir, 0o755)
	return filepath.Join(dir, "events.db")
}

// ---------------------------------------------------------------- setup -----

// cmdSetup installs the pinned adapters once into a shared directory. Session
// spawn then execs a local binary instead of going through `npx`, which
// re-resolves against the npm registry every single time: measured 8.6s versus
// 370ms for the same handshake, and an outright hang when the registry was
// slow. Process startup must not depend on the network.
func cmdSetup(args []string) error {
	fs := flag.NewFlagSet("setup", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

	dir := agent.AdapterDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, "package.json")); os.IsNotExist(err) {
		if err := os.WriteFile(filepath.Join(dir, "package.json"),
			[]byte(`{"name":"amac-adapters","private":true}`+"\n"), 0o644); err != nil {
			return err
		}
	}

	pkgs := agent.Packages()
	fmt.Printf("installing %d adapters into %s\n", len(pkgs), dir)
	for _, p := range pkgs {
		fmt.Printf("  %s\n", p)
	}

	cmd := exec.Command("npm", append([]string{"install", "--silent"}, pkgs...)...)
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("npm install: %w", err)
	}

	for _, n := range agent.Names() {
		a, _ := agent.Get(n)
		status := "MISSING"
		if a.Local() {
			status = "ok"
		}
		fmt.Printf("  %-8s %s\n", n, status)
	}
	return nil
}

// ---------------------------------------------------------------- probe -----

func cmdProbe(args []string) error {
	fs := flag.NewFlagSet("probe", flag.ExitOnError)
	name := fs.String("agent", "claude", "agent to probe")
	all := fs.Bool("all", false, "probe every known agent")
	dir := fs.String("dir", "", "working directory for the agent (default: cwd)")
	timeout := fs.Duration("timeout", 90*time.Second, "handshake timeout")
	dbPath := fs.String("db", defaultLogPath(), "event log path")
	if err := fs.Parse(args); err != nil {
		return err
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

	targets := []string{*name}
	if *all {
		targets = agent.Names()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var failed int
	for _, t := range targets {
		if err := probeOne(ctx, log, t, workdir, *timeout); err != nil {
			fmt.Printf("  %-8s FAILED  %v\n\n", t, err)
			failed++
			continue
		}
	}

	n, _ := log.Count(ctx)
	head, _ := log.Head(ctx)
	fmt.Printf("event log: %s (%d events, head=%d)\n", *dbPath, n, head)

	if failed > 0 {
		return fmt.Errorf("%d of %d agents failed", failed, len(targets))
	}
	return nil
}

func probeOne(ctx context.Context, log *event.Log, name, workdir string, timeout time.Duration) error {
	a, err := agent.Get(name)
	if err != nil {
		return err
	}

	fmt.Printf("%s\n  spawn   %s\n", name, strings.Join(a.Argv(), " "))
	start := time.Now()

	// The adapter's own stderr is diagnostics, not protocol. Keep it out of
	// the parser and out of the operator's way unless something breaks.
	var stderr io.Writer = io.Discard
	if os.Getenv("AMAC_DEBUG") != "" {
		stderr = os.Stderr
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	c, err := acp.Spawn(runCtx, name, a.Argv(), workdir, stderr)
	if err != nil {
		return err
	}
	defer c.Close()

	res, err := c.Initialize(runCtx)
	if err != nil {
		return err
	}
	elapsed := time.Since(start)

	fmt.Printf("  agent   %s v%s\n", res.AgentInfo.Title, res.AgentInfo.Version)
	fmt.Printf("  proto   v%d  (handshake %s)\n", res.ProtocolVersion, elapsed.Round(time.Millisecond))

	// Capability probes are how the supervisor will later decide what it may
	// ask of a given agent, so print the ones that change behaviour.
	for _, cap := range []string{"loadSession", "sessionCapabilities.resume", "sessionCapabilities.list", "promptCapabilities.image"} {
		fmt.Printf("  %-28s %v\n", cap, res.Supports(cap))
	}
	if len(res.AuthMethods) > 0 {
		var ids []string
		for _, m := range res.AuthMethods {
			ids = append(ids, m.ID)
		}
		fmt.Printf("  auth    %s\n", strings.Join(ids, ", "))
	}

	ev, err := event.New(event.KindSessionStarted, "probe", "", map[string]any{
		"agent":        name,
		"adapter":      res.AgentInfo.Name,
		"version":      res.AgentInfo.Version,
		"protocol":     res.ProtocolVersion,
		"handshake_ms": elapsed.Milliseconds(),
		"capabilities": json.RawMessage(res.AgentCapabilities),
		"cwd":          workdir,
	})
	if err != nil {
		return err
	}
	stored, err := log.Append(ctx, ev)
	if err != nil {
		return err
	}
	fmt.Printf("  event   seq=%d %s\n\n", stored.Seq, stored.Kind)
	return nil
}

// ---------------------------------------------------------------- log -------

func cmdLog(args []string) error {
	fs := flag.NewFlagSet("log", flag.ExitOnError)
	n := fs.Int("n", 20, "max events")
	since := fs.Int64("since", 0, "only events after this seq")
	dbPath := fs.String("db", defaultLogPath(), "event log path")
	if err := fs.Parse(args); err != nil {
		return err
	}

	log, err := event.Open(*dbPath, event.Full)
	if err != nil {
		return err
	}
	defer log.Close()

	ctx := context.Background()
	head, err := log.Head(ctx)
	if err != nil {
		return err
	}
	from := *since
	if from == 0 && head > int64(*n) {
		from = head - int64(*n)
	}

	events, err := log.Since(ctx, from, *n)
	if err != nil {
		return err
	}
	if len(events) == 0 {
		fmt.Println("no events")
		return nil
	}
	for _, e := range events {
		payload := string(e.Payload)
		if len(payload) > 96 {
			payload = payload[:96] + "..."
		}
		fmt.Printf("%5d  %s  %-16s %-10s %s\n",
			e.Seq, e.At.Local().Format("15:04:05"), e.Kind, e.Source, payload)
	}
	return nil
}
