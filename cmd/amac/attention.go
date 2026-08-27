package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/lgoyal6/amac/internal/attention"
	"github.com/lgoyal6/amac/internal/event"
)

// cmdAttention is the entry point every agent's signals land on.
//
// Codex allows exactly one `notify` program and it fires only on
// agent-turn-complete, so a request for approval never reaches a program that
// way. The terminal bell does carry it, which is why there are two callers for
// Codex: the notify hook, which knows what happened and what was said, and a
// tmux bell hook, which knows only that something wants attention.
//
// Claude Code needs neither trick. Its hooks say what they mean, so -claude
// takes the payload on stdin and maps it directly. It also reports states
// nobody should be interrupted over, which is why this can record a state
// change without sending anything.
func cmdAttention(args []string) error {
	fs := flag.NewFlagSet("attention", flag.ExitOnError)
	codexJSON := fs.String("codex", "", "Codex notify payload (its final argv element)")
	claudeHook := fs.Bool("claude", false, "called from a Claude Code hook: payload on stdin")
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

	// Most Claude hooks are worth showing and not worth interrupting anyone
	// over, so delivery and state are decided separately from here down.
	notify, state, fallbackName := true, "", ""

	if *claudeHook {
		payload, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("read hook payload: %w", err)
		}
		sig, err := attention.FromHook(payload)
		if err != nil {
			return err
		}
		n, notify, state = sig.Notice, sig.Notify, sig.State
		if n.Session == "" {
			n.Session = *session
		}
		// A Claude session started outside tmux has no session name to
		// inherit. Its directory is the name he would use for it anyway, and
		// the prefix keeps it from colliding with a real tmux session.
		if sig.Hook.CWD != "" {
			fallbackName = "claude-" + filepath.Base(sig.Hook.CWD)
		}
	}

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
		n.Session = fallbackName
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

	if state != "" {
		wrote, err := attention.RecordState(ctx, log, attention.State{
			Session: n.Session, Agent: n.Agent, State: state,
			Detail: firstLine(n.Message, 160),
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "amac attention: record state: %v\n", err)
		}
		if !*quiet && !notify {
			verb := "state"
			if !wrote {
				verb = "state unchanged"
			}
			fmt.Printf("%s: %s (%s)\n", verb, n.Session, state)
		}
	}
	if !notify {
		// Nothing here wants the human. The board has what it needs.
		return nil
	}

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

// firstLine trims a message to something that fits on a session card. The full
// text is in the event either way; this is the line on the board.
func firstLine(s string, max int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if len(s) > max {
		s = s[:max] + "\u2026"
	}
	return s
}
