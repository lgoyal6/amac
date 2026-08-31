package apply

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/lgoyal6/amac/internal/event"
)

func openApplicationRepo(t *testing.T) (*event.Log, *Repository) {
	t.Helper()
	log, err := event.Open(filepath.Join(t.TempDir(), "events.db"), event.Relaxed)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	repo, err := NewRepository(log.DB())
	if err != nil {
		t.Fatal(err)
	}
	return log, repo
}

func TestRepositoryIsAFastDurableApplicationView(t *testing.T) {
	_, repo := openApplicationRepo(t)
	ctx := context.Background()
	a, err := repo.UpsertLocal(ctx, Application{Company: "Acme", Role: "Platform Intern", URL: "https://jobs.example/acme", Source: SourceExtension, AppliedAt: time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != "Applied" || a.SyncState != "pending" {
		t.Fatalf("unexpected initial state: %+v", a)
	}
	status, follow := "Interview", "2026-09-08"
	a, err = repo.Update(ctx, a.ID, Update{Status: &status, FollowUpAt: &follow})
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != "Interview" || a.FollowUpAt == nil || a.FollowUpAt.Format("2006-01-02") != follow {
		t.Fatalf("update was not persisted: %+v", a)
	}
	got, err := repo.List(ctx, ListOptions{Query: "platform"})
	if err != nil || len(got) != 1 {
		t.Fatalf("search got %d, %v", len(got), err)
	}
}

func TestNotionImportDoesNotOverwriteAPendingLocalEdit(t *testing.T) {
	_, repo := openApplicationRepo(t)
	ctx := context.Background()
	base := Application{Company: "Acme", Role: "SWE Intern", Status: "Applied", Source: SourceNotion, NotionID: "page-1", AppliedAt: time.Now()}
	if _, err := repo.UpsertFromNotion(ctx, base); err != nil {
		t.Fatal(err)
	}
	status := "Interview"
	if _, err := repo.Update(ctx, base.Key(), Update{Status: &status}); err != nil {
		t.Fatal(err)
	}
	base.Status = "Applied"
	got, err := repo.UpsertFromNotion(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "Interview" || got.SyncState != "pending" {
		t.Fatalf("stale Notion value won: %+v", got)
	}
}

type failingSink struct{}

func (failingSink) Name() string                                      { return "notion" }
func (failingSink) Upsert(context.Context, string, Application) error { return errors.New("offline") }

func TestTrackerKeepsLocalCaptureWhenBackupIsOffline(t *testing.T) {
	log, repo := openApplicationRepo(t)
	a := Application{Company: "Acme", Role: "SWE Intern", Source: SourceExtension, AppliedAt: time.Now()}
	isNew, err := NewTracker(log, failingSink{}).Record(context.Background(), a)
	if err != nil || !isNew {
		t.Fatalf("record = %v,%v", isNew, err)
	}
	got, err := repo.Get(context.Background(), a.Key())
	if err != nil {
		t.Fatal(err)
	}
	if got.SyncState != "error" || got.SyncError == "" {
		t.Fatalf("backup failure not visible: %+v", got)
	}
}

func TestDuplicateCaptureDoesNotMoveAnApplicationBackToApplied(t *testing.T) {
	log, repo := openApplicationRepo(t)
	ctx := context.Background()
	a := Application{Company: "Acme", Role: "SWE Intern", Source: SourceExtension, AppliedAt: time.Now()}
	tracker := NewTracker(log, nil)
	if _, err := tracker.Record(ctx, a); err != nil {
		t.Fatal(err)
	}
	status := "Interview"
	if _, err := repo.Update(ctx, a.Key(), Update{Status: &status}); err != nil {
		t.Fatal(err)
	}
	a.Source = SourceEmail
	a.AppliedAt = a.AppliedAt.Add(5 * time.Minute)
	if isNew, err := tracker.Record(ctx, a); err != nil || isNew {
		t.Fatalf("duplicate = %v,%v", isNew, err)
	}
	got, err := repo.Get(ctx, a.Key())
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "Interview" {
		t.Fatalf("duplicate reset status to %q", got.Status)
	}
}

func TestUpdateRejectsUnknownStatusAndBadDate(t *testing.T) {
	_, repo := openApplicationRepo(t)
	ctx := context.Background()
	a, _ := repo.UpsertLocal(ctx, Application{Company: "Acme", Role: "SWE", AppliedAt: time.Now()})
	bad := "Ghosted"
	if _, err := repo.Update(ctx, a.ID, Update{Status: &bad}); err == nil {
		t.Fatal("accepted unknown status")
	}
	badDate := "tomorrow"
	if _, err := repo.Update(ctx, a.ID, Update{FollowUpAt: &badDate}); err == nil {
		t.Fatal("accepted bad follow-up date")
	}
}
