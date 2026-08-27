// Package mcp lets the agents ask amac what is going on.
//
// Everything else here points one way: amac watches agents and reports to a
// human. This turns it around. An agent running in a repo currently has no idea
// whether another agent is already editing the same tree, whether the pipeline
// whose output it is about to trust delivered this morning, or that the thing
// it just found and cannot fix could be written down somewhere a human will
// see. It asks the person, who is on their phone, and the answer arrives twenty
// minutes later or not at all.
//
// The tools are read-mostly on purpose. Two of them write, and both are
// additive: file a task, post a heartbeat. Nothing here stops a session or
// answers a permission request, because an agent approving another agent's tool
// call is a loop nobody asked for and the entire reason permission prompts
// reach a human at all.
//
// Transport is line-delimited JSON-RPC over stdio, which internal/acp also
// speaks. It is not shared: that transport is unexported and shaped for a
// client, and exporting it so a server could borrow fifty lines would couple
// the load-bearing ACP client to something unrelated to it.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
)

// protocolVersion is the revision this speaks. Stated rather than echoed back
// from the client: claiming to support whatever was asked for is how a version
// mismatch turns into a confusing failure three calls later.
const protocolVersion = "2025-06-18"

type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	// Handler returns text. Everything an agent reads is text, and returning
	// structured data it would have to re-serialise to read is a shape chosen
	// for the wire rather than for the reader.
	Handler func(ctx context.Context, args json.RawMessage) (string, error) `json:"-"`
}

type Server struct {
	tools []Tool
	name  string

	mu  sync.Mutex
	out *bufio.Writer
}

func NewServer(name string, tools []Tool) *Server {
	return &Server{name: name, tools: tools}
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// Serve runs until the input closes, which is how a stdio server is told to
// stop: the client exits and the pipe ends.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	s.out = bufio.NewWriter(w)
	in := bufio.NewScanner(r)
	// A tool result can carry a diff or a session list. The default 64KB is
	// enough for what this returns, and an explicit limit means an oversized
	// line is an error rather than a silently truncated one.
	in.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	for in.Scan() {
		line := strings.TrimSpace(in.Text())
		if line == "" {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			s.reply(rpcResponse{JSONRPC: "2.0", Error: &rpcError{Code: -32700, Message: "parse error"}})
			continue
		}
		// A notification has no id and takes no response. Replying to one is a
		// protocol error that some clients tolerate and others do not.
		if len(req.ID) == 0 {
			continue
		}
		s.reply(s.handle(ctx, req))
	}
	return in.Err()
}

func (s *Server) reply(resp rpcResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.Marshal(resp)
	if err != nil {
		return
	}
	s.out.Write(b)
	s.out.WriteByte('\n')
	s.out.Flush()
}

func (s *Server) handle(ctx context.Context, req rpcRequest) rpcResponse {
	out := rpcResponse{JSONRPC: "2.0", ID: req.ID}

	switch req.Method {
	case "initialize":
		out.Result = map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": s.name, "version": "1"},
		}

	case "tools/list":
		list := make([]Tool, len(s.tools))
		copy(list, s.tools)
		out.Result = map[string]any{"tools": list}

	case "tools/call":
		var call struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &call); err != nil {
			out.Error = &rpcError{Code: -32602, Message: err.Error()}
			return out
		}
		for _, t := range s.tools {
			if t.Name != call.Name {
				continue
			}
			text, err := t.Handler(ctx, call.Arguments)
			if err != nil {
				// isError rather than a protocol error: the call reached the
				// tool and the tool has something to say. A JSON-RPC error
				// tells the agent the server broke, which sends it to retry
				// or give up instead of reading what went wrong.
				out.Result = content(err.Error(), true)
				return out
			}
			out.Result = content(text, false)
			return out
		}
		out.Error = &rpcError{Code: -32602, Message: "no such tool: " + call.Name}

	default:
		out.Error = &rpcError{Code: -32601, Message: "method not found: " + req.Method}
	}
	return out
}

func content(text string, isErr bool) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isErr,
	}
}

// Schema is a small helper so a tool's inputs read as a list rather than as a
// wall of nested JSON at every call site.
func Schema(props map[string]string, required ...string) json.RawMessage {
	p := map[string]any{}
	for name, desc := range props {
		p[name] = map[string]any{"type": "string", "description": desc}
	}
	if required == nil {
		required = []string{}
	}
	b, _ := json.Marshal(map[string]any{
		"type": "object", "properties": p, "required": required,
	})
	return b
}
