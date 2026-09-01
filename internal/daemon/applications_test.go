package daemon

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lgoyal6/amac/internal/apply"
)

func TestApplicationsAPIReadsCacheAndUpdatesLocallyWithoutNotion(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("NOTION_TOKEN", "")
	t.Setenv("NOTION_DATABASE_ID", "")
	srv := testServer(t)
	a := apply.Application{Company: "Acme", Role: "SWE Intern", Source: apply.SourceExtension, AppliedAt: time.Now()}
	if _, err := apply.NewTracker(srv.log, nil).Record(context.Background(), a); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	srv.listApplications(w, httptest.NewRequest("GET", "/api/applications", nil))
	if w.Code != 200 {
		t.Fatalf("list returned %d: %s", w.Code, w.Body)
	}
	var list applicationListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if list.Total != 1 || list.Applications[0].Company != "Acme" || list.Warning == "" {
		t.Fatalf("bad cache response: %+v", list)
	}

	r := httptest.NewRequest("PATCH", "/api/applications/"+a.Key(), jsonBody(`{"status":"Interview","followUpAt":"2026-09-10"}`))
	r.SetPathValue("key", a.Key())
	w = httptest.NewRecorder()
	srv.updateApplication(w, r)
	if w.Code != 200 {
		t.Fatalf("patch returned %d: %s", w.Code, w.Body)
	}
	var updated struct {
		Application apply.Application `json:"application"`
		Warning     string            `json:"warning"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Application.Status != "Interview" || updated.Application.FollowUpAt == nil || updated.Warning == "" {
		t.Fatalf("bad update response: %+v", updated)
	}
}

func TestApplicationsSyncExplainsMissingBackupWithoutFailingCache(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("NOTION_TOKEN", "")
	srv := testServer(t)
	w := httptest.NewRecorder()
	srv.syncApplications(w, httptest.NewRequest("POST", "/api/applications/sync", nil))
	if w.Code != 200 || !contains(w.Body.String(), "Notion backup is not connected") {
		t.Fatalf("got %d: %s", w.Code, w.Body)
	}
}

func jsonBody(s string) *strings.Reader { return strings.NewReader(s) }
func contains(s, sub string) bool       { return strings.Contains(s, sub) }

// The loop is started with the daemon's shutdown context, so a stopping daemon
// must not wait out the first timer before the goroutine exits.
func TestNotionSyncLoopStopsWithItsContext(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("NOTION_TOKEN", "")
	srv := testServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() { srv.SyncNotionPeriodically(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the sync loop ignored its cancelled context")
	}
}

// A daemon with no Notion token still runs the loop. It must stay silent rather
// than writing a failure the dashboard would show as a broken sync.
func TestBackgroundSyncWithoutNotionRecordsNoFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("NOTION_TOKEN", "")
	t.Setenv("NOTION_DATABASE_ID", "")
	srv := testServer(t)
	srv.syncNotionQuietly(context.Background())

	repo, err := apply.NewRepository(srv.log.DB())
	if err != nil {
		t.Fatal(err)
	}
	meta, _ := repo.SyncMeta(context.Background())
	if !meta.SyncedAt.IsZero() || meta.Error != "" {
		t.Fatalf("a disconnected backup wrote sync state: %+v", meta)
	}
}
