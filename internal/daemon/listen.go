package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

// Binder decides where the daemon can be reached, and it answers two different
// questions rather than one.
//
// Loopback is this machine asking for its own dashboard. Nothing outside the
// machine can reach 127.0.0.1, so there is no exposure to weigh and no reason to
// make it wait for anything: it binds first and it binds always.
//
// The tailnet is a phone asking. That address is the one with a security story
// behind it, and the rule there is unchanged: the Tailscale interface or nothing,
// never 0.0.0.0. What changes is that its absence is no longer fatal. Tailscale
// being off is a statement about the phone, not about the Mac, and a daemon that
// refuses to start on that basis takes the dashboard away from the one machine
// that never needed the tailnet to see it.
//
// The address is also watched rather than resolved once. A daemon started at
// login usually beats Tailscale to it, and an address that goes away when
// Tailscale quits leaves a listener bound to an interface that no longer exists:
// still open, still accepting nothing. Both are the same job, so both are handled
// by the same loop.
type Binder struct {
	// Servers maps a port to the server that answers on it. Each server is
	// served on every address this binder holds, so one server can be reached
	// on loopback and on the tailnet at once.
	Servers map[int]*http.Server

	// LookupTailnet finds the tailnet address. Nil means TailnetIP.
	LookupTailnet func() (string, error)

	// Poll is how often the tailnet address is re-checked. Zero means 3s.
	Poll time.Duration

	// Listen opens one listener. Nil means net.Listen. It exists because a test
	// cannot conjure a tailnet interface, and macOS will not hand out a second
	// loopback address to stand in for one, so the address bookkeeping has to be
	// checkable without either.
	Listen func(network, addr string) (net.Listener, error)

	// Announce reports a change in reachability. Nil discards it.
	Announce func(string)

	mu      sync.Mutex
	bound   string // tailnet address currently served, "" for none
	tailnet []net.Listener
}

// Start listens on loopback and returns once it is accepting, then watches the
// tailnet in the background until ctx is done.
//
// Failing to bind loopback is fatal, because that is the daemon being unreachable
// from the machine it runs on. Failing to find a tailnet address is not.
func (b *Binder) Start(ctx context.Context) error {
	for port, srv := range b.Servers {
		ln, err := b.listen("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprint(port)))
		if err != nil {
			return fmt.Errorf("cannot listen on 127.0.0.1:%d: %w", port, err)
		}
		go serve(srv, ln)
	}
	go b.watch(ctx)
	return nil
}

// watch keeps the tailnet listeners in step with the address that actually exists.
func (b *Binder) watch(ctx context.Context) {
	poll := b.Poll
	if poll <= 0 {
		poll = 3 * time.Second
	}
	lookup := b.LookupTailnet
	if lookup == nil {
		lookup = TailnetIP
	}

	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		b.reconcile(lookup)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// reconcile makes the bound address match the one Tailscale reports.
func (b *Binder) reconcile(lookup func() (string, error)) {
	// While the address we hold is still on an interface, nothing has changed and
	// there is nothing to ask. This matters more than it looks: the full lookup
	// shells out to Tailscale, and doing that every few seconds for the life of the
	// daemon is both tens of thousands of processes a day and a fresh chance to
	// meet the command that hangs. Reading the interface list costs neither.
	if held := b.held(); held != "" && assigned(held) {
		return
	}

	ip, err := lookup()
	if err != nil {
		ip = ""
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if ip == b.bound {
		return
	}

	// Either the address went away or it changed. The listeners standing on the
	// old one are answering an interface that is gone, so they go first.
	if b.bound != "" {
		for _, ln := range b.tailnet {
			_ = ln.Close()
		}
		b.tailnet = nil
		b.bound = ""
		b.say("tailnet address went away; still reachable on 127.0.0.1")
	}
	if ip == "" {
		return
	}

	for port, srv := range b.Servers {
		ln, err := b.listen("tcp", net.JoinHostPort(ip, fmt.Sprint(port)))
		if err != nil {
			// Leave b.bound empty so the next tick tries again rather than
			// believing a half-bound address is serving.
			for _, open := range b.tailnet {
				_ = open.Close()
			}
			b.tailnet = nil
			b.say(fmt.Sprintf("cannot listen on %s:%d yet: %v", ip, port, err))
			return
		}
		b.tailnet = append(b.tailnet, ln)
		go serve(srv, ln)
	}
	b.bound = ip
	b.say(fmt.Sprintf("tailnet address %s is up; reachable from your phone", ip))
}

// held reports the tailnet address currently served, or "" for none.
func (b *Binder) held() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.bound
}

func (b *Binder) listen(network, addr string) (net.Listener, error) {
	if b.Listen != nil {
		return b.Listen(network, addr)
	}
	return net.Listen(network, addr)
}

func (b *Binder) say(msg string) {
	if b.Announce != nil {
		b.Announce(msg)
	}
}

// serve runs one listener and swallows the two errors that are not faults: the
// server shutting down, and a tailnet listener being closed because its address
// disappeared.
func serve(srv *http.Server, ln net.Listener) {
	err := srv.Serve(ln)
	if err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
		_ = err
	}
}
