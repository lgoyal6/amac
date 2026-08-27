package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// cmdHooks reports, and wires, the signals that tell amac a session wants you.
//
// This exists because of a failure that stayed invisible for two weeks. The
// Codex path was wired into amac, the Claude path was still pointing at the
// predecessor, and nothing anywhere said so: the log filled with Codex
// attention events, the phone stayed quiet about Claude, and both look exactly
// the same from outside. A control plane that cannot report whether its own
// inputs are connected is a control plane that will lie by omission.
//
// So the default is a status report over every agent's path, and installing is
// the flag. The report is the point; the installer is a convenience.
func cmdHooks(args []string) error {
	fs := flag.NewFlagSet("hooks", flag.ExitOnError)
	install := fs.Bool("install", false, "wire Claude Code's hooks into amac")
	binPath := fs.String("bin", "", "amac binary the hooks should call (default: this one)")
	settingsPath := fs.String("settings", claudeSettingsPath(), "Claude Code settings file")
	if err := fs.Parse(args); err != nil {
		return err
	}

	exe := *binPath
	if exe == "" {
		var err error
		if exe, err = os.Executable(); err != nil {
			return fmt.Errorf("locate this binary: %w", err)
		}
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
	}

	if *install {
		changed, backup, err := installClaudeHooks(*settingsPath, exe)
		if err != nil {
			return err
		}
		if len(changed) == 0 {
			fmt.Println("Claude Code hooks already point at this binary; nothing to do.")
		} else {
			fmt.Printf("wired %s\n", *settingsPath)
			fmt.Printf("backup %s\n\n", backup)
			for _, e := range changed {
				fmt.Printf("  + %s\n", e)
			}
			fmt.Printf("\nExisting hooks on those events were kept. Claude Code reads settings\n")
			fmt.Printf("on session start, so this reaches sessions you start from now on.\n\n")
		}
	}

	return reportHooks(*settingsPath, exe)
}

func claudeSettingsPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "settings.json")
}

// ------------------------------------------------------------------ report --

type wiring struct {
	name   string
	wired  bool
	detail string
}

func reportHooks(settings, exe string) error {
	fmt.Printf("amac  %s\n\n", exe)

	groups := []struct {
		agent string
		rows  []wiring
	}{
		{"claude", claudeWiringStatus(settings)},
		{"codex", []wiring{codexWiringStatus(), tmuxBellStatus()}},
	}

	missing := 0
	for _, g := range groups {
		fmt.Printf("%s\n", g.agent)
		for _, r := range g.rows {
			mark := "no "
			if r.wired {
				mark = "ok "
			} else {
				missing++
			}
			fmt.Printf("  %s %-18s %s\n", mark, r.name, r.detail)
		}
		fmt.Println()
	}

	if missing > 0 {
		fmt.Printf("%d signal(s) not reaching amac. `amac hooks -install` wires Claude Code;\n", missing)
		fmt.Printf("the Codex ones are a line in ~/.codex/config.toml and ~/.tmux.conf.\n")
	}
	return nil
}

// claudeWiring is every hook amac wants from Claude Code, and what each one is
// for. Two of them ring the phone. The rest exist so the board can say what a
// session is doing instead of "unknown", which is all it could say for a
// session amac did not start.
var claudeWiring = []struct{ event, purpose string }{
	{"Notification", "blocked: waiting on you, and says what for"},
	{"Stop", "idle: turn finished, carries what it said"},
	{"UserPromptSubmit", "working"},
	{"PostToolUse", "working: clears blocked once you approve"},
	{"SessionStart", "idle"},
	{"SessionEnd", "ended"},
}

func claudeWiringStatus(path string) []wiring {
	hooks := readClaudeHooks(path)
	out := make([]wiring, 0, len(claudeWiring))
	for _, w := range claudeWiring {
		row := wiring{name: w.event, detail: w.purpose}
		for _, g := range hooks[w.event] {
			if strings.Contains(string(g), claudeHookMarker) {
				row.wired = true
				break
			}
		}
		if !row.wired {
			row.detail = "not wired - " + w.purpose
		}
		out = append(out, row)
	}
	return out
}

func codexWiringStatus() wiring {
	home, _ := os.UserHomeDir()
	b, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if err != nil {
		return wiring{name: "notify", detail: "no ~/.codex/config.toml"}
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "notify") {
			continue
		}
		if strings.Contains(line, "amac") {
			return wiring{name: "notify", wired: true, detail: "idle: turn finished, carries what it said"}
		}
		return wiring{name: "notify", detail: "notify is set, but not to amac"}
	}
	return wiring{name: "notify", detail: "no notify program set"}
}

// tmuxBellStatus reads the live tmux server rather than ~/.tmux.conf, because
// what is in the file and what the running server actually has are different
// questions, and only the second one delivers a notification.
func tmuxBellStatus() wiring {
	out, err := exec.Command("tmux", "show-hooks", "-g").Output()
	if err != nil {
		return wiring{name: "alert-bell", detail: "tmux server not running"}
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "alert-bell") {
			if strings.Contains(line, "amac") {
				return wiring{name: "alert-bell", wired: true, detail: "blocked: the only signal Codex has for it"}
			}
			return wiring{name: "alert-bell", detail: "set, but not to amac"}
		}
	}
	return wiring{name: "alert-bell", detail: "no alert-bell hook: Codex approvals will be silent"}
}

