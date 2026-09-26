package daemon

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"time"
)

// Resolving the bind address is a security decision, not a config detail. This
// daemon can start agents, approve their tool calls, and write files. Exposing
// it on 0.0.0.0 would hand that to anyone on the same coffee shop wifi.
//
// The rule, carried over from the predecessor's ttyd setup: bind to the
// Tailscale interface or do not start. There is deliberately no fallback.

// A var, not a const, so a test can point it at a stand-in. There is no other
// way to exercise a command that hangs.
var tailscaleCLI = "/Applications/Tailscale.app/Contents/MacOS/Tailscale"

// cliTimeout bounds the one call that can hang. Also a var for the same reason.
var cliTimeout = 3 * time.Second

// tailnetFromCLI asks Tailscale what this node's address is, and gives up quickly.
//
// The deadline is not defensive padding. This command talks to the Tailscale GUI,
// and under launchd that conversation can simply never finish: the process sits
// there, Output blocks, and whoever called it blocks too. The daemon was found in
// exactly that state, holding a `tailscale ip -4` child that had been alive for as
// long as the daemon had, never binding the tailnet and never reporting a reason,
// because a caller stuck inside exec has nothing to report.
//
// It also exits zero while printing a human-readable failure instead of an
// address, so the answer is trusted only when it looks like one. Both failures
// land in the same place: return nothing and let the caller read the interfaces,
// which is the more reliable source anyway.
func tailnetFromCLI() string {
	ctx, cancel := context.WithTimeout(context.Background(), cliTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, tailscaleCLI, "ip", "-4")

	// Cancelling is not enough on its own. Output waits for the stdout pipe to
	// close, and the pipe is inherited, so anything the command left behind holds
	// it open and we keep waiting on a process that is already dead. WaitDelay is
	// what actually lets go.
	cmd.WaitDelay = time.Second

	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	ip := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	if !strings.HasPrefix(ip, "100.") {
		return ""
	}
	return ip
}

// TailnetIP returns this machine's Tailscale address.
//
// Asking Tailscale directly matters. The tailnet range 100.64.0.0/10 is also
// carrier-grade NAT space, so a tethered hotspot can legitimately hand a
// physical interface a 100.x address. Trusting that would bind the daemon to
// the phone network. When the CLI is unavailable, only a utun interface counts.
func TailnetIP() (string, error) {
	reported := tailnetFromCLI()
	if reported != "" && assigned(reported) {
		return reported, nil
	}

	ifaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	for _, iface := range ifaces {
		if !strings.HasPrefix(iface.Name, "utun") || iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok || ipnet.IP.To4() == nil {
				continue
			}
			if ip := ipnet.IP.To4().String(); strings.HasPrefix(ip, "100.") {
				return ip, nil
			}
		}
	}
	if reported != "" {
		// The distinction is worth spelling out because the two failures look
		// identical and are fixed differently. Tailscale not installed is a
		// setup problem; Tailscale installed and switched off is a toggle.
		return "", fmt.Errorf("Tailscale reports %s but it is not up on this machine: connect it from the menu bar", reported)
	}
	return "", fmt.Errorf("no tailnet address: is Tailscale running?")
}

// assigned reports whether an address is actually on a local interface.
//
// `tailscale ip` answers "what is this node's address in the tailnet", and it
// keeps answering while the client is stopped, because the address is assigned
// by the control plane rather than by this machine. Binding it in that state
// fails with EADDRNOTAVAIL, from ListenAndServe, several seconds later, with a
// message that mentions neither Tailscale nor the reason. The question the
// daemon actually has is not what this node is called on the tailnet but
// whether that address exists here right now.
func assigned(ip string) bool {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && ipnet.IP.String() == ip {
			return true
		}
	}
	return false
}
