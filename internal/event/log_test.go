package event

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

func mustOpen(t *testing.T, path string, d Durability) *Log {
	t.Helper()
	l, err := Open(path, d)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func appendN(t *testing.T, l *Log, n int) {
	t.Helper()
	ctx := context.Background()
	for i := range n {
		ev, err := New(KindDaemon, "test", "s1", map[string]any{"i": i})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := l.Append(ctx, ev); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
}

func TestSequenceIsTotalOrder(t *testing.T) {
	l := mustOpen(t, filepath.Join(t.TempDir(), "e.db"), Full)
	appendN(t, l, 50)

	evs, err := l.Since(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 50 {
		t.Fatalf("got %d events, want 50", len(evs))
	}
	for i, e := range evs {
		if e.Seq != int64(i+1) {
			t.Fatalf("event %d has seq %d; sequences must be gapless and ordered", i, e.Seq)
		}
	}
}

// Concurrent writers must still produce a total order with no duplicate or
// skipped sequence numbers. The whole system uses seq as a join key and as the
// replay cursor, so a duplicate is silent data corruption.
func TestConcurrentAppendsKeepSequenceUnique(t *testing.T) {
	l := mustOpen(t, filepath.Join(t.TempDir(), "e.db"), Relaxed)

	const writers, each = 8, 40
	var wg sync.WaitGroup
	seqs := make(chan int64, writers*each)

	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				ev, _ := New(KindDaemon, "test", fmt.Sprintf("w%d", w), map[string]any{"i": i})
				got, err := l.Append(context.Background(), ev)
				if err != nil {
					t.Errorf("append: %v", err)
					return
				}
				seqs <- got.Seq
			}
		}()
	}
	wg.Wait()
	close(seqs)

	seen := map[int64]bool{}
	for s := range seqs {
		if seen[s] {
			t.Fatalf("sequence %d handed out twice", s)
		}
		seen[s] = true
	}
	if len(seen) != writers*each {
		t.Fatalf("got %d unique sequences, want %d", len(seen), writers*each)
	}
}

