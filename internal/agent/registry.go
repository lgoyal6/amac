// Package agent maps an agent name to the adapter command that speaks ACP for
// it. This is the whole of amac's vendor coupling: one table. Adding OpenCode,
// Gemini CLI or any future agent is a row here, not a change anywhere else.
package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

type Adapter struct {
	Name string
	// Pkg is the npm package, version-pinned. An adapter that silently changes
	// protocol behaviour under us is exactly the failure this design exists to
	// avoid.
	Pkg string
	// Bin is the executable name the package installs.
	Bin  string
	Note string
}

// AdapterDir holds one npm install shared by every session. Resolving the
// binary here instead of shelling out to `npx` is not a micro-optimisation:
// `npx -y pkg@version` re-resolves against the registry on every invocation,
// so a cold or throttled network turns each session spawn into a multi-second
// stall or an outright hang. A supervisor that starts sessions on demand
// cannot have process startup depend on the network being fast.
func AdapterDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".amac", "adapters")
}

// Argv returns the command to launch this adapter, preferring the local
// install and falling back to npx so a fresh machine still works before
// `amac setup` has run.
func (a Adapter) Argv() []string {
	local := filepath.Join(AdapterDir(), "node_modules", ".bin", a.Bin)
	if st, err := os.Stat(local); err == nil && !st.IsDir() {
		return []string{local}
	}
	return []string{"npx", "-y", a.Pkg}
}

// Local reports whether the fast path is available.
func (a Adapter) Local() bool {
	st, err := os.Stat(filepath.Join(AdapterDir(), "node_modules", ".bin", a.Bin))
	return err == nil && !st.IsDir()
}

// The two adapters verified working against a live handshake on 2026-08-08.
// Both were renamed out of the @zed-industries scope; the old names still
// resolve but warn on every install.
var registry = map[string]Adapter{
	"claude": {
		Name: "claude",
		Pkg:  "@agentclientprotocol/claude-agent-acp@0.66.0",
		Bin:  "claude-agent-acp",
		Note: "Claude Code. Advertises loadSession and session fork/list/resume.",
	},
	"codex": {
		Name: "codex",
		Pkg:  "@agentclientprotocol/codex-acp@1.1.14",
		Bin:  "codex-acp",
		Note: "OpenAI Codex. Advertises resume/list/close/delete plus steering extensions.",
	},
}

// Packages lists the pinned specs for `amac setup` to install.
func Packages() []string {
	out := make([]string, 0, len(registry))
	for _, n := range Names() {
		out = append(out, registry[n].Pkg)
	}
	return out
}

func Get(name string) (Adapter, error) {
	a, ok := registry[name]
	if !ok {
		return Adapter{}, fmt.Errorf("unknown agent %q (known: %v)", name, Names())
	}
	return a, nil
}

func Names() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
