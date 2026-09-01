package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// handlerFunc adapts a function to Handler. It lives here rather than in the
// client because production installs a real handler and never needed the
// adapter; putting it there would be an export with no caller.
type handlerFunc func(ctx context.Context, req IncomingRequest)

func (f handlerFunc) Handle(ctx context.Context, req IncomingRequest) { f(ctx, req) }

// fakeAgent is the other end of the wire: a pipe pair and a goroutine that
// answers. Real adapters are node processes, and spawning one per test would
// make these depend on npm, a network and an auth token to prove things about
// JSON-RPC.
type fakeAgent struct {
	c    *Client
	reqs chan []byte    // raw frames the client has written
	out  *io.PipeWriter // the agent's side of the client's input
	done chan struct{}
}

// newFakeAgent wires a client to a pipe pair and, crucially, always drains what
// the client writes. io.Pipe is unbuffered, so a test that only sometimes reads
// would block the client inside send before its call ever parked, and every
// assertion after that would be about the harness rather than the client.
func newFakeAgent(t *testing.T) *fakeAgent {
	t.Helper()
	clientReads, agentWrites := io.Pipe()
	agentReads, clientWrites := io.Pipe()

	c := &Client{
		tr:      newTransport(clientReads, clientWrites),
		name:    "fake",
		stderr:  &tailBuffer{limit: 4096},
		pending: make(map[string]chan response),
		notify:  make(chan Notification, 4),
		ctx:     context.Background(),
		done:    make(chan struct{}),
	}
	go c.readLoop()

	f := &fakeAgent{c: c, reqs: make(chan []byte, 64), out: agentWrites,
		done: make(chan struct{})}
	go func() {
		defer close(f.reqs)
		// Raw frames rather than decoded requests: the client writes both
		// requests and responses, and decoding everything as one shape
		// silently drops the fields of the other.
		dec := json.NewDecoder(agentReads)
		for {
			var raw json.RawMessage
			if err := dec.Decode(&raw); err != nil {
				return
			}
			select {
			case f.reqs <- raw:
			case <-f.done:
				return
			}
		}
	}()

	t.Cleanup(func() {
		close(f.done)
		agentWrites.Close()
		agentReads.Close()
	})
	return f
}

// raw returns the next frame the client sent.
func (f *fakeAgent) raw(t *testing.T) []byte {
	t.Helper()
	select {
	case b, ok := <-f.reqs:
		if !ok {
			t.Fatal("the client closed its side")
		}
		return b
	case <-time.After(5 * time.Second):
		t.Fatal("the client sent nothing")
		return nil
	}
}

// next decodes that frame as a request, for the cases that expect one.
func (f *fakeAgent) next(t *testing.T) request {
	t.Helper()
	var req request
	if err := json.Unmarshal(f.raw(t), &req); err != nil {
		t.Fatalf("client frame is not a request: %v", err)
	}
	return req
}

func (f *fakeAgent) reply(t *testing.T, id json.RawMessage, result any) {
	t.Helper()
	b, _ := json.Marshal(result)
	f.write(t, response{JSONRPC: "2.0", ID: id, Result: b})
}

// write sends one frame to the client. It gives up rather than blocking
// forever: once the client has stopped reading, an unbuffered pipe write never
// returns, and a test that hangs there reports nothing about what it meant to
// check.
func (f *fakeAgent) write(t *testing.T, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	written := make(chan error, 1)
	go func() {
		_, err := f.out.Write(append(b, '\n'))
		written <- err
	}()
	select {
	case err := <-written:
		if err != nil {
			t.Fatalf("writing to client: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the client stopped reading its input")
	}
}

// writeRaw is write without the assertion, for the cases that expect the client
// to stop reading partway through.
func (f *fakeAgent) writeRaw(b []byte) {
	go func() { f.out.Write(b) }()
}

// The README's first claim: several goroutines can have calls in flight and
// each gets its own answer. Replies come back deliberately out of order,
// because in-order delivery would pass even if the client ignored ids entirely.
func TestConcurrentCallsAreCorrelated(t *testing.T) {
	f := newFakeAgent(t)
	const n = 20

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var out struct {
				N int `json:"n"`
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if err := f.c.Call(ctx, "echo", map[string]any{"n": i}, &out); err != nil {
				errs <- err
				return
			}
			if out.N != i {
				errs <- fmt.Errorf("call %d got the reply for %d", i, out.N)
			}
		}(i)
	}

	// Collect them all, then answer backwards. In-order replies would pass
	// even if the client ignored ids entirely.
	reqs := make([]request, 0, n)
	for i := 0; i < n; i++ {
		reqs = append(reqs, f.next(t))
	}
	for i := len(reqs) - 1; i >= 0; i-- {
		var p struct {
			N int `json:"n"`
		}
		json.Unmarshal(mustJSON(reqs[i].Params), &p)
		f.reply(t, reqs[i].ID, map[string]any{"n": p.N})
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// An agent-initiated request runs on its own goroutine, because
// session/request_permission blocks until a human decides. If it ran on the
// read loop, one unanswered prompt would freeze every other session behind it.
func TestABlockedHandlerDoesNotStallTheReadLoop(t *testing.T) {
	f := newFakeAgent(t)

	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	f.c.SetHandler(handlerFunc(func(ctx context.Context, req IncomingRequest) {
		entered <- struct{}{}
		<-release // a human, taking their time
		_ = req.Respond(map[string]any{"ok": true})
	}))

	// The agent asks something that will block, then immediately answers a
	// normal call. The second must not wait for the first.
	f.write(t, map[string]any{
		"jsonrpc": "2.0", "id": "agent-1", "method": "session/request_permission",
	})
	<-entered

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		done <- f.c.Call(ctx, "ping", nil, nil)
	}()

	req := f.next(t)
	f.reply(t, req.ID, map[string]any{})
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("call behind a blocked handler failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a blocked handler stalled the read loop")
	}
	close(release)
}

