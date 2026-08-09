// Package supervisor owns agent sessions: it starts them, keeps their state,
// answers what they ask, and records everything they do into the event log.
//
// It is the piece that replaces reading a terminal. Session state here is not
// inferred from rendered output; it arrives as protocol messages, so "is this
// agent blocked" is a fact rather than a regex result.
package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lgoyal6/amac/internal/acp"
	"github.com/lgoyal6/amac/internal/event"
)

// State is what the dashboard renders. There are exactly four, and the one
// that matters is Blocked: it is set when the agent asks a question, and
// cleared when the question is answered. No guessing.
type State string

const (
	StateStarting State = "starting"
	StateIdle     State = "idle"
	StateWorking  State = "working"
	StateBlocked  State = "blocked"
	StateEnded    State = "ended"
)

// Pending is a question an agent is waiting on. It holds the reply channel, so
// answering is a direct handoff rather than a keystroke injected at a screen
// and hoped for.
type Pending struct {
	ToolCallID string
	Title      string
	Options    []acp.PermissionOption
	AskedAt    time.Time

	reply chan acp.PermissionOutcome
	once  sync.Once
}

// Session is one running agent.
type Session struct {
	ID      string
	Agent   string
	Dir     string
	ACPID   string
	Started time.Time

	mu      sync.RWMutex
	state   State
	detail  string
	pending *Pending

	client *acp.Client
	sup    *Supervisor
}

func (s *Session) State() (State, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state, s.detail
}

func (s *Session) Pending() *Pending {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.pending
}

func (s *Session) setState(st State, detail string) {
	s.mu.Lock()
	changed := s.state != st || s.detail != detail
	s.state, s.detail = st, detail
	s.mu.Unlock()
	if changed {
		s.sup.record(event.KindSessionUpdate, s.ID, map[string]any{
			"state": string(st), "detail": detail,
		})
	}
}

// Answer resolves a pending permission request. It is safe to call twice: the
// second call is a no-op rather than a panic on a closed channel, because the
// dashboard and the phone can both be looking at the same question.
func (s *Session) Answer(optionID string) error {
	s.mu.Lock()
	p := s.pending
	s.mu.Unlock()
	if p == nil {
		return fmt.Errorf("session %s has no pending question", s.ID)
	}

	valid := optionID == ""
	for _, o := range p.Options {
		if o.OptionID == optionID {
			valid = true
			break
		}
	}
	if !valid {
		return fmt.Errorf("option %q not offered; valid: %s", optionID, optionIDs(p.Options))
	}

	out := acp.PermissionOutcome{Outcome: acp.OutcomeSelected, OptionID: optionID}
	if optionID == "" {
		out = acp.PermissionOutcome{Outcome: acp.OutcomeCancelled}
	}
	p.once.Do(func() { p.reply <- out })
	return nil
}

func optionIDs(opts []acp.PermissionOption) string {
	ids := make([]string, len(opts))
	for i, o := range opts {
		ids[i] = o.OptionID
	}
	return strings.Join(ids, ", ")
}

// Prompt sends work and blocks until the turn ends.
func (s *Session) Prompt(ctx context.Context, text string) (acp.PromptResult, error) {
	s.setState(StateWorking, "prompted")
	s.sup.record(event.KindSessionUpdate, s.ID, map[string]any{"prompt": text})

	var res acp.PromptResult
	err := s.client.Call(ctx, "session/prompt", acp.PromptParams{
		SessionID: s.ACPID,
		Prompt:    []acp.ContentBlock{acp.Text(text)},
	}, &res)
	if err != nil {
		s.setState(StateIdle, "prompt failed: "+err.Error())
		return res, err
	}
	s.setState(StateIdle, "turn ended: "+string(res.StopReason))
	return res, nil
}

func (s *Session) Close() {
	s.setState(StateEnded, "closed")
	s.sup.record(event.KindSessionEnded, s.ID, map[string]any{"agent": s.Agent})
	_ = s.client.Close()
}

// ---------------------------------------------------------------- handler ---

