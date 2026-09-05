package daemon

import (
	"net/http"
	"strconv"

	"github.com/lgoyal6/amac/internal/search"
)

// GET /api/search?q=...&limit=50
//
// The index is brought up to date inside Query, so there is nothing to
// schedule and nothing to run at startup. For an append-only log that is a
// range scan over whatever arrived since the last search.
func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))

	hits, err := search.Query(r.Context(), s.log, q, limit)
	if err != nil {
		// A query the engine cannot parse is the caller's, not the daemon's.
		// Returning 500 would put an unbalanced quote in the same box as a
		// broken database.
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"hits": hits, "count": len(hits)})
}
