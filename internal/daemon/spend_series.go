package daemon

// Chart data for the money tab. The drawing lives in the page and the numbers
// live in internal/spend; this file only parses a query and hands the result
// over, so the shape another workstream draws from is decided in one place.

import (
	"net/http"
	"strconv"
	"time"

	"github.com/lgoyal6/amac/internal/spend"
)

// transcriptsDir is a seam so a test can point the hourly scan at a fixture
// instead of this machine's real sessions.
var transcriptsDir = spend.TranscriptsDir

func (s *Server) spendSeries(w http.ResponseWriter, r *http.Request) {
	days := 30
	if q := r.URL.Query().Get("days"); q != "" {
		n, err := strconv.Atoi(q)
		if err != nil || (n != 30 && n != 60 && n != 90) {
			writeJSON(w, 400, map[string]string{"error": "days must be 30, 60 or 90"})
			return
		}
		days = n
	}
	// A missing snapshot still returns the full window, every point null, so
	// the chart keeps its axis and the reason travels in the warning.
	snap, err := spend.Read()
	out := snap.Series(time.Now(), days)
	if err != nil {
		out.Warning = "no snapshot yet: run `node ~/looseapi/bin/spend.mjs`"
	}
	writeJSON(w, 200, out)
}

func (s *Server) spendToday(w http.ResponseWriter, r *http.Request) {
	// nil rates on purpose: amac has no per-model price table, and the
	// dollars column stays null until one exists rather than guessed.
	writeJSON(w, 200, spend.Hourly(transcriptsDir(), time.Now(), nil))
}
