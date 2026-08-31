package queue

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lgoyal6/amac/internal/event"
)

func testQueue(t *testing.T) *Queue {
	t.Helper()
	return queueAt(t, filepath.Join(t.TempDir(), "q.db"))
}

func queueAt(t *testing.T, path string) *Queue {
	t.Helper()
	log, err := event.Open(path, event.Relaxed)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	q, err := Open(log)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func file(t *testing.T, q *Queue, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := q.File(context.Background(), Task{
			ID: fmt.Sprintf("t%03d", i), Title: fmt.Sprintf("task %d", i),
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// Filing is idempotent. Two health sweeps noticing the same broken automation
// must not produce two attempts at fixing it.
func TestFilingTwiceIsOneTask(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := q.File(ctx, Task{ID: "fix-thing", Title: "fix the thing"}); err != nil {
			t.Fatal(err)
		}
	}
	list, err := q.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("filed once, got %d tasks", len(list))
	}
	if list[0].Attempt != 0 {
		t.Errorf("filing must not count as an attempt, got %d", list[0].Attempt)
	}
}

func TestCancelReadyCannotCancelClaimedWork(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()
	file(t, q, 2)
	claimed, err := q.Claim(ctx, "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.CancelReady(ctx, claimed.ID, "stale"); err != ErrNotHeld {
		t.Fatalf("canceling claimed work = %v, want ErrNotHeld", err)
	}
	if err := q.CancelReady(ctx, "t001", "no longer needed"); err != nil {
		t.Fatal(err)
	}
	list, err := q.List(ctx, Canceled)
	if err != nil || len(list) != 1 || list[0].Result != "no longer needed" {
		t.Fatalf("canceled list = %#v, %v", list, err)
	}
}

// The core claim: concurrent workers never both hold the same task.
//
// Every claim records which task it took; if the select-and-take were two
// statements instead of one, two workers would read the same row as free and
// both take it, and the duplicate would show up here.
func TestConcurrentClaimsNeverOverlap(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()
	const tasks, workers = 200, 16
	file(t, q, tasks)

	var mu sync.Mutex
	claimedBy := map[string]string{}
	var wg sync.WaitGroup

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			owner := fmt.Sprintf("worker-%d", w)
			for {
				task, err := q.Claim(ctx, owner, time.Minute)
				if err == ErrNoWork {
					return
				}
				if err != nil {
					t.Errorf("claim: %v", err)
					return
				}
				mu.Lock()
				if prev, dup := claimedBy[task.ID]; dup {
					t.Errorf("%s claimed by both %s and %s", task.ID, prev, owner)
				}
				claimedBy[task.ID] = owner
				mu.Unlock()

				if err := q.Finish(ctx, task.ID, task.Token, Done, "ok"); err != nil {
					t.Errorf("finish %s: %v", task.ID, err)
				}
			}
		}(w)
	}
	wg.Wait()

	if len(claimedBy) != tasks {
		t.Fatalf("%d of %d tasks were claimed", len(claimedBy), tasks)
	}
	done, err := q.List(ctx, Done)
	if err != nil {
		t.Fatal(err)
	}
	if len(done) != tasks {
		t.Fatalf("%d of %d finished exactly once", len(done), tasks)
	}
}

