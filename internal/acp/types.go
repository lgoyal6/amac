package acp

import "encoding/json"

// ProtocolVersion is a single integer naming the MAJOR version only. The
// client MUST send the latest version it supports; the agent answers with the
// version it will actually speak, which may be lower.
const ProtocolVersion = 1

// ---------------------------------------------------------------- JSON-RPC --

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  any             `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`

	// Present only on agent-initiated requests and notifications.
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
}

type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return e.Message }

// ---------------------------------------------------------------- initialize -

type ClientInfo struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version"`
}

type FSCapabilities struct {
	ReadTextFile  bool `json:"readTextFile"`
	WriteTextFile bool `json:"writeTextFile"`
}

type ClientCapabilities struct {
	FS       FSCapabilities `json:"fs"`
	Terminal bool           `json:"terminal"`
}

type InitializeParams struct {
	ProtocolVersion    int                `json:"protocolVersion"`
	ClientCapabilities ClientCapabilities `json:"clientCapabilities"`
	ClientInfo         ClientInfo         `json:"clientInfo"`
}

type AuthMethod struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type AgentInfo struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version"`
}

// AgentCapabilities is deliberately loose. Adapters disagree on what they
// advertise (codex-acp reports auth, providers, close, delete and its own
// _meta extensions; claude-agent-acp reports fork) and the spec says an
// omitted capability simply means unsupported. Keeping the raw JSON means a
// new capability never breaks the handshake, which is the whole point of
// speaking a versioned protocol instead of scraping a screen.
type InitializeResult struct {
	ProtocolVersion   int             `json:"protocolVersion"`
	AgentInfo         AgentInfo       `json:"agentInfo"`
	AgentCapabilities json.RawMessage `json:"agentCapabilities"`
	AuthMethods       []AuthMethod    `json:"authMethods"`
}

// Supports reports whether a dotted capability path is present and truthy,
// e.g. Supports("sessionCapabilities.resume") or Supports("loadSession").
// Presence counts as support: the spec models several capabilities as empty
// objects rather than booleans.
func (r InitializeResult) Supports(path string) bool {
	var cur any
	if err := json.Unmarshal(r.AgentCapabilities, &cur); err != nil {
		return false
	}
	start := 0
	for i := 0; i <= len(path); i++ {
		if i != len(path) && path[i] != '.' {
			continue
		}
		key := path[start:i]
		start = i + 1
		m, ok := cur.(map[string]any)
		if !ok {
			return false
		}
		cur, ok = m[key]
		if !ok {
			return false
		}
	}
	switch v := cur.(type) {
	case bool:
		return v
	case nil:
		return false
	default:
		return true
	}
}

// ---------------------------------------------------------------- sessions --

type NewSessionParams struct {
	CWD        string `json:"cwd"`
	MCPServers []any  `json:"mcpServers"`
}

type NewSessionResult struct {
	SessionID string `json:"sessionId"`
}

// ---------------------------------------------------------------- prompt ----

type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

func Text(s string) ContentBlock { return ContentBlock{Type: "text", Text: s} }

type PromptParams struct {
	SessionID string         `json:"sessionId"`
	Prompt    []ContentBlock `json:"prompt"`
}

// StopReason says how a turn ended. EndTurn is the only one that means the
// agent finished because it was done.
type StopReason string

const (
	StopEndTurn         StopReason = "end_turn"
	StopMaxTokens       StopReason = "max_tokens"
	StopMaxTurnRequests StopReason = "max_turn_requests"
	StopRefusal         StopReason = "refusal"
	StopCancelled       StopReason = "cancelled"
)

type PromptResult struct {
	StopReason StopReason `json:"stopReason"`
}

// ---------------------------------------------------------------- updates ---

// SessionUpdate discriminator values.
const (
	UpdateAgentMessageChunk = "agent_message_chunk"
	UpdateAgentThoughtChunk = "agent_thought_chunk"
	UpdateToolCall          = "tool_call"
	UpdateToolCallUpdate    = "tool_call_update"
	UpdatePlan              = "plan"
	UpdateUsage             = "usage_update"
	UpdateUserMessageChunk  = "user_message_chunk"
	UpdateAvailableCommands = "available_commands_update"
	UpdateCurrentModeUpdate = "current_mode_update"
)

type SessionNotification struct {
	SessionID string          `json:"sessionId"`
	Update    json.RawMessage `json:"update"`
}

// UpdateKind pulls the discriminator without committing to a shape. Adapters
// emit update variants this client has never heard of, and an unknown variant
// must be recorded rather than rejected.
type UpdateEnvelope struct {
	SessionUpdate string `json:"sessionUpdate"`
	Content       *struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content,omitempty"`
	ToolCallID string `json:"toolCallId,omitempty"`
	Title      string `json:"title,omitempty"`
	Kind       string `json:"kind,omitempty"`
	Status     string `json:"status,omitempty"`
}

// ---------------------------------------------------------------- permission -

type PermissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

type PermissionToolCall struct {
	ToolCallID string `json:"toolCallId"`
	Title      string `json:"title"`
	Kind       string `json:"kind,omitempty"`
}

type RequestPermissionParams struct {
	SessionID string             `json:"sessionId"`
	ToolCall  PermissionToolCall `json:"toolCall"`
	Options   []PermissionOption `json:"options"`
}

const (
	OutcomeSelected  = "selected"
	OutcomeCancelled = "cancelled"
)

type PermissionOutcome struct {
	Outcome  string `json:"outcome"`
	OptionID string `json:"optionId,omitempty"`
}

type RequestPermissionResult struct {
	Outcome PermissionOutcome `json:"outcome"`
}

// ---------------------------------------------------------------- fs --------

type ReadTextFileParams struct {
	SessionID string `json:"sessionId"`
	Path      string `json:"path"`
	Line      *int   `json:"line,omitempty"`
	Limit     *int   `json:"limit,omitempty"`
}

type ReadTextFileResult struct {
	Content string `json:"content"`
}

type WriteTextFileParams struct {
	SessionID string `json:"sessionId"`
	Path      string `json:"path"`
	Content   string `json:"content"`
}
