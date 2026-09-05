package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This table is the whole of amac's vendor coupling, and it had no test. The
// two things it must get right are both failure modes that only appear on
// somebody else's machine: resolving to a local install when there is one, and
// resolving to something that still works when there is not.

func TestKnownAgentsResolveAndUnknownOnesSayWhatExists(t *testing.T) {
	for _, name := range []string{"claude", "codex"} {
		a, err := Get(name)
		if err != nil {
			t.Fatalf("Get(%q): %v", name, err)
		}
		if a.Name != name || a.Bin == "" || a.Pkg == "" {
			t.Errorf("%q is incompletely declared: %+v", name, a)
		}
		// The package is version-pinned on purpose: an adapter that silently
		// changes protocol behaviour underneath us is the failure this design
		// exists to avoid.
		if !strings.Contains(a.Pkg, "@") || strings.HasSuffix(a.Pkg, "@") {
			t.Errorf("%q is not version-pinned: %q", name, a.Pkg)
		}
	}

	_, err := Get("gemini")
	if err == nil {
		t.Fatal("an unknown agent must be an error")
	}
	// The error has to name what is available. "unknown agent" alone sends
	// somebody to read the source to find out what to type instead.
	for _, want := range []string{"claude", "codex"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not list %q as an option", err, want)
		}
	}
}

// Argv prefers the shared local install, and that is not a micro-optimisation:
// `npx -y pkg@version` re-resolves against the registry on every invocation, so
// a cold or throttled network turns each session spawn into a multi-second
// stall. A supervisor that starts sessions on demand cannot have process
// startup depend on the network being fast.
func TestArgvPrefersALocalInstallAndFallsBackToNpx(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	a, err := Get("claude")
	if err != nil {
		t.Fatal(err)
	}

	argv := a.Argv()
	if len(argv) == 0 || argv[0] != "npx" {
		t.Errorf("with nothing installed the fallback should be npx, got %v", argv)
	}
	if a.Local() {
		t.Error("Local() claimed an install that is not there")
	}

	bin := filepath.Join(AdapterDir(), "node_modules", ".bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(bin, a.Bin)
	if err := os.WriteFile(local, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	if got := a.Argv(); len(got) != 1 || got[0] != local {
		t.Errorf("Argv() = %v, want the local binary alone", got)
	}
	if !a.Local() {
		t.Error("Local() missed an install that is there")
	}
}

// A directory named like the binary is not a binary. Returning it as argv
// produces a spawn failure whose message is about permissions rather than about
// a half-finished install, which is a bad half hour for whoever hits it.
func TestADirectoryIsNotAnInstall(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	a, _ := Get("codex")
	if err := os.MkdirAll(filepath.Join(AdapterDir(), "node_modules", ".bin", a.Bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if a.Local() {
		t.Error("a directory was reported as an installed adapter")
	}
	if got := a.Argv(); got[0] != "npx" {
		t.Errorf("Argv() = %v, want the npx fallback", got)
	}
}

// Names and Packages drive `amac setup` and the board's agent picker, so they
// have to agree with the table rather than be maintained beside it.
func TestNamesAndPackagesCoverEveryAdapter(t *testing.T) {
	names, pkgs := Names(), Packages()
	if len(names) != len(pkgs) {
		t.Fatalf("%d names against %d packages", len(names), len(pkgs))
	}
	if len(names) < 2 {
		t.Fatalf("expected at least claude and codex, got %v", names)
	}
	for i := 1; i < len(names); i++ {
		if names[i-1] > names[i] {
			t.Errorf("Names() is not sorted, so the picker order would drift: %v", names)
			break
		}
	}
	for _, n := range names {
		a, err := Get(n)
		if err != nil {
			t.Errorf("Names() offered %q which Get rejects", n)
			continue
		}
		var found bool
		for _, p := range pkgs {
			if p == a.Pkg {
				found = true
			}
		}
		if !found {
			t.Errorf("setup would not install %q for %s", a.Pkg, n)
		}
	}
}

// AdapterDir is one shared install rather than one per session, and it has to
// follow HOME or a test, a container or a second user writes into somebody
// else's tree.
func TestAdapterDirFollowsHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if got := AdapterDir(); got != filepath.Join(home, ".amac", "adapters") {
		t.Errorf("AdapterDir() = %q, not under this HOME", got)
	}
}
