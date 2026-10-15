package supervisor

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestExactWritePreservesHardLinkVictimAndCreatesParents(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(victim, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(victim, target); err != nil {
		t.Fatal(err)
	}
	if err := writeExactPath(target, []byte("replacement")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(victim)
	if err != nil || string(got) != "original" {
		t.Fatalf("hard-link victim = %q, %v", got, err)
	}
	nested := filepath.Join(dir, "new", "parents", "output")
	if err := writeExactPath(nested, []byte("nested")); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(nested)
	if err != nil || string(got) != "nested" {
		t.Fatalf("nested output = %q, %v", got, err)
	}
}

func TestExactWriteRejectsParentAndLeafReplacementRaces(t *testing.T) {
	for _, parent := range []bool{false, true} {
		t.Run(map[bool]string{false: "leaf", true: "parent"}[parent], func(t *testing.T) {
			dir := t.TempDir()
			outside := filepath.Join(dir, "outside")
			inside := filepath.Join(dir, "inside")
			if err := os.Mkdir(outside, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(inside, 0o700); err != nil {
				t.Fatal(err)
			}
			victim := filepath.Join(outside, "output")
			if err := os.WriteFile(victim, []byte("original"), 0o600); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(inside, "output")
			stop := make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					if parent {
						parked := filepath.Join(dir, "parked")
						if os.Rename(inside, parked) == nil {
							_ = os.Symlink(outside, inside)
							_ = os.Remove(inside)
							_ = os.Rename(parked, inside)
						}
					} else {
						_ = os.Remove(target)
						_ = os.Symlink(victim, target)
						_ = os.Remove(target)
					}
				}
			}()
			for range 100 {
				_ = writeExactPath(target, []byte("authorized"))
			}
			close(stop)
			wg.Wait()
			got, err := os.ReadFile(victim)
			if err != nil || string(got) != "original" {
				t.Fatalf("race escaped exact path: %q, %v", got, err)
			}
		})
	}
}
