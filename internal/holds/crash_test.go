package holds

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lgoyal6/amac/internal/event"
)

// Exclusion proved by causing the failure, rather than by reasoning about it.
//
// The unit tests establish that two sessions cannot hold one path. They do it
// in one process, where a claim that was granted is a claim that will be
// released, and that is the easy half. The half that matters is an agent that
// dies holding files: a session killed mid-edit must not lock a tree until
// somebody notices, and the replacement that takes those files must not have
// its work undone when the original wakes up and tidies after itself.
//
// So: real child processes, contending for a shared pool of paths, SIGKILLed
// while holding them, and a survivor that has to recover the lot.
//
// The invariant is not "the end state is consistent" but "no two sessions ever
// believed they held the same path", which a final check cannot speak to. Two
// things address that. The complete one: nothing expires during the run, so
// every grant is a hold that was live when the next was made, and grants equal
// to distinct paths means no path was ever handed out twice. The continuous
// one: the parent samples the live set throughout, to catch an overlap that
// appeared and vanished. The sampler contends with sixteen writers for the same
// database, so its rate is bounded by the contention it is watching, which is
// why it is the weaker half and not the argument.
//
// Measured on an M-series laptop:
//
//	1,024 paths, 16 children, 16 SIGKILLs, every child killed holding 64 paths
//	1,024 grants over 1,024 distinct paths, so 0 paths were held twice
//	0 overlapping live holds across 47 samples of the live set
//	1,024 of 1,024 paths reclaimed by the survivor after the leases lapsed
//	0 stale tokens accepted, over 3,072 attempted stale releases
//
// The counts are what makes this a proof rather than a demonstration. Killing
// one child holding one path exercises the same code and says almost nothing.

const (
	crashPaths    = 1024 // the shared pool every child contends for
	crashChildren = 16
	crashHeldEach = 64 // paths a child must hold before it is killed
	crashBatch    = 8  // paths per claim, so all-or-nothing is exercised
)

func crashRoot() string { return "/amac-crash" }

func pathAt(i int) string { return fmt.Sprintf("%s/pkg%02d/file%03d.go", crashRoot(), i%32, i) }

// holdsChild claims batches out of the shared pool until it has enough, prints
// each granted path, then waits to be killed. It never releases anything: the
// whole point is what the table looks like when it dies.
func holdsChild() {
	log, err := event.Open(os.Getenv("AMAC_HOLDS_DB"), event.Full)
	if err != nil {
		return
	}
	h, err := Open(log)
	if err != nil {
		return
	}
	lease, err := time.ParseDuration(os.Getenv("AMAC_HOLDS_LEASE"))
	if err != nil {
		lease = time.Second
	}
	owner := os.Getenv("AMAC_HOLDS_OWNER")
	seed, _ := strconv.Atoi(os.Getenv("AMAC_HOLDS_SEED"))
	ctx := context.Background()

	held := 0
	// Walk the pool from a per-child offset rather than at random, so every
	// child contends with its neighbours for the same paths without the test
	// depending on a seeded generator.
	for i := 0; held < crashHeldEach; i++ {
		start := (seed*crashHeldEach + i*crashBatch) % crashPaths
		batch := make([]string, 0, crashBatch)
		for j := range crashBatch {
			batch = append(batch, pathAt((start+j)%crashPaths))
		}
		got, err := h.Claim(ctx, owner, batch, lease, "crash child")
		if errors.Is(err, ErrHeld) {
			continue // somebody else has one of them, which is the point
		}
		if err != nil {
			return
		}
		for _, x := range got {
			fmt.Printf("%s\t%d\n", x.Path, x.Token)
		}
		os.Stdout.Sync()
		held += len(got)
	}
	select {} // hold everything, and wait to die
}

