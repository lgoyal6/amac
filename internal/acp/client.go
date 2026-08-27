package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// Notification is an agent-initiated message with no reply expected. The one
// that matters is session/update, which carries tool calls, plans and output
// chunks as structured data.
type Notification struct {
	Method string
	Params json.RawMessage
}

// IncomingRequest is an agent-initiated message that MUST be answered. ACP is
// bidirectional: having declared fs capabilities, the agent will call
// fs/read_text_file and fs/write_text_file on us, and it asks for tool
// approval via session/request_permission.
//
// Distinguishing these from notifications is not cosmetic. A request left
// unanswered blocks that agent's whole turn indefinitely, and it looks exactly
// like the agent hanging. The presence of an id is the discriminator.
type IncomingRequest struct {
	Method string
	Params json.RawMessage

	id json.RawMessage
	c  *Client
}

// Respond returns a result to the agent. Exactly one of Respond or RespondErr
// must be called for every IncomingRequest.
func (r IncomingRequest) Respond(result any) error {
	if result == nil {
		result = json.RawMessage("null")
	}
	b, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return r.c.tr.send(response{JSONRPC: "2.0", ID: r.id, Result: b})
}

func (r IncomingRequest) RespondErr(code int, msg string) error {
	return r.c.tr.send(response{JSONRPC: "2.0", ID: r.id, Error: &RPCError{Code: code, Message: msg}})
}

// Handler answers agent-initiated requests. It runs on its own goroutine per
// request, so a handler that blocks on a human (which request_permission
// does, by design) cannot stall the read loop or other sessions.
type Handler interface {
	Handle(ctx context.Context, req IncomingRequest)
}

// Client speaks ACP to one agent subprocess.
//
// A single read loop owns the pipe. Callers never read; they park on a channel
// keyed by request id. That keeps request/response correlation correct when
// several goroutines have calls in flight, which they will as soon as the
// supervisor is driving more than one turn.
type Client struct {
	cmd  *exec.Cmd
	tr   *transport
	name string

	mu      sync.Mutex
	nextID  int64
	pending map[string]chan response
	closed  bool

	notify  chan Notification
	handler Handler
	ctx     context.Context
	stderr  *tailBuffer
	done    chan struct{}
	readEr  error
}

// tailBuffer keeps the last N bytes written to it. Bounded so a chatty adapter
// cannot grow memory for the lifetime of a long session.
type tailBuffer struct {
	mu    sync.Mutex
	buf   []byte
	limit int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.limit {
		t.buf = t.buf[len(t.buf)-t.limit:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.buf))
}

// Stderr returns what the adapter last printed. Empty when it said nothing.
func (c *Client) Stderr() string {
	if c.stderr == nil {
		return ""
	}
	return c.stderr.String()
}

// explain enriches a protocol error with the adapter's own diagnostics. "EOF"
// alone sends you reading the wrong layer.
func (c *Client) explain(err error) error {
	msg := c.Stderr()
	if msg == "" {
		return err
	}
	if len(msg) > 600 {
		msg = "..." + msg[len(msg)-600:]
	}
	return fmt.Errorf("%w (adapter stderr: %s)", err, msg)
}

// SetHandler installs the responder for agent-initiated requests. Without one,
// such requests are refused explicitly rather than dropped: an agent that gets
// an error can report it, whereas one that gets silence hangs forever.
func (c *Client) SetHandler(h Handler) {
	c.mu.Lock()
	c.handler = h
	c.mu.Unlock()
}

// Spawn starts an agent adapter and begins reading from it. stderr is handed
// to the caller so adapter diagnostics can be logged without being confused
// for protocol traffic.
func Spawn(ctx context.Context, name string, argv []string, dir string, stderr io.Writer) (*Client, error) {
	if len(argv) == 0 {
		return nil, errors.New("empty command")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir

	// Always keep the adapter's last stderr, even when the caller discards it.
	// An adapter that dies at startup (missing node, bad auth, wrong version)
	// closes stdout, and the only thing the protocol layer can report is
	// "EOF" - which says nothing about the cause. The real message is on
	// stderr, and it is worth exactly the few KB it costs to hold onto.
	tail := &tailBuffer{limit: 4096}
	if stderr != nil {
		cmd.Stderr = io.MultiWriter(stderr, tail)
	} else {
		cmd.Stderr = tail
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", argv[0], err)
	}

	c := &Client{
		cmd:     cmd,
		tr:      newTransport(stdout, stdin),
		name:    name,
		stderr:  tail,
		pending: make(map[string]chan response),
		notify:  make(chan Notification, 256),
		ctx:     ctx,
		done:    make(chan struct{}),
	}
	go c.readLoop()
	return c, nil
}

// Notifications delivers agent-initiated messages. The buffer is bounded on
// purpose: an agent that floods updates must not grow memory without limit.
// Dropping is visible (see readLoop) rather than silent.
func (c *Client) Notifications() <-chan Notification { return c.notify }

// Done closes when the read loop ends, whether from EOF, a decode failure, or
// the process exiting.
func (c *Client) Done() <-chan struct{} { return c.done }

func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.readEr
}

