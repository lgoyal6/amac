package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func run(t *testing.T, tools []Tool, lines ...string) []map[string]any {
	t.Helper()
	var out strings.Builder
	s := NewServer("test", tools)
	if err := s.Serve(context.Background(), strings.NewReader(strings.Join(lines, "\n")+"\n"), &out); err != nil {
		t.Fatal(err)
	}
	var got []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("not JSON: %q", line)
		}
		got = append(got, m)
	}
	return got
}

var echo = []Tool{{
	Name: "echo", Description: "echoes", InputSchema: Schema(map[string]string{"s": "text"}, "s"),
	Handler: func(_ context.Context, raw json.RawMessage) (string, error) {
		var a struct {
			S string `json:"s"`
		}
		json.Unmarshal(raw, &a)
		if a.S == "" {
			return "", errors.New("s is required")
		}
		return "you said " + a.S, nil
	},
}}

func TestHandshakeAndList(t *testing.T) {
	got := run(t, echo,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if len(got) != 2 {
		t.Fatalf("got %d responses", len(got))
	}
	res := got[0]["result"].(map[string]any)
	// Stated, not echoed. Claiming to support whatever a client asked for is
	// how a version mismatch becomes a confusing failure three calls later.
	if res["protocolVersion"] != protocolVersion {
		t.Errorf("protocolVersion = %v", res["protocolVersion"])
	}
	tools := got[1]["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("listed %d tools", len(tools))
	}
}

// A tool that fails has something to say, and the agent should read it. A
// JSON-RPC error says the server broke, which sends it to retry or give up.
func TestToolFailureIsReadableNotAProtocolError(t *testing.T) {
	got := run(t, echo,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{}}}`)
	res, ok := got[0]["result"].(map[string]any)
	if !ok {
		t.Fatalf("a failing tool must still return a result: %v", got[0])
	}
	if res["isError"] != true {
		t.Error("the result must be marked as an error")
	}
	text := res["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "s is required") {
		t.Errorf("the agent cannot see why it failed: %q", text)
	}
}

// A notification has no id and takes no response. Replying to one is a protocol
// error that some clients tolerate and others do not.
func TestNotificationsGetNoReply(t *testing.T) {
	got := run(t, echo,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":9,"method":"tools/list"}`)
	if len(got) != 1 {
		t.Fatalf("got %d responses, want 1", len(got))
	}
	if got[0]["id"] != float64(9) {
		t.Errorf("replied to the wrong message: %v", got[0]["id"])
	}
}

func TestUnknownToolAndMethod(t *testing.T) {
	got := run(t, echo,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"nope","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"telepathy"}`)
	for i, want := range []string{"no such tool", "method not found"} {
		e, ok := got[i]["error"].(map[string]any)
		if !ok {
			t.Fatalf("%d: expected an error, got %v", i, got[i])
		}
		if !strings.Contains(e["message"].(string), want) {
			t.Errorf("%d: message %q does not say %q", i, e["message"], want)
		}
	}
}

// Garbage on the wire must not take the server down with it: an agent that
// sends one malformed line still has a session to recover in.
func TestMalformedInputDoesNotStopTheServer(t *testing.T) {
	got := run(t, echo,
		`not json at all`,
		`{"jsonrpc":"2.0","id":7,"method":"tools/list"}`)
	if len(got) != 2 || got[1]["id"] != float64(7) {
		t.Fatalf("server did not survive bad input: %v", got)
	}
}
