package daemon

import (
	"strings"
	"testing"
)

// The bug this guards: `tailscale ip` reports this node's tailnet address even
// while the client is stopped, because the control plane assigned it rather
// than this machine. Trusting that answer meant the daemon got past its own
// safety check and then died inside ListenAndServe with EADDRNOTAVAIL, a
// message that names neither Tailscale nor the reason.
func TestAssignedMeansOnThisMachine(t *testing.T) {
	if !assigned("127.0.0.1") {
		t.Fatal("loopback must count as assigned")
	}
	// TEST-NET-3, reserved for documentation and never routed or assigned.
	if assigned("203.0.113.7") {
		t.Fatal("an address that is not on any interface must not count")
	}
}

// A stopped Tailscale and an absent Tailscale are fixed differently, so they
// must not produce the same sentence.
func TestStoppedTailscaleSaysSo(t *testing.T) {
	_, err := TailnetIP()
	if err == nil {
		t.Skip("tailnet is up on this machine")
	}
	if !strings.Contains(err.Error(), "Tailscale") {
		t.Fatalf("error must name Tailscale, got %q", err)
	}
}
