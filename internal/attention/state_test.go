package attention

import (
	"context"
	"testing"
)

// Not every writer knows whose session it is. The board records "answered from
// the board" when a key is sent from a phone, and "continued in Terminal" when
// a session is opened on the Mac; neither has any idea which login is running
// it, and both are the newest state afterwards. Taking them at their word
// would blank the account off a card that had one, so the session would be
// tagged, then untagged the moment it was touched from the board.
//
// The account is a property of the session, not of the event, so it is carried
// forward rather than re-asserted by every caller.
func TestAStateThatDoesNotKnowTheAccountKeepsTheKnownOne(t *testing.T) {
	log := testLog(t)
	ctx := context.Background()

	if _, err := RecordState(ctx, log, State{
		Session: "am-mint", Agent: "codex", Account: "codex-ish", State: StateWorking,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := RecordState(ctx, log, State{
		Session: "am-mint", Agent: "amac", State: StateIdle, Detail: "continued in Terminal",
	}); err != nil {
		t.Fatal(err)
	}

	got, ok := CurrentState(ctx, log, "am-mint")
	if !ok {
		t.Fatal("no state recorded")
	}
	if got.Account != "codex-ish" {
		t.Fatalf("account = %q after a writer that did not know it, want codex-ish", got.Account)
	}
	if got.State != StateIdle {
		t.Fatalf("state = %q, want the newer one to win", got.State)
	}
}

// An account that actually changes must still be recorded. Carrying forward is
// for writers with nothing to say, not a lock on the first answer.
func TestAKnownAccountStillOverwrites(t *testing.T) {
	log := testLog(t)
	ctx := context.Background()

	if _, err := RecordState(ctx, log, State{
		Session: "am-mint", Agent: "codex", Account: "codex", State: StateWorking,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := RecordState(ctx, log, State{
		Session: "am-mint", Agent: "codex", Account: "codex-ish", State: StateIdle,
	}); err != nil {
		t.Fatal(err)
	}
	if got, _ := CurrentState(ctx, log, "am-mint"); got.Account != "codex-ish" {
		t.Fatalf("account = %q, want the newer codex-ish", got.Account)
	}
}