// A dead agent has to surface as an error on every parked call. Hanging is the
// failure this is built to avoid: a session waiting forever looks identical to
// one thinking hard.
func TestADeadAgentWakesEveryParkedCall(t *testing.T) {
	f := newFakeAgent(t)

	const n = 5
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			errs <- f.c.Call(context.Background(), "slow", nil, nil)
		}()
	}
	// Wait until all of them are actually parked, so this tests the wake and
	// not a race between Call and the close.
	waitFor(t, func() bool {
		f.c.mu.Lock()
		defer f.c.mu.Unlock()
		return len(f.c.pending) == n
	}, "calls to park")

	f.out.Close() // the agent dies

	for i := 0; i < n; i++ {
		select {
		case err := <-errs:
			if err == nil {
				t.Fatal("a call against a dead agent must not succeed")
			}
			if !strings.Contains(err.Error(), "closed") {
				t.Errorf("error should say the connection closed, got %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a parked call hung after the agent died")
		}
	}

	// And a call made afterwards fails immediately rather than parking.
	if err := f.c.Call(context.Background(), "after", nil, nil); err == nil {
		t.Error("calling a closed agent must fail")
	}
}

// The notification buffer is bounded so a flooding agent cannot grow memory
// without limit. Overflow is loud: dropping quietly would lose session updates
// with nothing to say they went missing.
func TestNotificationOverflowIsLoudNotSilent(t *testing.T) {
	f := newFakeAgent(t) // buffer of 4, and nobody is draining it

	for i := 0; i < 50; i++ {
		b, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "method": "session/update", "params": map[string]int{"i": i},
		})
		f.writeRaw(append(b, '\n'))
	}

	select {
	case <-f.c.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the client kept accepting notifications past its buffer")
	}
	err := f.c.Err()
	if err == nil || !strings.Contains(err.Error(), "overflow") {
		t.Fatalf("overflow must be reported, got %v", err)
	}
}

// bufio.Scanner defaults to 64KB and a single tool result carrying a file blows
// straight past it. The failure mode is indistinguishable from an agent going
// quiet, which is why the limit is explicit.
func TestAFrameLargerThanScannersDefault(t *testing.T) {
	f := newFakeAgent(t)

	big := strings.Repeat("x", 300*1024) // well past 64KB
	go func() {
		req := f.next(t)
		f.reply(t, req.ID, map[string]any{"text": big})
	}()

	var out struct {
		Text string `json:"text"`
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := f.c.Call(ctx, "read", nil, &out); err != nil {
		t.Fatalf("a 300KB frame must survive: %v", err)
	}
	if len(out.Text) != len(big) {
		t.Fatalf("got %d bytes back, sent %d", len(out.Text), len(big))
	}
}

// A frame the client cannot parse means the stream is out of sync, and
// continuing would mis-attribute later replies to earlier requests.
func TestAnUnparseableFrameEndsTheConnection(t *testing.T) {
	f := newFakeAgent(t)
	f.writeRaw([]byte("{not json at all\n"))

	select {
	case <-f.c.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the client kept reading a stream it had lost sync with")
	}
	if err := f.c.Err(); err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("got %v", err)
	}
}

// A request with no handler installed is answered rather than ignored. An agent
// waiting forever for a reply nobody will send is the hang this avoids.
func TestARequestWithNoHandlerIsRefusedNotDropped(t *testing.T) {
	f := newFakeAgent(t)
	f.write(t, map[string]any{
		"jsonrpc": "2.0", "id": "x1", "method": "fs/read_text_file",
	})

	raw := string(f.raw(t))
	if !strings.Contains(raw, "no handler") {
		t.Fatalf("expected a refusal naming the missing handler, got %s", raw)
	}
	if !strings.Contains(raw, `"id":"x1"`) {
		t.Errorf("the refusal must carry the id it answers: %s", raw)
	}
}

