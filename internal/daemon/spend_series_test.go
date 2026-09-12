package daemon

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/lgoyal6/amac/internal/spend"
)

// The window is one of three fixed sizes and always comes back full, whether
// or not a snapshot exists on this machine: a chart needs its axis either way.
func TestSpendSeriesWindowIsFixedAndFull(t *testing.T) {
	for _, tc := range []struct {
		query string
		code  int
		days  int
	}{
		{"", 200, 30}, {"?days=30", 200, 30}, {"?days=60", 200, 60}, {"?days=90", 200, 90},
		{"?days=45", 400, 0}, {"?days=x", 400, 0},
	} {
		w := do(t, authed("GET", "/api/spend/series"+tc.query, ""))
		if w.Code != tc.code {
			t.Fatalf("%q: %d %s", tc.query, w.Code, w.Body)
		}
		if tc.code != 200 {
			continue
		}
		var got spend.Series
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("%q: not JSON: %s", tc.query, w.Body)
		}
		if got.Days != tc.days || len(got.Points) != tc.days {
			t.Errorf("%q: days=%d points=%d", tc.query, got.Days, len(got.Points))
		}
		if last := got.Points[len(got.Points)-1].Date; last != time.Now().Format("2006-01-02") {
			t.Errorf("%q: last point is %s, want today", tc.query, last)
		}
	}
}
