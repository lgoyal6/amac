package health

// A service, as opposed to a scheduled job.
//
// Every other automation here is periodic, and the question about a periodic
// job is "did it deliver recently". A daemon is not periodic: it is either
// serving or it is not, and asking when it last delivered says nothing. So
// this probe answers liveness directly and leaves Last zero, which suppresses
// the cadence test rather than inventing a delivery to satisfy it.
//
// That is a stronger check than the cadence test, not a weaker one. A silent
// job is only detectable after cadence plus grace has elapsed; a dead service
// is detectable on the next sweep.

import (
	"context"
	"net"
	"os"
	"strings"
	"time"
)

// serviceOnTailnet checks that a tailnet-bound service is actually serving.
//
// The label and port come from the roster rather than from constants, because a
// port stated in two places is a probe that reports a healthy daemon as down
// the day someone moves it.
//
// A service binding the tailnet has three distinguishable states and they are
// fixed differently: not loaded is a setup problem, loaded with no tailnet is a
// Tailscale toggle, and loaded with a tailnet but nothing answering is a crash.
// A single "down" for all three sends you looking in the wrong place.
func serviceOnTailnet(ctx context.Context, label, daemonPort string) (Report, error) {
	r := Report{State: OK}

	loaded, _, _, err := launchdStatus(ctx, label)
	if err != nil {
		return r, err
	}
	if !loaded {
		r.State = Down
		r.Detail = label + " is not loaded in launchd"
		return r, nil
	}

	ip, ok := tailnetAddr()
	if !ok {
		// Not a failure of the daemon. It is doing exactly what it was built
		// to do, which is refuse to bind anywhere but the tailnet, and saying
		// "down" without the reason would send you reading amac's logs instead
		// of turning Tailscale on.
		r.State = Down
		r.Detail = "not serving: Tailscale is not up, and the daemon binds the tailnet or nothing"
		return r, nil
	}

	// A dial, not an HTTP request. Every route needs the token, so a 401 and a
	// 200 prove the same thing here: something is listening and it is the
	// daemon. Asking for more would mean putting the token in the probe.
	conn, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(ip, daemonPort))
	if err != nil {
		r.State = Down
		r.Detail = "loaded and the tailnet is up, but nothing is listening on " + ip + ":" + daemonPort
		r.Err = err.Error()
		return r, nil
	}
	_ = conn.Close()

	r.Detail = "serving the board on " + ip + ":" + daemonPort
	if host, err := os.Hostname(); err == nil {
		r.Notes = []string{"http://" + strings.TrimSuffix(host, ".local") + ":" + daemonPort + " on the tailnet"}
	}
	return r, nil
}

// tailnetAddr finds this machine's tailnet address on a local interface.
//
// Deliberately not the Tailscale CLI: it reports the node's address from the
// control plane even while the client is stopped, so trusting it would report
// a daemon as merely crashed when Tailscale is simply off. The interface list
// answers the question the probe actually has, which is whether that address
// exists here right now.
func tailnetAddr() (string, bool) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "", false
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.To4() == nil {
			continue
		}
		if ip := ipnet.IP.To4().String(); strings.HasPrefix(ip, "100.") {
			return ip, true
		}
	}
	return "", false
}
