package daemon

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"
)

// freePort asks the kernel for a port and gives it straight back, so the binder
// can take it. Hardcoding one makes the test fail on whatever machine already
// happens to be using it.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

func hello() *http.Server {
	return &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "amac")
	})}
}

// fakeTailnet stands in for an interface a test cannot create. macOS does not
// hand out 127.0.0.2, and no test should require a live Tailscale, so the binder
// is asked for the address it wants and given a real loopback listener back. The
// address bookkeeping is then checkable, and the listener is a genuine one, so a
// served request and a closed socket both mean what they say.
type fakeTailnet struct {
	mu    sync.Mutex
	asked []string
	last  net.Listener
}

func (f *fakeTailnet) listen(network, addr string) (net.Listener, error) {
	host, _, _ := net.SplitHostPort(addr)
	if host == "127.0.0.1" {
		return net.Listen(network, addr)
	}
	ln, err := net.Listen(network, "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.asked = append(f.asked, addr)
	f.last = ln
	f.mu.Unlock()
	return ln, nil
}

func (f *fakeTailnet) addr() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.last == nil {
		return ""
	}
	return f.last.Addr().String()
}

func get(t *testing.T, addr string) (int, error) {
	t.Helper()
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get("http://" + addr + "/")
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// TestLoopbackServesWithNoTailnet is the case that used to be a startup failure.
// Tailscale being off says nothing about whether this machine can open its own
// dashboard, and refusing to start took it away from the one place that never
// needed the tailnet.
func TestLoopbackServesWithNoTailnet(t *testing.T) {
	port := freePort(t)
	srv := hello()
	b := &Binder{
		Servers:       map[int]*http.Server{port: srv},
		LookupTailnet: func() (string, error) { return "", fmt.Errorf("is Tailscale running?") },
		Poll:          20 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("loopback must come up without a tailnet: %v", err)
	}
	defer srv.Close()

	code, err := get(t, fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil || code != 200 {
		t.Fatalf("want 200 on loopback, got %d %v", code, err)
	}
	if got := b.Tailnet(); got != "" {
		t.Errorf("no tailnet address should be claimed, got %q", got)
	}
}

// TestTailnetIsPickedUpWhenItAppears covers the daemon that starts at login and
// beats Tailscale to it. The old code waited once and then gave up for good.
func TestTailnetIsPickedUpWhenItAppears(t *testing.T) {
	port := freePort(t)
	srv := hello()

	var mu sync.Mutex
	ip := ""
	fake := &fakeTailnet{}
	b := &Binder{
		Servers: map[int]*http.Server{port: srv},
		LookupTailnet: func() (string, error) {
			mu.Lock()
			defer mu.Unlock()
			if ip == "" {
				return "", fmt.Errorf("not yet")
			}
			return ip, nil
		},
		Poll:   20 * time.Millisecond,
		Listen: fake.listen,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	mu.Lock()
	ip = "100.64.0.5"
	mu.Unlock()

	waitFor(t, func() bool { return b.Tailnet() == "100.64.0.5" },
		"the address should be bound once it exists")

	want := fmt.Sprintf("100.64.0.5:%d", port)
	fake.mu.Lock()
	asked := append([]string(nil), fake.asked...)
	fake.mu.Unlock()
	if len(asked) != 1 || asked[0] != want {
		t.Fatalf("binder should have asked for %q, asked for %v", want, asked)
	}
	if code, err := get(t, fake.addr()); err != nil || code != 200 {
		t.Fatalf("the tailnet listener must serve, got %d %v", code, err)
	}
}

// TestStaleTailnetListenerIsDropped is what quitting Tailscale does. The listener
// stays open on an interface that is gone, so it accepts nothing while still
// looking bound, and the daemon has to notice rather than hold it forever.
func TestStaleTailnetListenerIsDropped(t *testing.T) {
	port := freePort(t)
	srv := hello()

	var mu sync.Mutex
	ip := "100.64.0.5"
	fake := &fakeTailnet{}
	b := &Binder{
		Servers: map[int]*http.Server{port: srv},
		LookupTailnet: func() (string, error) {
			mu.Lock()
			defer mu.Unlock()
			if ip == "" {
				return "", fmt.Errorf("stopped")
			}
			return ip, nil
		},
		Poll:   20 * time.Millisecond,
		Listen: fake.listen,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	waitFor(t, func() bool { return b.Tailnet() == "100.64.0.5" }, "bound to start with")
	live := fake.addr()

	mu.Lock()
	ip = ""
	mu.Unlock()

	waitFor(t, func() bool { return b.Tailnet() == "" },
		"the address going away must release the listener")

	// The socket itself must be gone, not merely forgotten: a listener left open
	// on a vanished interface is exactly the state this is meant to prevent.
	if _, err := get(t, live); err == nil {
		t.Error("the stale listener is still accepting; it should have been closed")
	}

	// Loopback is unaffected: the Mac keeps its dashboard.
	code, err := get(t, fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil || code != 200 {
		t.Fatalf("loopback must survive the tailnet going away, got %d %v", code, err)
	}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out: %s", what)
}
