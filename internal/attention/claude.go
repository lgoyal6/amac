package attention

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// Claude Code's hooks, which are the opposite shape to Codex's.
//
// Codex forced the design of everything above: one hook, firing only on
// agent-turn-complete, carrying no reason, so "this session is blocked" had to
// be recovered from a terminal bell and a four-second race between two
// signals. None of that is needed here. `Notification` fires exactly when
// Claude is waiting on a human and says what it is waiting for, and `Stop`
// fires when a turn ends. Each one means one thing, so neither is coalesced.
//
// What Claude does not hand over is the assistant's last message: Codex puts
// it in the payload, Claude puts the transcript path there instead. So the
// message is read back off disk. That file is written by the agent, not
// rendered by a terminal, which puts it in the same class of source as the ACP
// wire rather than in the class ACP was adopted to escape.

// Hook is the subset of a Claude Code hook payload amac reads. Every hook
// event carries the first four fields; the rest appear on one event each.
type Hook struct {
	HookEventName  string `json:"hook_event_name"`
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	CWD            string `json:"cwd"`

	Message string `json:"message"` // Notification: what it is waiting for
	Prompt  string `json:"prompt"`  // UserPromptSubmit
	Reason  string `json:"reason"`  // SessionEnd
}

// Session states a hook can establish. These are facts reported by the agent
// about itself, never inferences drawn from a rendered pane, which is the
// distinction the whole system rests on.
const (
	StateWorking = "working"
	StateIdle    = "idle"
	StateBlocked = "blocked"
	StateEnded   = "ended"
)

// Signal is one hook, decomposed into the two independent questions it answers:
// what the board should show, and whether the phone should ring. They are
// separate because most hooks answer only the first. A session that just
// started running a tool is worth showing and not worth interrupting anyone
// over, and conflating the two is how you end up with either a stale board or
// a muted channel.
type Signal struct {
	Notice Notice
	State  string // "" when this hook says nothing about state
	Notify bool
	Hook   Hook // the payload as received, for callers that need its context
}

// FromHook maps a Claude Code hook payload onto a signal.
//
// An unrecognised event is not an error: Claude Code adds hook events between
// releases, and a new one must degrade to "nothing to say" rather than to a
// hook that exits non-zero and makes the agent look broken to its user.
func FromHook(payload []byte) (Signal, error) {
	var h Hook
	if err := json.Unmarshal(payload, &h); err != nil {
		return Signal{}, fmt.Errorf("claude hook payload: %w", err)
	}

	n := Notice{Agent: "claude", Message: strings.TrimSpace(h.Message)}
	s := Signal{Notice: n, Hook: h}

	switch h.HookEventName {
	case "Notification":
		// The one signal Codex cannot produce at all: waiting on a human, with
		// the reason attached.
		s.State, s.Notify = StateBlocked, true
		s.Notice.Reason = WantsYou
		if s.Notice.Message == "" {
			s.Notice.Message = "asked for you"
		}

	case "Stop":
		s.State, s.Notify = StateIdle, true
		s.Notice.Reason = TurnComplete
		s.Notice.Message = LastAssistantMessage(h.TranscriptPath)

	case "UserPromptSubmit":
		s.State = StateWorking

	case "PostToolUse":
		// The only thing that clears `blocked` before the turn ends. Without
		// it a session shows as waiting on you for the whole run after you
		// have already approved the tool, which is precisely the confidently
		// wrong state this system exists to stop producing.
		s.State = StateWorking

	case "SessionStart":
		s.State = StateIdle

	case "SessionEnd":
		s.State = StateEnded
	}

	return s, nil
}

// transcriptTail bounds how much of a transcript is read. Sessions here run
// for hours and their JSONL reaches tens of megabytes; the last assistant
// message is always at the end, so reading the whole file would cost more
// every hour of a session's life for no gain.
const transcriptTail = 512 << 10

// LastAssistantMessage returns the text of the newest assistant turn in a
// Claude Code transcript.
//
// Sidechain entries are skipped. Those are subagent turns, and a subagent's
// closing message is not what the session said to you: reporting one would
// attribute a Task tool's summary to the session that spawned it, which reads
// as the agent having answered a question nobody asked.
func LastAssistantMessage(path string) string {
	if path == "" {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return ""
	}
	start := fi.Size() - transcriptTail
	if start < 0 {
		start = 0
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return ""
	}
	buf, err := io.ReadAll(f)
	if err != nil {
		return ""
	}

	lines := strings.Split(string(buf), "\n")
	if start > 0 && len(lines) > 0 {
		// The seek almost certainly landed mid-record, and half a JSON object
		// is not a parse failure worth reporting, it is a line to drop.
		lines = lines[1:]
	}

	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" || !strings.Contains(line, `"assistant"`) {
			continue
		}
		var rec struct {
			Type        string `json:"type"`
			IsSidechain bool   `json:"isSidechain"`
			Message     struct {
				Role    string `json:"role"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		if rec.Type != "assistant" || rec.IsSidechain {
			continue
		}
		var sb strings.Builder
		for _, c := range rec.Message.Content {
			if c.Type == "text" {
				sb.WriteString(c.Text)
			}
		}
		// A turn that ended in a tool call has no text of its own. Keep
		// walking back rather than reporting an empty message as the answer.
		if text := strings.TrimSpace(sb.String()); text != "" {
			return text
		}
	}
	return ""
}