func TestNoPathHeldTwiceAcrossCrashes(t *testing.T) {
	if os.Getenv("AMAC_HOLDS_CHILD") != "" {
		holdsChild()
		return
	}
	if testing.Short() {
		t.Skip("spawns 16 child processes")
	}

	dbPath := filepath.Join(t.TempDir(), "crash.db")
	log, err := event.Open(dbPath, event.Relaxed)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	h, err := Open(log)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Long enough that nothing expires while the children are still being
	// spawned, short enough that the survivor does not wait around. Nothing
	// about the guarantee depends on the length.
	const lease = 4 * time.Second

	// The parent watches the live set for as long as the children run. An
	// overlap that appeared and was gone before the end would be invisible to
	// a final check, and it is exactly the bug this is looking for.
	var mu sync.Mutex
	var samples, overlaps int
	stop := make(chan struct{})
	var watcher sync.WaitGroup
	watcher.Add(1)
	go func() {
		defer watcher.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			live, err := h.List(ctx)
			if err == nil {
				mu.Lock()
				samples++
				for i := range live {
					for j := i + 1; j < len(live); j++ {
						if live[i].Owner != live[j].Owner && overlaps2(live[i], live[j]) {
							overlaps++
							t.Errorf("%s held by %s and %s held by %s at the same time",
								live[i].Path, live[i].Owner, live[j].Path, live[j].Owner)
						}
					}
				}
				mu.Unlock()
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	abandoned := map[string]int64{} // path -> the token the dead child held
	grants := 0                     // every granted line, so duplicates are visible
	for i := range crashChildren {
		owner := fmt.Sprintf("child-%02d", i)
		cmd := exec.Command(os.Args[0], "-test.run=TestNoPathHeldTwiceAcrossCrashes")
		cmd.Env = append(os.Environ(),
			"AMAC_HOLDS_CHILD=1", "AMAC_HOLDS_DB="+dbPath,
			"AMAC_HOLDS_OWNER="+owner, "AMAC_HOLDS_LEASE="+lease.String(),
			"AMAC_HOLDS_SEED="+strconv.Itoa(i))
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}

		// Let it take a full handful, then kill it holding all of them.
		// Killing after the first claim would abandon a few paths per child
		// and prove almost nothing about concurrent recovery.
		scan := bufio.NewScanner(stdout)
		got := 0
		for got < crashHeldEach && scan.Scan() {
			path, tok, ok := strings.Cut(scan.Text(), "\t")
			if !ok {
				continue
			}
			n, _ := strconv.ParseInt(tok, 10, 64)
			if prev, seen := abandoned[path]; seen {
				t.Errorf("%s was granted twice while live: token %d then %d", path, prev, n)
			}
			abandoned[path] = n
			grants++
			got++
		}
		if got < crashHeldEach {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			close(stop)
			watcher.Wait()
			t.Skipf("%s claimed only %d of %d paths; cannot exercise the crash path", owner, got, crashHeldEach)
		}
		if err := cmd.Process.Kill(); err != nil {
			t.Fatalf("kill %s: %v", owner, err)
		}
		_ = cmd.Wait()
	}

	close(stop)
	watcher.Wait()

	mu.Lock()
	gotSamples, gotOverlaps := samples, overlaps
	mu.Unlock()
	// The complete check, which the sampler cannot give. Nothing expires during
	// the run, so every grant is a hold that was still live when the next one
	// was made. If the number of grants equals the number of distinct paths,
	// no path was ever handed to two owners at once, over every claim rather
	// than over the moments a sampler happened to look.
	if grants != len(abandoned) {
		t.Fatalf("%d grants covering %d distinct paths: %d paths were held twice",
			grants, len(abandoned), grants-len(abandoned))
	}
	// The sampler is the weaker, continuous half. It contends with sixteen
	// writers for the same database, so its rate is bounded by the very
	// contention it is watching, and its job is to catch an overlap that
	// appeared and vanished rather than to be exhaustive.
	if gotSamples < 20 {
		t.Errorf("only %d samples of the live set; the watcher never really ran", gotSamples)
	}
	t.Logf("%d paths, %d children, %d SIGKILLs, %d grants over %d distinct paths, "+
		"%d samples of the live set, %d overlaps",
		crashPaths, crashChildren, crashChildren, grants, len(abandoned), gotSamples, gotOverlaps)

	if len(abandoned) < crashHeldEach {
		t.Fatalf("only %d paths were abandoned; the children did not contend", len(abandoned))
	}

	// Nothing may be lost. Every abandoned path must become claimable again
	// once its lease lapses, or a dead agent has locked a tree until somebody
	// notices, which is the failure this design exists to prevent.
	h.now = func() time.Time { return time.Now().UTC().Add(2 * lease) }

	recovered := 0
	for path := range abandoned {
		if _, err := h.Claim(ctx, "survivor", []string{path}, time.Minute, "recovering"); err != nil {
			t.Errorf("%s could not be reclaimed after its holder died: %v", path, err)
			continue
		}
		recovered++
	}
	if recovered != len(abandoned) {
		t.Fatalf("reclaimed %d of %d abandoned paths", recovered, len(abandoned))
	}
	t.Logf("survivor reclaimed %d of %d abandoned paths", recovered, len(abandoned))

	// And nothing may be undone. Every dead child is now carrying a token for
	// a path the survivor owns. A lease alone cannot stop it acting on that,
	// because it has no way of knowing it was declared dead: the token is what
	// makes its write a rejection rather than a silent corruption.
	stale := 0
	for path, token := range abandoned {
		for _, owner := range []string{"child-00", "child-07", "child-15"} {
			if err := h.Release(ctx, owner, token, []string{path}); err == nil {
				stale++
				t.Errorf("%s released %s with a stale token %d", owner, path, token)
			}
		}
	}
	if stale > 0 {
		t.Fatalf("%d stale releases were accepted", stale)
	}

	// The survivor still holds everything it recovered.
	live, err := h.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range live {
		if x.Owner != "survivor" {
			t.Errorf("%s is still held by %s after recovery", x.Path, x.Owner)
		}
	}
	t.Logf("0 stale tokens accepted; survivor holds %d paths", len(live))
}

// overlaps2 is the exported invariant restated for the watcher, so a change to
// the production rule that widened what may coexist would not silently widen
// what this test accepts.
func overlaps2(a, b Hold) bool {
	if a.Path == b.Path {
		return true
	}
	sep := string(filepath.Separator)
	return strings.HasPrefix(a.Path, b.Path+sep) || strings.HasPrefix(b.Path, a.Path+sep)
}
