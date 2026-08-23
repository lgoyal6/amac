package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/lgoyal6/amac/internal/attention"
	"github.com/lgoyal6/amac/internal/event"
)

// cmdAttention is the entry point both of Codex's signals land on.
//
// Codex allows exactly one `notify` program and it fires only on
// agent-turn-complete, so a request for approval never reaches a program that
// way. The terminal bell does carry it, which is why there are two callers:
// the notify hook, which knows what happened and what was said, and a tmux
// bell hook, which knows only that something wants attention.
func cmdAttention(args []string) error {
	fs := flag.NewFlagSet("attention", flag.ExitOnError)
	codexJSON := fs.String("codex", "", "Codex notify payload (its final argv element)")
	bell := fs.Bool("bell", false, "called from the tmux bell hook: unknown reason, coalesces")
	session := fs.String("session", "", "tmux session (default: the caller's)")
	agent := fs.String("agent", "codex", "which agent")
	reason := fs.String("reason", attention.WantsYou, "turn-complete | wants-attention")
	message := fs.String("message", "", "what the agent last said")
	// Long enough for the notify hook to overtake a bell fired by the same
	// turn, short enough that a real approval request still feels immediate.
	coalesce := fs.Duration("coalesce", 0, "wait this long before deciding")
	dbPath := fs.String("db", defaultLogPath(), "event log path")
	quiet := fs.Bool("quiet", false, "no stdout, for hooks")
	if err := fs.Parse(args); err != nil {
		return err
	}

	n := attention.Notice{Session: *session, Agent: *agent, Reason: *reason, Message: *message}

	if *codexJSON != "" {
		// Codex hands the event as JSON. Only agent-turn-complete exists
		// today, but keying off the field rather than assuming keeps this
		// honest if that changes.
		var ev struct {
			Type                 string `json:"type"`
			LastAssistantMessage string `json:"last-assistant-message"`
		}
		if err := json.Unmarshal([]byte(*codexJSON), &ev); err != nil {
			return fmt.Errorf("codex payload: %w", err)
		}
		n.Reason = attention.TurnComplete
		if ev.Type != "" && ev.Type != "agent-turn-complete" {
			n.Reason = ev.Type
		}
		if n.Message == "" {
			n.Message = ev.LastAssistantMessage
		}
	}
	if *bell {
		n.Reason = attention.WantsYou
		if *coalesce == 0 {
			*coalesce = 4 * time.Second
		}
	}
	if n.Session == "" {
		n.Session = callerSession()
	}
	if n.Session == "" {
		// Outside tmux there is no session to name and no pane to return to.
		// Still worth telling him, under the agent's own name.
		n.Session = n.Agent
	}

	log, err := event.Open(*dbPath, event.Full)
	if err != nil {
		return err
	}
	defer log.Close()

	// Generous: the coalesce wait plus a Discord round trip.
	ctx, cancel := context.WithTimeout(context.Background(), *coalesce+30*time.Second)
	defer cancel()

	out, err := attention.Handle(ctx, log, n, *coalesce)
	if !*quiet {
		if out.Sent {
			fmt.Printf("sent: %s (%s)\n", n.Session, n.Reason)
		} else {
			fmt.Printf("held: %s (%s)\n", n.Session, out.Why)
		}
	}
	// A hook that exits non-zero makes the agent look broken to its user. The
	// failure is already in the log and on stderr.
	if err != nil {
		fmt.Fprintf(os.Stderr, "amac attention: %v\n", err)
	}
	return nil
}

// callerSession asks tmux which session the calling process sits in.
func callerSession() string {
	pane := os.Getenv("TMUX_PANE")
	if pane == "" {
		return ""
	}
	out, err := exec.Command("tmux", "display-message", "-pt", pane, "#S").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
