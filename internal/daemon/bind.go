package daemon

import (
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

const tailscaleCLI = "/Applications/Tailscale.app/Contents/MacOS/Tailscale"

// TailnetIP returns this machine's Tailscale address.
//
// Asking Tailscale directly matters. The tailnet range 100.64.0.0/10 is also
// carrier-grade NAT space, so a tethered hotspot can legitimately hand a
// physical interface a 100.x address. Trusting that would bind the daemon to
// the phone network. When the CLI is unavailable, only a utun interface counts.
func TailnetIP() (string, error) {
	reported := ""
	if out, err := exec.Command(tailscaleCLI, "ip", "-4").Output(); err == nil {
		ip := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
		if strings.HasPrefix(ip, "100.") {
			reported = ip
			if assigned(ip) {
				return ip, nil
			}
		}
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

// WaitForTailnet blocks until the mesh is up. A daemon started at login before
// Tailscale finishes connecting should wait, not fall back to a public bind
// and not exit.
func WaitForTailnet(timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for {
		ip, err := TailnetIP()
		if err == nil {
			return ip, nil
		}
		if time.Now().After(deadline) {
			return "", err
		}
		time.Sleep(3 * time.Second)
	}
}
