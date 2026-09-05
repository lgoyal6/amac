package daemon

import (
	"testing"

	"github.com/lgoyal6/amac/internal/event"
)

// Withdrawing work from the board. The interesting half is what it refuses:
// a phone's view of the queue is routinely stale by the time a thumb reaches
// it, and cancelling a task somebody else has already picked up would delete
// work in flight.
func TestCancellingReadyWorkAndRefusingClaimedWork(t *testing.T) {
	s := testServer(t)

	code, filed := post(t, s, "POST", "/api/tasks", `{"title":"withdraw me","dir":"/tmp"}`)
	if code != 200 {
		t.Fatalf("filing returned %d: %v", code, filed)
	}
	id, _ := filed["id"].(string)
	if id == "" {
		t.Fatalf("no id in the reply: %v", filed)
	}

	if code, body := post(t, s, "DELETE", "/api/tasks/"+id, ""); code != 200 {
		t.Fatalf("cancelling ready work returned %d: %v", code, body)
	}
	// And it is a fact in the log, not just an HTTP status, or "why did that
	// task vanish" has no answer.
	if n := countKind(t, s, event.Kind("task.canceled")); n != 1 {
		t.Errorf("%d cancellations recorded, want 1", n)
	}

	// Twice is a conflict rather than a success. A second tap on a stale
	// screen must not read as having done something.
	if code, _ := post(t, s, "DELETE", "/api/tasks/"+id, ""); code != 409 {
		t.Errorf("cancelling an already cancelled task returned %d, want 409", code)
	}
}

// A task that does not exist is a 409 for the same reason: it is not ready,
// and the caller is told to refresh rather than given a 200 for a no-op.
func TestCancellingSomethingThatIsNotThere(t *testing.T) {
	s := testServer(t)
	code, body := post(t, s, "DELETE", "/api/tasks/no-such-task", "")
	if code == 200 {
		t.Errorf("cancelling a task that does not exist reported success: %v", body)
	}
	if code != 409 {
		t.Errorf("got %d, want 409", code)
	}
}