func (c *Client) Name() string { return c.name }

func (c *Client) readLoop() {
	defer close(c.done)
	defer close(c.notify)

	for {
		raw, err := c.tr.recv()
		if err != nil {
			c.fail(err)
			return
		}

		var msg response
		if err := json.Unmarshal(raw, &msg); err != nil {
			// A frame we cannot parse means we have lost sync with the stream.
			// Continuing would silently mis-attribute later replies.
			c.fail(fmt.Errorf("decode message: %w (frame: %.200s)", err, raw))
			return
		}

		if msg.Method != "" {
			// An id means the agent expects an answer. Handle it on its own
			// goroutine: session/request_permission blocks until a human
			// decides, and doing that on the read loop would freeze every
			// other session behind it.
			if len(msg.ID) > 0 {
				req := IncomingRequest{Method: msg.Method, Params: msg.Params, id: msg.ID, c: c}
				c.mu.Lock()
				h := c.handler
				c.mu.Unlock()
				if h == nil {
					_ = req.RespondErr(-32601, "amac: no handler for "+msg.Method)
					continue
				}
				go h.Handle(c.ctx, req)
				continue
			}

			// No id: a notification, nothing to answer.
			select {
			case c.notify <- Notification{Method: msg.Method, Params: msg.Params}:
			default:
				// Bounded queue full. Say so rather than block the read loop,
				// which would deadlock every in-flight request.
				c.fail(fmt.Errorf("notification queue overflow on %s", msg.Method))
				return
			}
			continue
		}

		key := string(msg.ID)
		c.mu.Lock()
		ch, ok := c.pending[key]
		delete(c.pending, key)
		c.mu.Unlock()
		if ok {
			ch <- msg
		}
	}
}

// fail records the terminating error and wakes every parked caller, so a dead
// agent surfaces as an error on each call instead of a hang.
func (c *Client) fail(err error) {
	c.mu.Lock()
	if c.readEr == nil && !errors.Is(err, io.EOF) {
		c.readEr = err
	}
	pending := c.pending
	c.pending = make(map[string]chan response)
	c.closed = true
	c.mu.Unlock()

	for _, ch := range pending {
		ch <- response{Error: &RPCError{Code: -32000, Message: "agent connection closed: " + errString(err)}}
	}
}

func errString(err error) string {
	if err == nil {
		return "unknown"
	}
	return err.Error()
}

// Call issues a request and waits for its reply. ctx cancellation abandons the
// wait; the id stays registered so a late reply is discarded rather than
// mismatched onto a future request.
func (c *Client) Call(ctx context.Context, method string, params any, out any) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return fmt.Errorf("call %s: agent %s is closed", method, c.name)
	}
	c.nextID++
	id := strconv.FormatInt(c.nextID, 10)
	ch := make(chan response, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	if err := c.tr.send(request{JSONRPC: "2.0", ID: json.RawMessage(id), Method: method, Params: params}); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return err
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case resp := <-ch:
		if resp.Error != nil {
			return fmt.Errorf("%s: %w", method, resp.Error)
		}
		if out == nil || len(resp.Result) == 0 {
			return nil
		}
		if err := json.Unmarshal(resp.Result, out); err != nil {
			return fmt.Errorf("decode %s result: %w", method, err)
		}
		return nil
	}
}

func (c *Client) Notify(method string, params any) error {
	return c.tr.send(request{JSONRPC: "2.0", Method: method, Params: params})
}

// Initialize performs the version and capability handshake. It is the first
// call on every connection.
func (c *Client) Initialize(ctx context.Context) (InitializeResult, error) {
	var res InitializeResult
	err := c.Call(ctx, "initialize", InitializeParams{
		ProtocolVersion: ProtocolVersion,
		ClientCapabilities: ClientCapabilities{
			FS:       FSCapabilities{ReadTextFile: true, WriteTextFile: true},
			Terminal: false,
		},
		ClientInfo: ClientInfo{Name: "amac", Title: "amac", Version: "0.1.0"},
	}, &res)
	if err != nil {
		// The handshake is where a broken adapter shows up, so this is the one
		// place worth paying to produce a diagnosable error.
		return res, c.explain(err)
	}
	if res.ProtocolVersion > ProtocolVersion {
		return res, fmt.Errorf("agent %s speaks protocol v%d, amac supports v%d", c.name, res.ProtocolVersion, ProtocolVersion)
	}
	return res, nil
}

func (c *Client) Close() error {
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	return c.cmd.Wait()
}
