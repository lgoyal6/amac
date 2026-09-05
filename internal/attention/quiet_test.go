package attention

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lgoyal6/amac/internal/event"
)

func quietLog(t *testing.T) *event.Log {
	t.Helper()
	l, err := event.Open(filepath.Join(t.TempDir(), "q.db"), event.Relaxed)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

// addAt writes an event and backdates it, which is how a turn of a given
// length is expressed: the gap since the session's previous event is the turn.
func addAt(t *testing.T, l *event.Log, kind event.Kind, session string, ago time.Duration) {
	t.Helper()
	e, err := event.New(kind, "test", session, map[string]any{"state": "working"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := l.Append(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}
	when := time.Now().UTC().Add(-ago).Format(time.RFC3339Nano)
	if _, err := l.DB().Exec(`UPDATE events SET at = ? WHERE seq = ?`, when, got.Seq); err != nil {
		t.Fatal(err)
	}
}

// The rule, and the measurement behind it. 81% of everything amac delivered was
// a turn-complete, and the median turn behind one took under a minute: most of
// the volume was announcing something instant.
func TestAnInstantTurnIsNotWorthANotification(t *testing.T) {
	l := quietLog(t)
	addAt(t, l, event.KindSessionState, "am-claude-1", 20*time.Second)

	out := decide(context.Background(), l, Notice{
		Session: "am-claude-1", Agent: "claude", Reason: TurnComplete})
	if out.Sent {
		t.Error("a twenty second turn was pushed")
	}
	if !strings.Contains(out.Why, "worth interrupting") {
		t.Errorf("why = %q, which does not say why it was held", out.Why)
	}
}

// And the case the threshold exists to protect. A blanket cut would be quieter
// and would throw this away, which is the only turn-complete worth a push.
func TestWorkLeftRunningForAWhileStillNotifies(t *testing.T) {
	l := quietLog(t)
	addAt(t, l, event.KindSessionState, "am-claude-1", 90*time.Minute)

	out := decide(context.Background(), l, Notice{
		Session: "am-claude-1", Agent: "claude", Reason: TurnComplete})
	if !out.Sent {
		t.Errorf("a ninety minute turn was suppressed: %s", out.Why)
	}
}

// Nothing about how long an agent worked changes whether it is now stuck
// waiting for a person. This is the 14 a day that are actually acted on.
func TestABlockedSessionIsNeverHeldBackForBeingQuick(t *testing.T) {
	l := quietLog(t)
	addAt(t, l, event.KindSessionState, "am-claude-1", time.Second)

	out := decide(context.Background(), l, Notice{
		Session: "am-claude-1", Agent: "claude", Reason: "wants-attention"})
	if !out.Sent {
		t.Errorf("a blocked session was suppressed for being quick: %s", out.Why)
	}
}

// A rule that goes silent when it cannot measure is a rule that swallows real
// alerts the first time something upstream changes shape. With no history to
// measure a turn against, the notification goes out.
func TestASessionWithNoHistoryIsNotifiedAbout(t *testing.T) {
	l := quietLog(t)
	out := decide(context.Background(), l, Notice{
		Session: "am-brand-new", Agent: "claude", Reason: TurnComplete})
	if !out.Sent {
		t.Errorf("a session with no history was suppressed: %s", out.Why)
	}
}

// The threshold is a setting because it is a judgement about one person's
// attention, and zero turns the rule off rather than meaning "suppress
// everything", which is the reading that would silence the whole stream.
func TestTheThresholdIsASettingAndZeroDisablesIt(t *testing.T) {
	l := quietLog(t)
	addAt(t, l, event.KindSessionState, "am-claude-1", 20*time.Second)
	n := Notice{Session: "am-claude-1", Agent: "claude", Reason: TurnComplete}

	old := MinTurn
	t.Cleanup(func() { MinTurn = old })

	MinTurn = 0
	if out := decide(context.Background(), l, n); !out.Sent {
		t.Errorf("zero suppressed instead of disabling the rule: %s", out.Why)
	}
	MinTurn = 5 * time.Second
	if out := decide(context.Background(), l, n); !out.Sent {
		t.Errorf("a twenty second turn was held against a five second threshold: %s", out.Why)
	}
}
