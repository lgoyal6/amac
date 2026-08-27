package attention

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The two hooks that are worth a phone banner, and the ones that are only
// worth a card on the board. Getting this split wrong in either direction is
// how a channel gets muted or a board goes stale.
func TestHookSplitsStateFromDelivery(t *testing.T) {
	for _, tc := range []struct {
		event  string
		state  string
		notify bool
		reason string
	}{
		{"Notification", StateBlocked, true, WantsYou},
		{"Stop", StateIdle, true, TurnComplete},
		{"UserPromptSubmit", StateWorking, false, ""},
		{"PostToolUse", StateWorking, false, ""},
		{"SessionStart", StateIdle, false, ""},
		{"SessionEnd", StateEnded, false, ""},
		// A hook event added by a future Claude Code release must degrade to
		// silence, not to an error that makes the agent look broken.
		{"SomethingNewInTheNextRelease", "", false, ""},
	} {
		sig, err := FromHook([]byte(`{"hook_event_name":"` + tc.event + `","session_id":"abc"}`))
		if err != nil {
			t.Fatalf("%s: %v", tc.event, err)
		}
		if sig.State != tc.state || sig.Notify != tc.notify {
			t.Errorf("%s: state=%q notify=%v, want state=%q notify=%v",
				tc.event, sig.State, sig.Notify, tc.state, tc.notify)
		}
		if sig.Notice.Reason != tc.reason {
			t.Errorf("%s: reason=%q, want %q", tc.event, sig.Notice.Reason, tc.reason)
		}
	}
}

// Claude says what it is waiting for. Codex cannot, and dropping the reason
// would throw away the one advantage this side has.
func TestNotificationCarriesWhatItIsWaitingFor(t *testing.T) {
	sig, err := FromHook([]byte(`{"hook_event_name":"Notification",
		"message":"Claude needs your permission to use Bash"}`))
	if err != nil {
		t.Fatal(err)
	}
	if sig.Notice.Message != "Claude needs your permission to use Bash" {
		t.Fatalf("got %q", sig.Notice.Message)
	}
	// A Notification with no message at all still has to read as a session
	// waiting on you rather than as an empty banner.
	sig, _ = FromHook([]byte(`{"hook_event_name":"Notification"}`))
	if sig.Notice.Message == "" {
		t.Fatal("an empty Notification must still say something")
	}
}

func writeTranscript(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLastAssistantMessage(t *testing.T) {
	path := writeTranscript(t,
		`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"do the thing"}]}}`,
		`{"type":"assistant","isSidechain":false,"message":{"role":"assistant","content":[{"type":"text","text":"first answer"}]}}`,
		`{"type":"assistant","isSidechain":false,"message":{"role":"assistant","content":[{"type":"text","text":"the last thing it said"}]}}`,
	)
	if got := LastAssistantMessage(path); got != "the last thing it said" {
		t.Fatalf("got %q", got)
	}
}

// A subagent's closing summary is not what the session said to you. Reporting
// one attributes a Task tool's output to the session that spawned it.
func TestSidechainIsNotTheSessionsAnswer(t *testing.T) {
	path := writeTranscript(t,
		`{"type":"assistant","isSidechain":false,"message":{"role":"assistant","content":[{"type":"text","text":"what the session said"}]}}`,
		`{"type":"assistant","isSidechain":true,"message":{"role":"assistant","content":[{"type":"text","text":"what a subagent said"}]}}`,
	)
	if got := LastAssistantMessage(path); got != "what the session said" {
		t.Fatalf("got %q, want the session's own message", got)
	}
}

// A turn that ended in a tool call has no text of its own. Reporting the empty
// string would send a banner with nothing in it.
func TestSkipsTurnsWithNoText(t *testing.T) {
	path := writeTranscript(t,
		`{"type":"assistant","isSidechain":false,"message":{"role":"assistant","content":[{"type":"text","text":"the real answer"}]}}`,
		`{"type":"assistant","isSidechain":false,"message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash"}]}}`,
	)
	if got := LastAssistantMessage(path); got != "the real answer" {
		t.Fatalf("got %q", got)
	}
}

// Transcripts here reach tens of megabytes, so only the tail is read. The seek
// lands mid-record and that half-line must be dropped rather than parsed.
func TestTailOfALargeTranscript(t *testing.T) {
	filler := strings.Repeat(
		`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"`+
			strings.Repeat("x", 900)+`"}]}}`+"\n", 900)
	path := filepath.Join(t.TempDir(), "big.jsonl")
	body := filler +
		`{"type":"assistant","isSidechain":false,"message":{"role":"assistant","content":[{"type":"text","text":"found at the end"}]}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); fi.Size() <= transcriptTail {
		t.Fatalf("fixture is %d bytes, needs to exceed the %d-byte tail", fi.Size(), transcriptTail)
	}
	if got := LastAssistantMessage(path); got != "found at the end" {
		t.Fatalf("got %q", got)
	}
}

func TestMissingTranscriptIsNotAFailure(t *testing.T) {
	if got := LastAssistantMessage("/nonexistent/transcript.jsonl"); got != "" {
		t.Fatalf("got %q", got)
	}
	if got := LastAssistantMessage(""); got != "" {
		t.Fatalf("got %q", got)
	}
}

// PostToolUse fires on every tool call. Writing "working" a thousand times a
// day would turn the log into a heartbeat and wake every live subscriber.
func TestOnlyStateChangesAreRecorded(t *testing.T) {
	log := testLog(t)
	ctx := context.Background()
	s := State{Session: "am-mint", Agent: "claude", State: StateWorking}

	if wrote, err := RecordState(ctx, log, s); err != nil || !wrote {
		t.Fatalf("first state must be recorded: wrote=%v err=%v", wrote, err)
	}
	if wrote, err := RecordState(ctx, log, s); err != nil || wrote {
		t.Fatalf("unchanged state must not be recorded: wrote=%v err=%v", wrote, err)
	}
	s.State = StateBlocked
	if wrote, err := RecordState(ctx, log, s); err != nil || !wrote {
		t.Fatalf("a change must be recorded: wrote=%v err=%v", wrote, err)
	}

	got, ok := CurrentState(ctx, log, "am-mint")
	if !ok || got.State != StateBlocked {
		t.Fatalf("current state = %+v, ok=%v", got, ok)
	}
	if all := States(ctx, log); all["am-mint"].State != StateBlocked {
		t.Fatalf("States() = %+v", all)
	}
}
