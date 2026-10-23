package health

import (
	"reflect"
	"testing"
)

// On a hotspot the Wi-Fi interface holds a 100.x address too. Tailscale's utun
// address has to be dialled first, and the other one kept as a fallback rather
// than taken as the answer.
func TestTailnetCandidatesPutUtunFirst(t *testing.T) {
	got := orderTailnet([]ifaceAddr{
		{"lo0", "127.0.0.1"},
		{"en0", "100.64.1.26"},
		{"en0", "192.168.1.5"},
		{"utun4", "100.127.168.102"},
	})
	want := []string{"100.127.168.102", "100.64.1.26"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got := orderTailnet([]ifaceAddr{{"en0", "192.168.1.5"}}); len(got) != 0 {
		t.Fatalf("no 100.x address means no tailnet, got %v", got)
	}
}
