package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeCLI writes a stand-in for the Tailscale binary and points tailscaleCLI at
// it for the length of the test.
func fakeCLI(t *testing.T, script string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tailscale")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := tailscaleCLI
	tailscaleCLI = path
	t.Cleanup(func() { tailscaleCLI = old })
}

// TestCLIThatHangsDoesNotHangUs is the failure this deadline exists for.
//
// Under launchd `tailscale ip -4` can talk to a GUI that never answers, and the
// process then sits forever. Without a deadline the caller sits with it: the
// daemon was found holding a child as old as itself, never binding the tailnet
// and never reporting why, because code stuck inside exec cannot report anything.
func TestCLIThatHangsDoesNotHangUs(t *testing.T) {
	fakeCLI(t, "sleep 30")
	old := cliTimeout
	cliTimeout = 150 * time.Millisecond
	t.Cleanup(func() { cliTimeout = old })

	done := make(chan string, 1)
	go func() { done <- tailnetFromCLI() }()

	select {
	case got := <-done:
		if got != "" {
			t.Errorf("a hung CLI must yield nothing, got %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("tailnetFromCLI never returned; the deadline is not working")
	}
}

// TestCLIErrorTextIsNotAnAddress covers the other half of the same surprise. The
// CLI exits zero and prints a sentence when the GUI is not up, so a caller that
// trusts exit status alone ends up treating prose as an address.
func TestCLIErrorTextIsNotAnAddress(t *testing.T) {
	fakeCLI(t, `echo "The Tailscale GUI failed to start: The operation couldn't be completed. (Tailscale.CLIError error 3.)"`)
	if got := tailnetFromCLI(); got != "" {
		t.Errorf("an error sentence is not an address, got %q", got)
	}
}

// TestCLIAddressIsRead keeps the happy path honest: the deadline and the shape
// check must not reject a real answer.
func TestCLIAddressIsRead(t *testing.T) {
	fakeCLI(t, `echo 100.127.168.102`)
	if got := tailnetFromCLI(); got != "100.127.168.102" {
		t.Errorf("want the address back, got %q", got)
	}
}

// TestCLIExitFailureIsQuiet covers Tailscale not being installed at all, which is
// a legitimate state on a machine that only ever uses loopback.
func TestCLIExitFailureIsQuiet(t *testing.T) {
	fakeCLI(t, "exit 1")
	if got := tailnetFromCLI(); got != "" {
		t.Errorf("a failed CLI must yield nothing, got %q", got)
	}
}
