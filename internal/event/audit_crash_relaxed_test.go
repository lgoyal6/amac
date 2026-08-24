package event

// Added by the 2026-08-17 resume audit as a COMPARISON ARM for
// TestCrashDurability. That test proves Full survives SIGKILL; it says nothing
// about what Relaxed loses, so the durability claim has no baseline. This runs
// the identical procedure with Relaxed and REPORTS the loss instead of failing,
// so the two arms can be compared on the same machine in the same session.
//
// What this can and cannot show. SIGKILL ends the process but does not clear
// the OS page cache, so writes Relaxed already handed to the kernel are still
// there on reopen. Measured loss is 0, repeatably, and that is the result
// rather than a flaw in the procedure: it is evidence for the first half of
// Relaxed's doc comment, "survives process kill". The second half, "can lose
// the tail on machine crash", needs power loss or a kernel panic to observe and
// no test on this machine can stage one. So Relaxed's exposure is bounded to
// machine death, and this is the arm that shows it.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

func TestAuditCrashRelaxed(t *testing.T) {
	if os.Getenv("AMAC_AUDIT_RELAXED_CHILD") != "" {
		auditRelaxedChild()
		return
	}

	dbPath := filepath.Join(t.TempDir(), "crash-relaxed.db")
	cmd := exec.Command(os.Args[0], "-test.run=TestAuditCrashRelaxed")
	cmd.Env = append(os.Environ(), "AMAC_AUDIT_RELAXED_CHILD=1", "AMAC_AUDIT_RELAXED_DB="+dbPath)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

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
		t.Skipf("child acknowledged only %d events before dying", len(acked))
	}

	l2, err := Open(dbPath, Relaxed)
	if err != nil {
		t.Fatalf("reopen after SIGKILL: %v", err)
	}
	defer l2.Close()

	ctx := context.Background()
	evs, err := l2.Since(ctx, 0, 10000)
	if err != nil {
		t.Fatalf("read after crash: %v", err)
	}
	got := map[int64]bool{}
	for _, e := range evs {
		got[e.Seq] = true
	}
	lost := 0
	for _, s := range acked {
		if !got[s] {
			lost++
		}
	}
	// Reported, not asserted: the point is the number, not a pass/fail.
	fmt.Printf("AUDIT_RELAXED acked=%d lost=%d\n", len(acked), lost)
}

func auditRelaxedChild() {
	l, err := Open(os.Getenv("AMAC_AUDIT_RELAXED_DB"), Relaxed)
	if err != nil {
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
