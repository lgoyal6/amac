package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/lgoyal6/amac/internal/daemon"
	"github.com/lgoyal6/amac/internal/event"
	"github.com/lgoyal6/amac/internal/model"
	"github.com/lgoyal6/amac/internal/orchestrator"
	"github.com/lgoyal6/amac/internal/router"
	"github.com/lgoyal6/amac/internal/supervisor"
)

func cmdDaemon(args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	port := fs.Int("port", 7788, "listen port")
	dbPath := fs.String("db", defaultLogPath(), "event log path")
	wait := fs.Duration("wait-tailnet", 2*time.Minute, "how long to wait for Tailscale before giving up")
	localhost := fs.Bool("localhost", false, "bind 127.0.0.1 instead of the tailnet (local testing only)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Bind resolution is fail-closed. This daemon starts agents, approves
	// their tool calls and writes files; there is no version of exposing it on
	// 0.0.0.0 that is acceptable, so a missing tailnet is a startup failure
	// rather than a fallback. -localhost exists for tests on this machine only.
	var host string
	if *localhost {
		host = "127.0.0.1"
	} else {
		// Said before the wait, not after. Under launchd this is the whole
		// log for two minutes, and an empty log while a process sits there
		// looks exactly like a hang with no cause.
		fmt.Printf("waiting up to %s for the tailnet...\n", *wait)
		ip, err := daemon.WaitForTailnet(*wait)
		if err != nil {
			return fmt.Errorf("refusing to start without a tailnet address: %w", err)
		}
		host = ip
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

	srv := &http.Server{
		Addr:              net.JoinHostPort(host, fmt.Sprint(*port)),
		Handler:           daemon.New(sup, log, orch, token).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: /api/stream is a long-lived SSE connection and a
		// write deadline would sever it on a fixed schedule. Idle connections
		// are handled by the keepalive frame instead.
		IdleTimeout: 120 * time.Second,
	}

	head, _ := log.Head(context.Background())
	fmt.Printf("amac daemon\n")
	fmt.Printf("  dashboard  http://%s:%d/?token=%s\n", host, *port, token)
	fmt.Printf("  events     %s (head=%d)\n", *dbPath, head)
	fmt.Printf("  bind       %s (%s)\n\n", host, bindNote(*localhost))

	errc := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	fmt.Println("\nshutting down")
	// Stop agents first: they are child processes, and leaving them orphaned
	// would leak both processes and their API spend.
	sup.Shutdown()
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutCtx)
}

func bindNote(localhost bool) string {
	if localhost {
		return "LOCAL ONLY - not reachable from your phone"
	}
	return "tailnet only"
}
