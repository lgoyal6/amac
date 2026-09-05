package main

import (
	"strings"
	"testing"
)

// The bug this covers reported a wired account as unwired. Codex takes exactly
// one notify program, so chaining to another one means passing it as an
// argument, which makes `notify` a multi-line array; reading only the line the
// key sits on saw "notify = [" and no mention of amac.
func TestNotifyValueSpansItsWholeArray(t *testing.T) {
	const doc = `model = "gpt-5.6-sol"
notify = [
    "/Applications/Something.app/Contents/MacOS/Client",
    "turn-ended",
    "--previous-notify",
    '["/Users/me/.local/bin/amac-notify-codex","turn-ended"]',
]

[projects."/Users/me"]
trust_level = "trusted"
`
	got, ok := tomlValue(doc, "notify")
	if !ok {
		t.Fatal("notify not found")
	}
	if !strings.Contains(got, "amac-notify-codex") {
		t.Errorf("value stopped short of the chained program:\n%s", got)
	}
	if strings.Contains(got, "trust_level") {
		t.Errorf("value ran past the closing bracket:\n%s", got)
	}
}

func TestNotifyValueOnOneLine(t *testing.T) {
	got, ok := tomlValue("notify = \"/usr/local/bin/amac\"\n", "notify")
	if !ok || !strings.Contains(got, "amac") {
		t.Fatalf("got %q, %v", got, ok)
	}
}

// A key that merely starts with the same letters is a different key, and
// matching it would report the wrong program as the notify hook.
func TestPrefixIsNotTheKey(t *testing.T) {
	if _, ok := tomlValue("notify_style = \"quiet\"\n", "notify"); ok {
		t.Error("notify_style matched as notify")
	}
	if _, ok := tomlValue("model = \"x\"\n", "notify"); ok {
		t.Error("found a key that is not there")
	}
}

// Two agents outside tmux must not both be nobody. File claims are per session,
// so a shared empty identity would let them hold the same file by both failing
// to identify themselves, which is the bug this replaced: the first end to end
// run had agent A and agent B both claim the same path successfully.
func TestCallerSessionPrefersAnExplicitName(t *testing.T) {
	t.Setenv("AMAC_SESSION", "agent-A")
	t.Setenv("TMUX_PANE", "")
	if got := callerSession(); got != "agent-A" {
		t.Errorf("callerSession() = %q, want agent-A", got)
	}
	t.Setenv("AMAC_SESSION", "  ")
	if got := callerSession(); got != "" {
		t.Errorf("blank name should not become an identity, got %q", got)
	}
}
