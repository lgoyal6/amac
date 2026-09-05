package machine

import (
	"context"
	"runtime"
	"testing"
	"time"
)

// Read is what the machine card draws and what the health sweep calls
// capacity. It had never been executed by a test, so nothing checked that the
// numbers it returns are internally consistent, which is the only property
// worth asserting about a live reading: the values themselves change between
// two calls and cannot be pinned.

func TestReadReturnsAConsistentPicture(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("vm_stat and this statfs layout are macOS")
	}
	s, err := Read(context.Background())
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if s.At.IsZero() {
		t.Error("a capacity reading without a timestamp cannot be called stale")
	}
	if time.Since(s.At) > time.Minute {
		t.Errorf("reading is %v old", time.Since(s.At))
	}

	// Disk. Used and free must account for the total, or the percentage is
	// drawn from numbers that do not describe one volume.
	if s.Disk.Total <= 0 {
		t.Fatalf("disk total = %d", s.Disk.Total)
	}
	if s.Disk.Used+s.Disk.Free != s.Disk.Total {
		t.Errorf("disk used+free = %d, total = %d", s.Disk.Used+s.Disk.Free, s.Disk.Total)
	}
	if s.Disk.Percent < 0 || s.Disk.Percent > 100 {
		t.Errorf("disk percent = %d", s.Disk.Percent)
	}
	if s.Disk.Mount == "" {
		t.Error("the reading should say which volume it measured")
	}

	// Memory. The split is the one Activity Monitor shows, because that is the
	// one a reader can act on, and every band has to fit inside the total.
	m := s.Memory
	if m.Total <= 0 {
		t.Fatalf("memory total = %d", m.Total)
	}
	for name, v := range map[string]int64{
		"wired": m.Wired, "app": m.App, "compressed": m.Compressed, "available": m.Available,
	} {
		if v < 0 {
			t.Errorf("%s = %d, negative", name, v)
		}
		if v > m.Total {
			t.Errorf("%s = %d exceeds total %d", name, v, m.Total)
		}
	}
	if m.Percent < 0 || m.Percent > 100 {
		t.Errorf("memory percent = %d", m.Percent)
	}
	// Counting the file cache as used is what makes naive memory graphs
	// frightening and useless on macOS, so available must not be zero on a
	// machine that is running.
	if m.Available == 0 {
		t.Error("available memory is zero, which would mean nothing is reclaimable")
	}

	// Swap is allowed to be absent: a machine with it disabled is not an error
	// and simply has no swap row.
	if s.Swap.Total > 0 {
		if s.Swap.Used+s.Swap.Free != s.Swap.Total {
			t.Errorf("swap used+free = %d, total = %d", s.Swap.Used+s.Swap.Free, s.Swap.Total)
		}
		if s.Swap.Percent < 0 || s.Swap.Percent > 100 {
			t.Errorf("swap percent = %d", s.Swap.Percent)
		}
	}
}

// The breakdown is help, not the reading. It is ranked, bounded, and its
// absence must not take the capacity numbers down with it.
func TestTopIsPresentAndOrdered(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("ps output differs")
	}
	s, err := Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Top) == 0 {
		t.Skip("no processes reported")
	}
	for i, p := range s.Top {
		if p.Name == "" || p.RSS <= 0 || p.Count < 1 {
			t.Errorf("top[%d] is incomplete: %+v", i, p)
		}
		if i > 0 && s.Top[i-1].RSS < p.RSS {
			t.Errorf("top is not ranked by memory: %+v", s.Top)
		}
	}
}

// The reading is cached for a moment because the dashboard polls, and three
// forks per poll for numbers that move on the order of seconds is a cost with
// no corresponding gain in truth.
func TestARepeatReadIsServedFromTheCache(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS only")
	}
	ctx := context.Background()
	first, err := Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !first.At.Equal(second.At) {
		t.Errorf("two reads a moment apart re-forked: %v then %v", first.At, second.At)
	}
}

// A volume that does not exist falls back to "/" rather than failing, because a
// Mac before the volume split still has a disk worth reporting.
func TestAMissingVolumeFallsBackToRoot(t *testing.T) {
	d, err := readDisk("/no/such/volume/anywhere")
	if err != nil {
		t.Fatalf("a missing mount should fall back, not fail: %v", err)
	}
	if d.Mount != "/" {
		t.Errorf("fell back to %q, want /", d.Mount)
	}
	if d.Total <= 0 {
		t.Error("the fallback returned no disk")
	}
}