// A subscriber that stops reading must be dropped, never allowed to stall the
// writer. The log is the durable record; a live subscription is a convenience.
func TestSlowSubscriberDoesNotBlockWriter(t *testing.T) {
	l := mustOpen(t, filepath.Join(t.TempDir(), "e.db"), Relaxed)

	_, unsub := l.Subscribe(1) // deliberately never drained
	defer unsub()

	done := make(chan struct{})
	go func() {
		appendN(t, l, 200)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("writer blocked behind a subscriber that stopped reading")
	}

	n, err := l.Count(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 200 {
		t.Fatalf("wrote %d events, want 200: dropping a subscriber must not drop data", n)
	}
}

func TestReplayFromSequence(t *testing.T) {
	l := mustOpen(t, filepath.Join(t.TempDir(), "e.db"), Full)
	appendN(t, l, 30)

	evs, err := l.Since(context.Background(), 20, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 10 {
		t.Fatalf("replay from 20 gave %d events, want 10", len(evs))
	}
	if evs[0].Seq != 21 {
		t.Fatalf("replay starts at %d, want 21 (since is exclusive)", evs[0].Seq)
	}
}

func TestPayloadSurvivesRoundTrip(t *testing.T) {
	l := mustOpen(t, filepath.Join(t.TempDir(), "e.db"), Full)

	type payload struct {
		Title string `json:"title"`
		Nest  struct {
			Cost float64 `json:"cost"`
		} `json:"nest"`
	}
	in := payload{Title: "write hello.txt"}
	in.Nest.Cost = 0.245548

	ev, err := New(KindSessionUpdate, "test", "s1", in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Append(context.Background(), ev); err != nil {
		t.Fatal(err)
	}

	evs, _ := l.Since(context.Background(), 0, 10)
	var out payload
	if err := json.Unmarshal(evs[0].Payload, &out); err != nil {
		t.Fatalf("payload did not survive: %v", err)
	}
	if out.Title != in.Title || out.Nest.Cost != in.Nest.Cost {
		t.Fatalf("got %+v, want %+v", out, in)
	}
}

// TestCrashDurability is the one that matters.
//
// A log whose entire value is being trustworthy after a crash has to be tested
// by actually crashing. This forks a helper that appends with Full durability
// and reports each acknowledged sequence, SIGKILLs it mid-write, then reopens
// the database and asserts that every acknowledged event is still there, that
// nothing is torn, and that the sequence continues correctly for new writes.
//
// SIGKILL specifically: no deferred Close, no flush, no cleanup. That is the
// power-loss case as closely as userspace can reproduce it.
func TestCrashDurability(t *testing.T) {
	if os.Getenv("AMAC_CRASH_CHILD") != "" {
		crashChild()
		return
	}

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "crash.db")

	cmd := exec.Command(os.Args[0], "-test.run=TestCrashDurability")
	cmd.Env = append(os.Environ(), "AMAC_CRASH_CHILD=1", "AMAC_CRASH_DB="+dbPath)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	// Read acknowledged sequences until we have enough, then kill without warning.
	acked := make([]int64, 0, 64)
	buf := make([]byte, 1)
	line := ""
	for len(acked) < 40 {
		n, err := stdout.Read(buf)
		if err != nil || n == 0 {
			break
		}
		if buf[0] != '\n' {
			line += string(buf[0])
			continue
		}
		if seq, err := strconv.ParseInt(line, 10, 64); err == nil {
			acked = append(acked, seq)
		}
		line = ""
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill child: %v", err)
	}
	_ = cmd.Wait()

	if len(acked) < 40 {
		t.Fatalf("child acknowledged only %d events before dying; test needs 40", len(acked))
	}

	// Reopen after the crash. This is the recovery path: SQLite replays its WAL.
	l2, err := Open(dbPath, Full)
	if err != nil {
		t.Fatalf("reopen after SIGKILL: %v", err)
	}
	defer l2.Close()

	ctx := context.Background()
	head, err := l2.Head(ctx)
	if err != nil {
		t.Fatalf("head after crash: %v", err)
	}

	last := acked[len(acked)-1]
	if head < last {
		t.Fatalf("lost acknowledged data: head=%d but seq %d was acknowledged to the caller", head, last)
	}

	// Every acknowledged event must be readable and intact, not just counted.
	evs, err := l2.Since(ctx, 0, 10000)
	if err != nil {
		t.Fatalf("read after crash: %v", err)
	}
	got := map[int64]bool{}
	for _, e := range evs {
		if e.Kind == "" || e.Source == "" || e.At.IsZero() {
			t.Fatalf("torn record at seq %d: %+v", e.Seq, e)
		}
		if len(e.Payload) > 0 && !json.Valid(e.Payload) {
			t.Fatalf("corrupt payload at seq %d", e.Seq)
		}
		got[e.Seq] = true
	}
	for _, s := range acked {
		if !got[s] {
			t.Fatalf("seq %d was acknowledged but is missing after the crash", s)
		}
	}

	// The log must remain writable, and must not reuse a sequence number.
	ev, _ := New(KindDaemon, "test", "after-crash", map[string]any{"ok": true})
	stored, err := l2.Append(ctx, ev)
	if err != nil {
		t.Fatalf("append after crash recovery: %v", err)
	}
	if stored.Seq <= head {
		t.Fatalf("sequence went backwards after recovery: %d <= %d", stored.Seq, head)
	}
}

// crashChild appends forever with Full durability, printing each sequence only
// after Append has returned, so the parent only ever treats acknowledged
// writes as durable.
func crashChild() {
	l, err := Open(os.Getenv("AMAC_CRASH_DB"), Full)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ctx := context.Background()
	for i := 0; ; i++ {
		ev, err := New(KindDaemon, "child", "crash", map[string]any{"i": i, "pad": "0123456789"})
		if err != nil {
			os.Exit(1)
		}
		stored, err := l.Append(ctx, ev)
		if err != nil {
			os.Exit(1)
		}
		fmt.Printf("%d\n", stored.Seq)
	}
}

// The fsync policy is the one knob that trades data loss against throughput.
// Choosing Full is only defensible if the cost is known, so measure it:
//
//	go test ./internal/event/ -bench=Append -benchtime=300x -run=XXX
func BenchmarkAppendFull(b *testing.B)    { benchAppend(b, Full) }
func BenchmarkAppendRelaxed(b *testing.B) { benchAppend(b, Relaxed) }

func benchAppend(b *testing.B, d Durability) {
	l, err := Open(filepath.Join(b.TempDir(), "bench.db"), d)
	if err != nil {
		b.Fatal(err)
	}
	defer l.Close()

	ctx := context.Background()
	ev, _ := New(KindSessionUpdate, "bench", "s1", map[string]any{
		"update": "agent_message_chunk", "text": "a representative chunk of agent output",
	})
	b.ResetTimer()
	for range b.N {
		if _, err := l.Append(ctx, ev); err != nil {
			b.Fatal(err)
		}
	}
}
