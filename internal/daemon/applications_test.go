package daemon

import (
	"context"
	"encoding/json"
	"fmt"
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

// The jobs tab read "200 of 200 applications" against 257 rows, because Total
// was the length of the limited page and so could never exceed the limit. The
// number would have read 200 forever: each new application pushed an old one
// out of the page and the total never moved.
func TestApplicationTotalCountsBeyondThePage(t *testing.T) {
	s := testServer(t)
	repo, err := apply.NewRepository(s.log.DB())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 7; i++ {
		if _, err := repo.UpsertLocal(t.Context(), apply.Application{
			ID: fmt.Sprintf("k%d", i), Company: fmt.Sprintf("Co %d", i), Role: "SWE", Status: "Applied",
		}); err != nil {
			t.Fatal(err)
		}
	}

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, authed("GET", "/api/applications?limit=3", ""))
	if w.Code != 200 {
		t.Fatalf("got %d: %s", w.Code, w.Body)
	}
	var body struct {
		Applications []map[string]any `json:"applications"`
		Total        int              `json:"total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("not JSON: %s", w.Body)
	}
	if len(body.Applications) != 3 {
		t.Errorf("page = %d rows, want the 3 asked for", len(body.Applications))
	}
	if body.Total != 7 {
		t.Errorf("total = %d, want 7: the count must not be the page size", body.Total)
	}
}

// A filtered count has to mean the same thing as the rows beside it, which is
// why List and Count share one WHERE.
func TestApplicationTotalRespectsTheFilter(t *testing.T) {
	s := testServer(t)
	repo, _ := apply.NewRepository(s.log.DB())
	for i, status := range []string{"Applied", "Applied", "Rejected"} {
		repo.UpsertLocal(t.Context(), apply.Application{
			ID: fmt.Sprintf("f%d", i), Company: "Co", Role: "SWE", Status: status,
		})
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, authed("GET", "/api/applications?status=Applied", ""))
	var body struct {
		Total int `json:"total"`
	}
	json.Unmarshal(w.Body.Bytes(), &body)
	if body.Total != 2 {
		t.Errorf("filtered total = %d, want 2", body.Total)
	}
}
