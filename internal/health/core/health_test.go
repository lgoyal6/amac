package core

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSweepAtCadenceBoundaryAndFutureDelivery(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		age  time.Duration
		want State
	}{
		{name: "inside grace", age: 90 * time.Minute, want: OK},
		{name: "exact boundary", age: 2 * time.Hour, want: OK},
		{name: "past boundary", age: 2*time.Hour + time.Nanosecond, want: Late},
		{name: "future timestamp", age: -time.Minute, want: OK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := SweepAt(context.Background(), []Automation{{
				Name: "sample", Every: time.Hour, Grace: time.Hour,
				Check: func(context.Context) (Report, error) {
					return Report{State: OK, Last: now.Add(-tc.age)}, nil
				},
			}}, now)
			if got[0].State != tc.want {
				t.Fatalf("state = %s, want %s", got[0].State, tc.want)
			}
		})
	}
}

func TestSweepReadsClockAfterProbe(t *testing.T) {
	before := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	current := before
	got := sweep(context.Background(), []Automation{{
		Name: "slow probe", Every: time.Hour,
		Check: func(context.Context) (Report, error) {
			current = before.Add(2 * time.Hour)
			return Report{State: OK, Last: before}, nil
		},
	}}, func() time.Time { return current })
	if got[0].State != Late {
		t.Fatalf("slow probe state = %s, want late", got[0].State)
	}
}

func TestSweepAtPreservesErrorAndStableOrdering(t *testing.T) {
	now := time.Now()
	items := []Automation{
		{Name: "first unknown", Check: func(context.Context) (Report, error) {
			return Report{Detail: "probe stopped"}, errors.New("read failed")
		}},
		{Name: "late", Every: time.Minute, Check: func(context.Context) (Report, error) {
			return Report{State: OK, Last: now.Add(-time.Hour)}, nil
		}},
		{Name: "failed", Every: time.Minute, Check: func(context.Context) (Report, error) {
			return Report{State: Failing, Last: now.Add(-time.Hour), Detail: "run failed"}, nil
		}},
		{Name: "second unknown", Check: func(context.Context) (Report, error) {
			return Report{State: Unknown, Detail: "no delivery time"}, nil
		}},
	}
	got := SweepAt(context.Background(), items, now)
	want := []string{"failed", "late", "first unknown", "second unknown"}
	for i := range want {
		if got[i].Name != want[i] {
			t.Fatalf("report %d = %q, want %q", i, got[i].Name, want[i])
		}
	}
	if got[2].Err != "read failed" || got[2].State != Unknown {
		t.Fatalf("probe error report = %#v", got[2])
	}
}