// A cancelled call must not leave its slot behind. Ids only ever increase, so a
// late reply lands on an id nothing is waiting for and is dropped either way;
// keeping the entry only grows the map for the life of the process.
func TestACancelledCallDoesNotLeakItsSlot(t *testing.T) {
	f := newFakeAgent(t)

	go func() {
		for i := 0; i < 30; i++ {
			f.next(t) // read the request, never answer it
		}
	}()

	for i := 0; i < 30; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		_ = f.c.Call(ctx, "never-answered", nil, nil)
		cancel()
	}

	waitFor(t, func() bool {
		f.c.mu.Lock()
		defer f.c.mu.Unlock()
		return len(f.c.pending) == 0
	}, "the pending map to drain")
}

// A late reply for a call that was abandoned must be harmless.
func TestALateReplyIsDropped(t *testing.T) {
	f := newFakeAgent(t)

	// The call is abandoned before any answer arrives.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	go func() { _ = f.c.Call(ctx, "abandoned", nil, nil) }()
	abandoned := f.next(t).ID
	<-ctx.Done()
	cancel()

	// The answer turns up anyway, for an id nothing is waiting on.
	f.reply(t, abandoned, map[string]any{"too": "late"})

	// The connection survives it, and the next call still works.
	go func() {
		req := f.next(t)
		f.reply(t, req.ID, map[string]any{"ok": true})
	}()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	if err := f.c.Call(ctx2, "after", nil, nil); err != nil {
		t.Fatalf("a late reply broke the next call: %v", err)
	}
}

// Spawn keeps the adapter's stderr, because an adapter that dies at startup
// closes stdout and the protocol layer can only report EOF, which says nothing
// about the cause.
func TestSpawnKeepsStderrForADeadAdapter(t *testing.T) {
	c, err := Spawn(context.Background(), "fake", []string{"sh", "-c",
		"echo 'node: command not found' >&2; exit 1"}, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	select {
	case <-c.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("a process that exited did not end the read loop")
	}
	if got := c.Stderr(); !strings.Contains(got, "command not found") {
		t.Fatalf("stderr was lost; got %q", got)
	}
	// And the error a caller sees names the cause rather than saying EOF.
	err = c.Call(context.Background(), "anything", nil, nil)
	if err == nil {
		t.Fatal("a call to a dead adapter must fail")
	}
}

// The stderr test above only ever failed by luck: it asks for a string that
// arrives on a goroutine nobody joined, so a quiet machine wins the race and a
// loaded one does not. This asks the deterministic question instead. os/exec
// copies stderr on its own goroutine and joins it in Wait, so "the process has
// been reaped" is exactly the condition under which Stderr is complete, and it
// is observable without racing anything.
func TestDoneMeansTheAdapterHasBeenReaped(t *testing.T) {
	c, err := Spawn(context.Background(), "fake", []string{"sh", "-c",
		"echo 'node: command not found' >&2; exit 1"}, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	select {
	case <-c.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("a process that exited did not end the read loop")
	}
	if c.cmd.ProcessState == nil {
		t.Fatal("Done closed before the process was reaped, so Stderr can still be empty")
	}
}

// A parked caller must not be woken ahead of the reap either. Initialize is
// the one that matters: it hands its error straight to explain, which reads
// Stderr, and the handshake is where a broken adapter shows up.
func TestTheHandshakeErrorCarriesTheAdaptersOwnWords(t *testing.T) {
	c, err := Spawn(context.Background(), "fake", []string{"sh", "-c",
		"echo 'node: command not found' >&2; exit 1"}, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	_, err = c.Initialize(context.Background())
	if err == nil {
		t.Fatal("a handshake with a dead adapter must fail")
	}
	if !strings.Contains(err.Error(), "command not found") {
		t.Fatalf("the error says nothing about why: %v", err)
	}
}

// The reap is deliberately skipped when the stream ended for a reason other
// than the adapter going away, because there is a live process on the other
// end and nothing to wait for. Waiting there would turn a rare lost diagnostic
// into a guaranteed delay in reporting a session dead.
func TestALiveAdapterIsNotWaitedFor(t *testing.T) {
	// exec, so the shell becomes the sleep rather than forking it. A forked
	// grandchild survives Close's kill still holding the pipes, and Close waits
	// on those, which costs this test the full thirty seconds for nothing.
	c, err := Spawn(context.Background(), "fake", []string{"sh", "-c",
		"echo 'not json'; exec sleep 30"}, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// Comfortably under reapWait, and comfortably over the time a shell needs
	// to echo one line: a regression here costs the whole five seconds.
	select {
	case <-c.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("a decode failure waited on a process that is still running")
	}
}

// And the bound itself, so a process that closes stdout without exiting cannot
// hold a session open forever.
func TestTheReapGivesUp(t *testing.T) {
	c, err := Spawn(context.Background(), "fake", []string{"sleep", "30"}, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	start := time.Now()
	c.awaitExit(50 * time.Millisecond)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("awaitExit ignored its limit and took %s", elapsed)
	}
}

func TestSpawnRejectsAnEmptyCommand(t *testing.T) {
	if _, err := Spawn(context.Background(), "x", nil, "", nil); err == nil {
		t.Fatal("an empty argv must be refused")
	}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