// Handle answers everything the agent asks of us. This is the client half of
// ACP, and it is where amac's permission policy will eventually live.
func (s *Session) Handle(ctx context.Context, req acp.IncomingRequest) {
	switch req.Method {
	case "session/request_permission":
		s.handlePermission(ctx, req)

	case "fs/read_text_file":
		var p acp.ReadTextFileParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			_ = req.RespondErr(-32602, "bad params: "+err.Error())
			return
		}
		content, err := readTextFile(p)
		if err != nil {
			_ = req.RespondErr(-32000, err.Error())
			return
		}
		_ = req.Respond(acp.ReadTextFileResult{Content: content})

	case "fs/write_text_file":
		var p acp.WriteTextFileParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			_ = req.RespondErr(-32602, "bad params: "+err.Error())
			return
		}
		if !filepath.IsAbs(p.Path) {
			_ = req.RespondErr(-32602, "path must be absolute")
			return
		}
		if err := os.MkdirAll(filepath.Dir(p.Path), 0o755); err != nil {
			_ = req.RespondErr(-32000, err.Error())
			return
		}
		if err := os.WriteFile(p.Path, []byte(p.Content), 0o644); err != nil {
			_ = req.RespondErr(-32000, err.Error())
			return
		}
		s.sup.record(event.KindActuation, s.ID, map[string]any{"op": "write", "path": p.Path, "bytes": len(p.Content)})
		_ = req.Respond(nil)

	default:
		// Refuse explicitly. An agent that receives an error can report it;
		// one that receives silence waits forever.
		_ = req.RespondErr(-32601, "amac does not implement "+req.Method)
	}
}

func (s *Session) handlePermission(ctx context.Context, req acp.IncomingRequest) {
	var p acp.RequestPermissionParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		_ = req.RespondErr(-32602, "bad params: "+err.Error())
		return
	}

	pend := &Pending{
		ToolCallID: p.ToolCall.ToolCallID,
		Title:      p.ToolCall.Title,
		Options:    p.Options,
		AskedAt:    time.Now(),
		reply:      make(chan acp.PermissionOutcome, 1),
	}

	s.mu.Lock()
	s.pending = pend
	s.mu.Unlock()
	s.setState(StateBlocked, p.ToolCall.Title)
	s.sup.record(event.KindPermissionRequested, s.ID, map[string]any{
		"toolCallId": p.ToolCall.ToolCallID,
		"title":      p.ToolCall.Title,
		"options":    p.Options,
	})

	// Park until answered, the session dies, or the process shuts down. There
	// is deliberately no timeout: a question that auto-answers after N minutes
	// is a question that eventually approves something nobody read.
	var outcome acp.PermissionOutcome
	select {
	case outcome = <-pend.reply:
	case <-s.client.Done():
		outcome = acp.PermissionOutcome{Outcome: acp.OutcomeCancelled}
	case <-ctx.Done():
		outcome = acp.PermissionOutcome{Outcome: acp.OutcomeCancelled}
	}

	s.mu.Lock()
	s.pending = nil
	s.mu.Unlock()

	s.sup.record(event.KindPermissionAnswered, s.ID, map[string]any{
		"toolCallId": p.ToolCall.ToolCallID,
		"outcome":    outcome.Outcome,
		"optionId":   outcome.OptionID,
		"waited_ms":  time.Since(pend.AskedAt).Milliseconds(),
	})
	s.setState(StateWorking, "answered: "+outcome.OptionID)

	_ = req.Respond(acp.RequestPermissionResult{Outcome: outcome})
}

func readTextFile(p acp.ReadTextFileParams) (string, error) {
	if !filepath.IsAbs(p.Path) {
		return "", fmt.Errorf("path must be absolute: %s", p.Path)
	}
	b, err := os.ReadFile(p.Path)
	if err != nil {
		return "", err
	}
	if p.Line == nil && p.Limit == nil {
		return string(b), nil
	}
	// Line numbers are 1-based per the spec.
	lines := strings.Split(string(b), "\n")
	start := 0
	if p.Line != nil && *p.Line > 0 {
		start = *p.Line - 1
	}
	if start > len(lines) {
		return "", nil
	}
	end := len(lines)
	if p.Limit != nil && start+*p.Limit < end {
		end = start + *p.Limit
	}
	return strings.Join(lines[start:end], "\n"), nil
}
