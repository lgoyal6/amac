package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/lgoyal6/amac/internal/acp"
	"github.com/lgoyal6/amac/internal/agent"
	"github.com/lgoyal6/amac/internal/event"
)

// Supervisor owns every live session on this machine.
type Supervisor struct {
	log *event.Log

	mu       sync.RWMutex
	sessions map[string]*Session
	seq      int
}

func New(log *event.Log) *Supervisor {
	return &Supervisor{log: log, sessions: make(map[string]*Session)}
}

// record is fire-and-forget on purpose. Losing an observability event must
// never fail the operation that produced it, and the failure is reported
// rather than swallowed.
func (s *Supervisor) record(kind event.Kind, session string, payload any) {
	ev, err := event.New(kind, "supervisor", session, payload)
	if err == nil {
		_, err = s.log.Append(context.Background(), ev)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "amac: event log write failed (%s): %v\n", kind, err)
	}
}

// Start spawns an agent, handshakes, and opens a session in dir.
func (s *Supervisor) Start(ctx context.Context, agentName, dir string) (*Session, error) {
	a, err := agent.Get(agentName)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	s.seq++
	id := fmt.Sprintf("%s-%d", agentName, s.seq)
	s.mu.Unlock()

	var stderr io.Writer = io.Discard
	if os.Getenv("AMAC_DEBUG") != "" {
		stderr = os.Stderr
	}

	client, err := acp.Spawn(ctx, agentName, a.Argv(), dir, stderr)
	if err != nil {
		return nil, fmt.Errorf("spawn %s: %w", agentName, err)
	}

	sess := &Session{
		ID: id, Agent: agentName, Dir: dir, Started: time.Now(),
		state: StateStarting, client: client, sup: s,
	}
	// The handler must be installed before any call that can make the agent
	// ask us something, which initialize can.
	client.SetHandler(sess)

	init, err := client.Initialize(ctx)
	if err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("initialize %s: %w", agentName, err)
	}

	var ns acp.NewSessionResult
	err = client.Call(ctx, "session/new", acp.NewSessionParams{CWD: dir, MCPServers: []any{}}, &ns)
	if err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("session/new on %s: %w", agentName, err)
	}
	sess.ACPID = ns.SessionID

	s.mu.Lock()
	s.sessions[id] = sess
	s.mu.Unlock()

	s.record(event.KindSessionStarted, id, map[string]any{
		"agent": agentName, "adapter": init.AgentInfo.Name, "version": init.AgentInfo.Version,
		"protocol": init.ProtocolVersion, "acpSessionId": ns.SessionID, "cwd": dir,
	})
	sess.setState(StateIdle, "ready")

	go sess.consume()
	go sess.watchExit()
	return sess, nil
}

// consume drains session/update notifications into the event log. This is the
// stream that a terminal renders and then throws away; keeping it is what
// makes the dashboard, the miner and replay possible.
func (s *Session) consume() {
	for n := range s.client.Notifications() {
		if n.Method != "session/update" {
			s.sup.record(event.KindSessionUpdate, s.ID, map[string]any{"method": n.Method})
			continue
		}
		var sn acp.SessionNotification
		if err := json.Unmarshal(n.Params, &sn); err != nil {
			continue
		}
		var env acp.UpdateEnvelope
		_ = json.Unmarshal(sn.Update, &env)

		payload := map[string]any{"update": env.SessionUpdate}
		switch env.SessionUpdate {
		case acp.UpdateToolCall, acp.UpdateToolCallUpdate:
			payload["toolCallId"] = env.ToolCallID
			payload["title"] = env.Title
			payload["status"] = env.Status
			if env.Status != "" && env.Title != "" {
				s.setState(StateWorking, env.Title)
			}
		case acp.UpdateAgentMessageChunk, acp.UpdateAgentThoughtChunk:
			if env.Content != nil {
				payload["text"] = env.Content.Text
			}
		default:
			// Unknown variants are recorded whole rather than dropped: an
			// adapter gaining a new update type must not create a blind spot.
			payload["raw"] = json.RawMessage(sn.Update)
		}
		s.sup.record(event.KindSessionUpdate, s.ID, payload)
	}
}

// watchExit turns process death into a state change, so a crashed agent shows
// as ended rather than sitting forever as working.
func (s *Session) watchExit() {
	<-s.client.Done()
	detail := "agent exited"
	if err := s.client.Err(); err != nil {
		detail = "agent failed: " + err.Error()
	}
	s.setState(StateEnded, detail)

	s.sup.mu.Lock()
	delete(s.sup.sessions, s.ID)
	s.sup.mu.Unlock()
}

func (s *Supervisor) Get(id string) (*Session, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.sessions[id]
	return sess, ok
}

func (s *Supervisor) List() []*Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		out = append(out, sess)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Blocked returns sessions waiting on a human. This is the query the whole
// system exists to answer, and it is now a map lookup rather than a regex over
// a screenshot.
func (s *Supervisor) Blocked() []*Session {
	var out []*Session
	for _, sess := range s.List() {
		if st, _ := sess.State(); st == StateBlocked {
			out = append(out, sess)
		}
	}
	return out
}

func (s *Supervisor) Shutdown() {
	for _, sess := range s.List() {
		sess.Close()
	}
}
