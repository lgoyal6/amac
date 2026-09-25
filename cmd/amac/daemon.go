package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/lgoyal6/amac/internal/daemon"
	"github.com/lgoyal6/amac/internal/event"
	"github.com/lgoyal6/amac/internal/model"
	"github.com/lgoyal6/amac/internal/orchestrator"
	"github.com/lgoyal6/amac/internal/queue"
	"github.com/lgoyal6/amac/internal/router"
	"github.com/lgoyal6/amac/internal/supervisor"
)

func cmdDaemon(args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	port := fs.Int("port", 7788, "listen port")
	dbPath := fs.String("db", defaultLogPath(), "event log path")
	localhost := fs.Bool("localhost", false, "serve this machine only; never bind the tailnet")
	if err := fs.Parse(args); err != nil {
		return err
	}

	token, err := daemon.Token()
	if err != nil {
		return err
	}

	log, err := event.Open(*dbPath, event.Full)
	if err != nil {
		return err
	}
	defer log.Close()

	sup := supervisor.New(log)
	// The board can convene the org, so the daemon carries an orchestrator.
	// A missing model key is not fatal: triage falls back to heuristics, and a
	// dashboard that refuses to start because a grading model is unreachable
	// would be trading the whole feature for one of its parts.
	reg, _ := model.FromEnv()
	orch := orchestrator.New(sup, router.New(reg, log), log)

	q, err := queue.Open(log)
	if err != nil {
		return err
	}

	api := daemon.New(sup, log, orch, q, token)
	srv := &http.Server{
		Handler:           api.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: /api/stream is a long-lived SSE connection and a
		// write deadline would sever it on a fixed schedule. Idle connections
		// are handled by the keepalive frame instead.
		IdleTimeout: 120 * time.Second,
	}

	// codex-relay binds loopback and refuses anything else, so the only way to open
	// its dashboard from a phone is to carry it across on this machine. It listens
	// separately because the page it serves asks for /api at the root, which is a
	// path amac already answers.
	relay := &http.Server{
		Handler:           api.RelayProxy(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	binder := &daemon.Binder{
		Servers:  map[int]*http.Server{*port: srv, daemon.RelayProxyPort: relay},
		Announce: func(msg string) { fmt.Printf("  %s\n", msg) },
	}
	if *localhost {
		// Asked for this machine only, so do not go looking for the tailnet at all.
		binder.LookupTailnet = func() (string, error) {
			return "", fmt.Errorf("-localhost was given")
		}
	}
	if err := binder.Start(ctx); err != nil {
		return err
	}

	head, _ := log.Head(context.Background())
	fmt.Printf("amac daemon\n")
	fmt.Printf("  dashboard  http://127.0.0.1:%d/?token=%s\n", *port, token)
	fmt.Printf("  relay      %s\n", daemon.RelayProxyURL("127.0.0.1"))
	fmt.Printf("  events     %s (head=%d)\n", *dbPath, head)
	fmt.Printf("  bind       %s\n\n", bindNote(*localhost))

	// The jobs tab reads a local cache that nothing but the sync button moved.
	go api.SyncNotionPeriodically(ctx)

	// Losing a listener is no longer a reason to exit. Loopback failing is caught
	// at startup, and a tailnet address that comes and goes is now an expected
	// thing the binder absorbs rather than a fault that ends the process.
	<-ctx.Done()

	fmt.Println("\nshutting down")
	// Stop agents first: they are child processes, and leaving them orphaned
	// would leak both processes and their API spend.
	sup.Shutdown()
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = relay.Shutdown(shutCtx)
	return srv.Shutdown(shutCtx)
}

func bindNote(localhost bool) string {
	if localhost {
		return "127.0.0.1 only - not reachable from your phone"
	}
	return "127.0.0.1 always, plus the tailnet whenever Tailscale is up"
}

// cmdURL prints the dashboard link.
//
// Getting the link was harder than it should be: the daemon prints it once at
// startup into a launchd log, and assembling it by hand means knowing the port,
// finding the tailnet address and catting the token. That is three steps to put
// a URL on a phone, and the phone is the whole point.
func cmdURL(args []string) error {
	fs := flag.NewFlagSet("url", flag.ExitOnError)
	port := fs.Int("port", 7788, "port the daemon is on")
	if err := fs.Parse(args); err != nil {
		return err
	}

	token, err := daemon.Token()
	if err != nil {
		return err
	}
	ip, err := daemon.TailnetIP()
	if err != nil {
		// The address, not a guess at one. Printing a localhost link here would
		// hand back something that cannot work from a phone, which is the only
		// device that needs this command.
		return fmt.Errorf("%w\n\nThe board is tailnet-only. Connect Tailscale and run this again", err)
	}

	fmt.Printf("http://%s:%d/?token=%s\n", ip, *port, token)
	return nil
}
