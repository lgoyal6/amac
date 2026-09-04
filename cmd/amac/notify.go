package main

// `amac notify` exists so nothing on this machine has to reach into agentmon to
// tell Laksh something.
//
// The two remaining callers, brew-autoupgrade and agents-sync, both sourced
// ~/.agentmon/lib.sh and called am_push_discord, which ends in `|| true`. That
// swallowed every delivery failure: when Discord was unreachable the job
// believed it had notified, and the only trace was a curl line in a log nobody
// reads. ~/.agentmon/logs/push.err is full of them.
//
// This returns the error instead. A caller that cannot deliver exits nonzero,
// which lands in its own marker line, which amac's health sweep already reads.
// A notification that did not arrive becomes a visible red rather than silence.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/lgoyal6/amac/internal/health"
)

func cmdNotify(args []string) error {
	fs := flag.NewFlagSet("notify", flag.ExitOnError)
	title := fs.String("title", "", "bold first line")
	dry := fs.Bool("dry", false, "print what would be sent and exit 0")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `amac notify [-title T] [-dry] <body...>

Sends one Discord message through amac's own delivery, and fails loudly when it
cannot. Reads the body from the arguments, or from stdin when none are given.
`)
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	body := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if body == "" {
		b, err := readAllStdin()
		if err != nil {
			return fmt.Errorf("read stdin: %w", err)
		}
		body = strings.TrimSpace(b)
	}
	if body == "" {
		return errors.New("nothing to send: pass a body or pipe one in")
	}

	msg := body
	if *title != "" {
		msg = "**" + *title + "**\n" + body
	}
	if *dry {
		fmt.Printf("--- notify (dry run, %d chars) ---\n%s\n", len(msg), msg)
		return nil
	}
	if err := health.Send(context.Background(), msg); err != nil {
		return fmt.Errorf("notify: %w", err)
	}
	return nil
}

func readAllStdin() (string, error) {
	info, err := os.Stdin.Stat()
	// No pipe means no body, rather than a read that blocks forever on a tty.
	if err != nil || info.Mode()&os.ModeCharDevice != 0 {
		return "", nil
	}
	b, err := os.ReadFile("/dev/stdin")
	return string(b), err
}