// ----------------------------------------------------------------- install --

// claudeHookMarker identifies the entries this installer owns. Matching on the
// command means a re-install replaces amac's own hook and leaves every other
// hook on the same event alone.
const claudeHookMarker = "attention -claude"

type hookCmd struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
	Async   bool   `json:"async,omitempty"`
}

type hookGroup struct {
	Hooks []hookCmd `json:"hooks"`
}

func readClaudeHooks(path string) map[string][]json.RawMessage {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal(b, &doc) != nil {
		return nil
	}
	var hooks map[string][]json.RawMessage
	if json.Unmarshal(doc["hooks"], &hooks) != nil {
		return nil
	}
	return hooks
}

// installClaudeHooks adds amac to Claude Code's hooks without taking anything
// out that it did not put there.
//
// Every existing hook group is carried across as the raw JSON it already was.
// Decoding them into a struct and re-encoding would silently drop any field
// this build does not know about, which for a settings file that gains fields
// between releases is a data-loss bug waiting for a release to trigger it. The
// only groups removed are amac's own, so re-running is idempotent.
//
// agentmon's hooks are deliberately left in place. They feed the `agent ls`
// tooling that is still in daily use, and their push is suppressed by their
// own presence check nearly always, so leaving them costs a duplicate banner
// approximately never and removing them would break a tool nobody asked to
// have broken.
func installClaudeHooks(path, exe string) (changed []string, backup string, err error) {
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, "", err
	}
	doc := map[string]json.RawMessage{}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &doc); err != nil {
			return nil, "", fmt.Errorf("%s is not valid JSON: %w", path, err)
		}
	}
	hooks := map[string][]json.RawMessage{}
	if raw, ok := doc["hooks"]; ok && len(raw) > 0 {
		if err := json.Unmarshal(raw, &hooks); err != nil {
			return nil, "", fmt.Errorf("%s: hooks: %w", path, err)
		}
	}

	for _, w := range claudeWiring {
		mine, err := json.Marshal(hookGroup{Hooks: []hookCmd{{
			Type:    "command",
			Command: fmt.Sprintf("%s attention -claude -quiet", shellQuoteIfNeeded(exe)),
			// Notification and Stop can make a Discord round trip; the rest
			// write one row at most. Async either way: a hook that makes the
			// agent wait on the network is a hook that will get uninstalled.
			Timeout: 15,
			Async:   true,
		}}})
		if err != nil {
			return nil, "", err
		}

		kept := make([]json.RawMessage, 0, len(hooks[w.event])+1)
		replaced := false
		for _, g := range hooks[w.event] {
			if strings.Contains(string(g), claudeHookMarker) {
				replaced = true
				continue
			}
			kept = append(kept, g)
		}
		if !replaced {
			changed = append(changed, w.event+"  "+w.purpose)
		}
		hooks[w.event] = append(kept, mine)
	}

	if len(changed) == 0 {
		return nil, "", nil
	}

	hooksRaw, err := json.Marshal(hooks)
	if err != nil {
		return nil, "", err
	}
	doc["hooks"] = hooksRaw

	out, err := marshalIndentOrdered(doc)
	if err != nil {
		return nil, "", err
	}

	// Back up before writing, always. This file carries permissions and model
	// settings that are not amac's to lose, and "restore the one before" has
	// to be possible without a git history the file does not have.
	if len(b) > 0 {
		backup = fmt.Sprintf("%s.amac-%s.bak", path, time.Now().Format("20060102-150405"))
		if err := os.WriteFile(backup, b, 0o600); err != nil {
			return nil, "", fmt.Errorf("backup: %w", err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, "", err
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return nil, "", err
	}
	return changed, backup, nil
}

// marshalIndentOrdered writes the document with its top-level keys sorted and
// every value it did not touch byte-identical to what was read. Key order in
// JSON carries no meaning, and preserving it would mean hand-rolling a parser;
// preserving the *values* is what actually matters, and json.RawMessage does
// that for free.
func marshalIndentOrdered(doc map[string]json.RawMessage) ([]byte, error) {
	keys := make([]string, 0, len(doc))
	for k := range doc {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var sb strings.Builder
	sb.WriteString("{\n")
	for i, k := range keys {
		key, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, doc[k], "  ", "  "); err != nil {
			return nil, err
		}
		fmt.Fprintf(&sb, "  %s: %s", key, pretty.String())
		if i < len(keys)-1 {
			sb.WriteString(",")
		}
		sb.WriteString("\n")
	}
	sb.WriteString("}\n")
	return []byte(sb.String()), nil
}

// shellQuoteIfNeeded guards the one path shape that breaks a hook command
// silently: a binary living under a directory with a space in it.
func shellQuoteIfNeeded(s string) string {
	if !strings.ContainsAny(s, " \t\"'$") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
