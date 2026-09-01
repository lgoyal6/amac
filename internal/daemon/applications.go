package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/lgoyal6/amac/internal/apply"
)

type applicationListResponse struct {
	Applications []apply.Application `json:"applications"`
	Total        int                 `json:"total"`
	SyncedAt     *time.Time          `json:"syncedAt,omitempty"`
	Source       string              `json:"source"`
	Stale        bool                `json:"stale"`
	Warning      string              `json:"warning,omitempty"`
}

// listApplications is intentionally cache-only. Notion page loads are the
// problem this endpoint solves, so an ordinary dashboard refresh must never
// block on Notion. POST /api/applications/sync is the explicit network step.
func (s *Server) listApplications(w http.ResponseWriter, r *http.Request) {
	repo, err := apply.NewRepository(s.log.DB())
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	apps, err := repo.List(r.Context(), apply.ListOptions{Query: r.URL.Query().Get("q"), Status: r.URL.Query().Get("status"), Limit: limit})
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	meta, _ := repo.SyncMeta(r.Context())
	resp := applicationListResponse{Applications: apps, Total: len(apps), Source: "local", Stale: meta.SyncedAt.IsZero() || time.Since(meta.SyncedAt) > 24*time.Hour}
	if !meta.SyncedAt.IsZero() {
		v := meta.SyncedAt
		resp.SyncedAt = &v
		resp.Source = "notion cache"
	}
	if _, err := apply.NewNotion(); err != nil {
		resp.Warning = "Notion backup is not connected"
	} else if meta.Error != "" {
		resp.Warning = "The last Notion sync did not finish"
	}
	writeJSON(w, 200, resp)
}

func (s *Server) updateApplication(w http.ResponseWriter, r *http.Request) {
	var body apply.Update
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	repo, err := apply.NewRepository(s.log.DB())
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	a, err := repo.Update(r.Context(), r.PathValue("key"), body)
	if err == sql.ErrNoRows {
		writeJSON(w, 404, map[string]string{"error": "no such application"})
		return
	}
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	warning := ""
	if notion, nerr := apply.NewNotion(); nerr == nil {
		if err := notion.Upsert(r.Context(), a.ID, a); err != nil {
			_ = repo.MarkSyncError(r.Context(), a.ID, err.Error())
			warning = "Saved in AMAC; Notion backup will retry"
		} else {
			_ = repo.MarkSynced(r.Context(), a.ID, time.Now())
		}
	} else {
		warning = "Saved in AMAC; Notion backup is not connected"
	}
	a, _ = repo.Get(r.Context(), a.ID)
	writeJSON(w, 200, map[string]any{"application": a, "warning": warning})
}

func (s *Server) syncApplications(w http.ResponseWriter, r *http.Request) {
	repo, err := apply.NewRepository(s.log.DB())
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	notion, err := apply.NewNotion()
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "warning": "Notion backup is not connected"})
		return
	}
	meta, err := s.syncNotion(r.Context(), repo, notion)
	if err != nil {
		writeJSON(w, 502, map[string]any{"ok": false, "warning": "Notion sync did not finish", "syncedAt": meta.SyncedAt, "imported": meta.ItemCount})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "syncedAt": meta.SyncedAt, "imported": meta.ItemCount})
}

// syncNotion is the one place the page walk actually runs, so the button and
// the loop below cannot overlap.
func (s *Server) syncNotion(ctx context.Context, repo *apply.Repository, n *apply.Notion) (apply.SyncMeta, error) {
	s.syncing.Lock()
	defer s.syncing.Unlock()
	return apply.SyncFromNotion(ctx, repo, n)
}

// How often the daemon refreshes the application cache. Applications are filed
// by hand a few times a day, so this is about how stale the jobs tab is allowed
// to look rather than about keeping up with anything.
const notionSyncInterval = 15 * time.Minute

// SyncNotionPeriodically refreshes the cache until ctx ends.
//
// listApplications is deliberately cache-only and must stay that way, which
// left the cache moving only when somebody pressed the button: applications
// filed after the last press were simply absent from the dashboard, with
// nothing on screen to say so until the 24-hour staleness flag caught up.
// Refreshing out of band fixes that without putting Notion back in the path of
// a page load.
func (s *Server) SyncNotionPeriodically(ctx context.Context) {
	// Not immediately. The daemon has just finished waiting for the tailnet,
	// and a first call that beats the network up records a failure the
	// dashboard would then show for a quarter of an hour.
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			timer.Reset(notionSyncInterval)
			s.syncNotionQuietly(ctx)
		}
	}
}

// syncNotionQuietly runs one background sync. A failure is not worth an event
// or a crash: SyncFromNotion records it as sync metadata, which is what the
// dashboard already reads to say the last sync did not finish.
func (s *Server) syncNotionQuietly(ctx context.Context) {
	repo, err := apply.NewRepository(s.log.DB())
	if err != nil {
		return
	}
	notion, err := apply.NewNotion()
	if err != nil {
		// Notion is not connected. The dashboard says so on every load.
		return
	}
	if _, err := s.syncNotion(ctx, repo, notion); err != nil {
		fmt.Printf("notion sync failed: %v\n", err)
	}
}
