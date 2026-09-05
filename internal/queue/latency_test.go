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

// report prints the shape. Deliberately no p99.9: four hundred samples cannot
// resolve it. Nearest-rank puts q=0.999 at index 398 of 0..399, which is the
// second largest sample rather than a percentile, and printing it under a
// percentile's name is what invited an assertion on a single observation. The
// largest sample is still here, honestly labelled as the largest sample.
func (d *dist) report(t *testing.T) {
	t.Helper()
	sort.Slice(d.samples, func(i, j int) bool { return d.samples[i] < d.samples[j] })
	fmt.Printf("  %-28s n=%-6d p50=%-9v p90=%-9v p99=%-9v max=%v\n",
		d.name, len(d.samples), d.quantile(0.50).Round(time.Microsecond),
		d.quantile(0.90).Round(time.Microsecond), d.quantile(0.99).Round(time.Microsecond),
		d.samples[len(d.samples)-1].Round(time.Microsecond))
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
	// claim path. A tail orders of magnitude past p50 means workers are
	// starving on the transaction rather than queueing for it, and that is a
	// regression worth failing a build over even though the absolute figures
	// are machine-specific.
	//
	// Read at p99, not at q=0.999, which is the whole reason this used to fail
	// on a shared runner. Nearest-rank over 400 samples puts q=0.999 at index
	// 398: the second largest observation, not a percentile. Two descheduled
	// goroutines were therefore enough to fail a build, and two strays in four
	// hundred samples is an ordinary morning on a shared runner. GitHub's
	// ubuntu runner produced exactly that, 211ms at index 398 with a 248ms max
	// above it, against a p50 of 250us; the same commit passed on a re-run.
	//
	// p99 leaves four samples above it, so a handful of strays cannot move it,
	// while a genuine collapse must make at least one claim in a hundred slow
	// and still fails. That is the property this was always trying to state.
	//
	// The threshold is unchanged at 500x on purpose. It is a collapse detector
	// and not a tuning knob: measured over twenty local runs the uncontended
	// ratio sits between 1.4x and 3.9x, and the CI run that failed on the old
	// statistic had a p99 ratio of 5.7x. Anything approaching 500x is a broken
	// claim path, not a busy machine.
	if p50, p99, ok := tailIsProportionate(solo.samples, maxTailRatio); !ok {
		t.Errorf("uncontended p99 is %v against a p50 of %v; the claim path is not stable", p99, p50)
	}
}

// maxTailRatio is how far past the median the 99th percentile may sit before
// the claim path is called broken.
const maxTailRatio = 500

// tailIsProportionate is the assertion above, as a function, so that what it
// does with a stray sample and what it does with a fat tail are both things a
// test can state rather than things a reader has to believe. An empty
// distribution is proportionate: there is nothing to be disproportionate about,
// and a failing claim loop already fails elsewhere.
func tailIsProportionate(samples []time.Duration, ratio float64) (p50, p99 time.Duration, ok bool) {
	if len(samples) == 0 {
		return 0, 0, true
	}
	d := &dist{samples: append([]time.Duration(nil), samples...)}
	sort.Slice(d.samples, func(i, j int) bool { return d.samples[i] < d.samples[j] })
	p50, p99 = d.quantile(0.50), d.quantile(0.99)
	if p50 <= 0 {
		return p50, p99, true
	}
	return p50, p99, float64(p99) <= ratio*float64(p50)
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

// The two cases the statistic has to tell apart, which is the only reason to
// have changed it.
//
// A shared CI runner deschedules a goroutine now and then and produces a couple
// of enormous samples. That is the machine, not the queue, and it used to fail
// the build because the check read q=0.999, which over 400 samples is index
// 398: the second largest observation rather than a percentile. Two strays
// were enough, which is what the failing run showed, 211ms at that index with
// a 248ms max above it.
//
// A claim path that has actually collapsed makes many claims slow, not one. p99
// leaves four samples above it out of 400, so strays cannot reach it and a
// sustained tail cannot avoid it.
func TestTheStabilityCheckIgnoresStraysAndCatchesAFatTail(t *testing.T) {
	const n = 400
	base := func() []time.Duration {
		out := make([]time.Duration, 0, n)
		for i := range n {
			// A tight middle, as the uncontended path really looks: p50 near
			// 200us with a little spread.
			out = append(out, time.Duration(180+i%40)*time.Microsecond)
		}
		return out
	}

	// The failing run, reconstructed: two samples past 200ms out of 400.
	strays := func() []time.Duration {
		s := base()
		s[0], s[1] = 211*time.Millisecond, 248*time.Millisecond
		return s
	}

	t.Run("the runner's strays are the runner, not the queue", func(t *testing.T) {
		if _, p99, ok := tailIsProportionate(strays(), maxTailRatio); !ok {
			t.Errorf("two stray samples failed the check (p99 %v); this is the flake", p99)
		}
	})

	t.Run("four strays still cannot reach p99", func(t *testing.T) {
		s := base()
		for i := range 4 {
			s[i] = 211 * time.Millisecond
		}
		if _, p99, ok := tailIsProportionate(s, maxTailRatio); !ok {
			t.Errorf("four strays out of 400 failed the check (p99 %v)", p99)
		}
	})

	t.Run("one claim in twenty being slow is a collapse and must fail", func(t *testing.T) {
		s := base()
		for i := range n / 20 {
			s[i] = 211 * time.Millisecond
		}
		if p50, p99, ok := tailIsProportionate(s, maxTailRatio); ok {
			t.Errorf("a fat tail passed: p99 %v against p50 %v", p99, p50)
		}
	})

	// The old statistic on the same samples, kept because it is the reason for
	// the change. Read at q=0.999 the second stray IS the assertion, so the
	// flake was not bad luck: it was what the check was measuring. If this ever
	// stops holding, the reasoning above is wrong and should be rewritten
	// rather than quietly kept.
	t.Run("the old statistic fails on the same samples, which is why it moved", func(t *testing.T) {
		sorted := strays()
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		d := &dist{samples: sorted}
		p50, p999 := d.quantile(0.50), d.quantile(0.999)
		if float64(p999) <= maxTailRatio*float64(p50) {
			t.Fatalf("q=0.999 gave %v against p50 %v and would have passed; "+
				"the stated reason for moving to p99 does not hold", p999, p50)
		}
	})

	// And an empty distribution is not a failure.
	if _, _, ok := tailIsProportionate(nil, maxTailRatio); !ok {
		t.Error("an empty distribution was called unstable")
	}
}