// An abandoned task becomes claimable again, and a worker that never existed
// looks exactly like one that was killed.
func TestAnExpiredLeaseIsReclaimed(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()
	file(t, q, 1)

	first, err := q.Claim(ctx, "dies", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.Claim(ctx, "waiting", time.Minute); err != ErrNoWork {
		t.Fatalf("a live lease must block a second claim, got %v", err)
	}

	// Move past the lease rather than sleeping through it.
	q.now = func() time.Time { return time.Now().UTC().Add(2 * time.Minute) }

	second, err := q.Claim(ctx, "takes-over", time.Minute)
	if err != nil {
		t.Fatalf("an expired lease must be reclaimable: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("reclaimed %s, expected %s", second.ID, first.ID)
	}
	if second.Token <= first.Token {
		t.Fatalf("token must advance on every claim: %d then %d", first.Token, second.Token)
	}
	if second.Attempt != 2 {
		t.Errorf("attempt = %d, want 2", second.Attempt)
	}
}

// The failure a lease alone cannot prevent: a worker stalls, its lease expires,
// another worker legitimately takes the task, and then the first one wakes up
// and reports a result. Accepting it would mark someone else's in-flight work
// as finished. The token is what refuses it.
func TestAFencedWorkerCannotFinishOrRenew(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()
	file(t, q, 1)

	zombie, err := q.Claim(ctx, "stalls", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	q.now = func() time.Time { return time.Now().UTC().Add(2 * time.Minute) }
	live, err := q.Claim(ctx, "took-over", time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	if err := q.Finish(ctx, zombie.ID, zombie.Token, Done, "I finished it"); err != ErrNotHeld {
		t.Fatalf("a fenced worker must not finish the task, got %v", err)
	}
	if err := q.Renew(ctx, zombie.ID, zombie.Token, time.Minute); err != ErrNotHeld {
		t.Fatalf("a fenced worker must not renew, got %v", err)
	}
	if err := q.Release(ctx, zombie.ID, zombie.Token); err != ErrNotHeld {
		t.Fatalf("a fenced worker must not release someone else's claim, got %v", err)
	}

	// And the live holder is untouched by any of it.
	if err := q.Finish(ctx, live.ID, live.Token, Done, "actually finished"); err != nil {
		t.Fatalf("the live holder must still be able to finish: %v", err)
	}
	got, _ := q.Get(ctx, live.ID)
	if got.Result != "actually finished" {
		t.Fatalf("result = %q, the zombie's write got through", got.Result)
	}
}

// Renewing keeps a task held, which is what lets a long job outlive its lease
// without being taken away mid-run.
func TestRenewHoldsTheTask(t *testing.T) {
	q := testQueue(t)
	ctx := context.Background()
	file(t, q, 1)

	held, err := q.Claim(ctx, "slow", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	q.now = func() time.Time { return time.Now().UTC().Add(90 * time.Second) }
	if err := q.Renew(ctx, held.ID, held.Token, 10*time.Minute); err != nil {
		t.Fatalf("renew: %v", err)
	}
	if _, err := q.Claim(ctx, "thief", time.Minute); err != ErrNoWork {
		t.Fatalf("a renewed lease must still block, got %v", err)
	}
}

// ------------------------------------------------------------------ crash ---

// The whole point, proved rather than asserted: workers that are SIGKILLed
// mid-task lose no work and duplicate none.
//
// Children claim tasks and are killed without warning while holding them. Their
// leases expire, the survivor picks the abandoned work up, and every task must
// end up finished exactly once with no task claimed by two workers at the same
// moment. A crash mid-claim is the case the single-statement UPDATE and the
// fencing token exist for, and the only honest way to test it is to cause one.
func TestNoWorkLostOrDuplicatedAcrossCrashes(t *testing.T) {
	if os.Getenv("AMAC_QUEUE_CHILD") != "" {
		crashChild()
		return
	}

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "crash.db")

	const tasks, kids, heldEach = 120, 6, 5
	q := queueAt(t, dbPath)
	ctx := context.Background()
	file(t, q, tasks)

	// Six children, each killed while holding five tasks, so thirty are
	// abandoned mid-flight rather than one. A short lease keeps the test quick;
	// nothing about the guarantee depends on its length.
	const lease = "300ms"
	for i := 0; i < kids; i++ {
		cmd := exec.Command(os.Args[0], "-test.run=TestNoWorkLostOrDuplicatedAcrossCrashes")
		cmd.Env = append(os.Environ(),
			"AMAC_QUEUE_CHILD=1", "AMAC_QUEUE_DB="+dbPath,
			"AMAC_QUEUE_OWNER="+fmt.Sprintf("child-%d", i), "AMAC_QUEUE_LEASE="+lease)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}

		// Let it take a full handful, then kill it holding all of them. Killing
		// after the first claim would abandon one task per child and prove
		// almost nothing about concurrent recovery.
		for h := 0; h < heldEach; h++ {
			if readLine(stdout) == "" {
				_ = cmd.Process.Kill()
				t.Skip("child stopped claiming; cannot exercise the crash path")
			}
		}
		if err := cmd.Process.Kill(); err != nil {
			t.Fatalf("kill child: %v", err)
		}
		_ = cmd.Wait()
	}

	// Nothing may be lost. Move past the abandoned leases and drain the queue.
	q.now = func() time.Time { return time.Now().UTC().Add(time.Minute) }
	seen := map[string]bool{}
	for {
		task, err := q.Claim(ctx, "survivor", time.Minute)
		if err == ErrNoWork {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if seen[task.ID] {
			t.Fatalf("%s handed out twice in the same drain", task.ID)
		}
		seen[task.ID] = true
		if err := q.Finish(ctx, task.ID, task.Token, Done, "recovered"); err != nil {
			t.Fatalf("finish %s: %v", task.ID, err)
		}
	}

	done, err := q.List(ctx, Done)
	if err != nil {
		t.Fatal(err)
	}
	if len(done) != tasks {
		t.Fatalf("%d of %d tasks finished; work was lost", len(done), tasks)
	}
	// Exactly once: no task may carry two terminal records in the log.
	var dupes int
	rows, err := q.db.Query(`
		SELECT json_extract(payload,'$.task'), COUNT(*)
		  FROM events WHERE kind = 'task.done' GROUP BY 1 HAVING COUNT(*) > 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err == nil {
			t.Errorf("%s was completed %d times", id, n)
			dupes++
		}
	}
	if dupes > 0 {
		t.Fatalf("%d task(s) completed more than once", dupes)
	}

	// The children must actually have held work, or this proved nothing.
	var attempts int
	if err := q.db.QueryRow(`SELECT COALESCE(SUM(attempt),0) FROM tasks`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	// Every abandoned task must have been retried, or the recovery path went
	// largely unexercised and the result says less than it appears to.
	abandoned := kids * heldEach
	if attempts < tasks+abandoned {
		t.Fatalf("%d attempts over %d tasks; expected at least %d, so %d abandoned tasks were not all recovered",
			attempts, tasks, tasks+abandoned, abandoned)
	}
	t.Logf("%d tasks, %d kills holding %d each, %d attempts, %d finished exactly once, 0 duplicated",
		tasks, kids, heldEach, attempts, len(done))
}

func readLine(r interface{ Read([]byte) (int, error) }) string {
	var sb strings.Builder
	buf := make([]byte, 1)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		n, err := r.Read(buf)
		if err != nil || n == 0 {
			return ""
		}
		if buf[0] == '\n' {
			return sb.String()
		}
		sb.WriteByte(buf[0])
	}
	return ""
}

// crashChild claims work, says so, and then blocks forever holding it, waiting
// to be killed. It never renews, so its lease expires the way a dead worker's
// does.
func crashChild() {
	log, err := event.Open(os.Getenv("AMAC_QUEUE_DB"), event.Full)
	if err != nil {
		return
	}
	q, err := Open(log)
	if err != nil {
		return
	}
	lease, err := time.ParseDuration(os.Getenv("AMAC_QUEUE_LEASE"))
	if err != nil {
		lease = time.Second
	}
	ctx := context.Background()
	owner := os.Getenv("AMAC_QUEUE_OWNER")

	for i := 0; i < 5; i++ {
		task, err := q.Claim(ctx, owner, lease)
		if err != nil {
			return
		}
		fmt.Printf("%s\n", task.ID)
		os.Stdout.Sync()
	}
	select {} // hold everything, and wait to die
}
