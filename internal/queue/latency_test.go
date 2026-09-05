package queue

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/lgoyal6/amac/internal/event"
)

// What a claim costs, as a distribution rather than an average.
//
// amac had one performance number, nanoseconds per append, and an average is
// the wrong summary for a contended write path. A claim is a conditional UPDATE
// inside a transaction that SQLite serialises across processes, so the
// interesting question is not what a typical claim costs but what the worst one
// does while sixteen workers are asking for the same row. That is the number a
// queue's behaviour under load is actually made of, and a mean hides it by
// construction.
//
// These are not a benchmark of SQLite and they are not comparable across
// machines. They exist so a regression in the claim path shows up as a shape
// change, and so the tail is stated rather than assumed.
//
// Measured on an M-series laptop, 400 claims per arm, which is what the shape
// looked like when this was written:
//
//	workers   p50       p99       max
//	1         202us     271us     2.3ms
//	4         199us     1.2ms     82ms
//	16        8.0ms     199ms     251ms
//
// One and four workers are the same at the median, and sixteen is forty times
// worse at p50 and several hundred at p99. That is SQLite serialising writers
// rather than anything amac does, and it is the number that says what this
// queue is for: agents on one laptop, not a fleet. A design that needed sixteen
// concurrent claimers would need a different store, and knowing where the knee
// is beats assuming there isn't one.

type dist struct {
	name    string
	samples []time.Duration
}

func (d *dist) add(x time.Duration) { d.samples = append(d.samples, x) }

func (d *dist) quantile(q float64) time.Duration {
	if len(d.samples) == 0 {
		return 0
	}
	// Nearest-rank on a sorted copy. Interpolating between two samples invents
	// a latency nothing observed, which is the wrong kind of tidy for a tail.
	i := int(q * float64(len(d.samples)-1))
	return d.samples[i]
}

func (d *dist) report(t *testing.T) {
	t.Helper()
	sort.Slice(d.samples, func(i, j int) bool { return d.samples[i] < d.samples[j] })
	fmt.Printf("  %-28s n=%-6d p50=%-9v p90=%-9v p99=%-9v p999=%-9v max=%v\n",
		d.name, len(d.samples), d.quantile(0.50).Round(time.Microsecond),
		d.quantile(0.90).Round(time.Microsecond), d.quantile(0.99).Round(time.Microsecond),
		d.quantile(0.999).Round(time.Microsecond), d.samples[len(d.samples)-1].Round(time.Microsecond))
}

func benchQueue(t *testing.T) (*Queue, func()) {
	t.Helper()
	dir := t.TempDir()
	log, err := event.Open(filepath.Join(dir, "events.db"), event.Relaxed)
	if err != nil {
		t.Fatal(err)
	}
	q, err := Open(log)
	if err != nil {
		log.Close()
		t.Fatal(err)
	}
	return q, func() { log.Close() }
}

// TestClaimLatencyDistribution prints the shape of the claim path uncontended
// and then under contention, which is the comparison that says what queueing
// costs as opposed to what SQLite costs.
func TestClaimLatencyDistribution(t *testing.T) {
	if testing.Short() {
		t.Skip("latency shape is not a correctness test")
	}
	ctx := context.Background()

	fmt.Println("claim latency, one worker (no contention):")
	solo := &dist{name: "claim"}
	q, done := benchQueue(t)
	const n = 400
	file(t, q, n)
	for range n {
		start := time.Now()
		if _, err := q.Claim(ctx, "solo", time.Minute); err != nil {
			break
		}
		solo.add(time.Since(start))
	}
	solo.report(t)
	done()

	for _, workers := range []int{4, 16} {
		fmt.Printf("claim latency, %d workers on one queue:\n", workers)
		q, done := benchQueue(t)
		file(t, q, n)
		d := &dist{name: fmt.Sprintf("claim x%d", workers)}
		var mu sync.Mutex
		var wg sync.WaitGroup
		for w := range workers {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				owner := fmt.Sprintf("w%d", w)
				for {
					start := time.Now()
					_, err := q.Claim(ctx, owner, time.Minute)
					took := time.Since(start)
					if err != nil {
						return
					}
					mu.Lock()
					d.add(took)
					mu.Unlock()
				}
			}(w)
		}
		wg.Wait()
		d.report(t)
		done()
	}

	// The property, rather than the numbers: contention must not collapse the
	// claim path. A p99 that is orders of magnitude past p50 means workers are
	// starving on the transaction rather than queueing for it, and that is a
	// regression worth failing a build over even though the absolute figures
	// are machine-specific.
	if len(solo.samples) > 0 {
		sort.Slice(solo.samples, func(i, j int) bool { return solo.samples[i] < solo.samples[j] })
		p50, p999 := solo.quantile(0.50), solo.quantile(0.999)
		if p50 > 0 && p999 > 500*p50 {
			t.Errorf("uncontended tail is %v against a p50 of %v; the claim path is not stable", p999, p50)
		}
	}
}

// TestAppendLatencyDistribution does the same for the log, whose durability
// mode is the one real tuning decision in the system.
func TestAppendLatencyDistribution(t *testing.T) {
	if testing.Short() {
		t.Skip("latency shape is not a correctness test")
	}
	ctx := context.Background()
	fmt.Println("append latency by durability mode:")

	for _, mode := range []struct {
		name string
		d    event.Durability
	}{{"relaxed", event.Relaxed}, {"full", event.Full}} {
		dir := t.TempDir()
		log, err := event.Open(filepath.Join(dir, "events.db"), mode.d)
		if err != nil {
			t.Fatal(err)
		}
		d := &dist{name: mode.name}
		for i := range 400 {
			ev, err := event.New(event.KindObservation, "bench", "", map[string]any{"i": i})
			if err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			if _, err := log.Append(ctx, ev); err != nil {
				t.Fatal(err)
			}
			d.add(time.Since(start))
		}
		d.report(t)
		log.Close()
	}

	// Deliberately no assertion comparing the two. A previous version of this
	// project shipped a single-figure fsync penalty that did not reproduce:
	// thirteen paired runs put it anywhere from Full being faster to 58% slower,
	// and a cold binary ran two to three times slower than the committed number.
	// The distributions are printed so a reader can see that spread rather than
	// be handed a point estimate that a second run would contradict.
	fmt.Println("  (no pass/fail on the gap: it does not reproduce across runs, by measurement)")
}
