package apply

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNotionReadsTheRealTrackerShape(t *testing.T) {
	var writes int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/v1/databases/db":
			json.NewEncoder(w).Encode(map[string]any{"properties": map[string]any{"Company": map[string]string{"type": "title"}, "Role": map[string]string{"type": "rich_text"}, "Status": map[string]string{"type": "select"}, "Date Applied": map[string]string{"type": "date"}, "Link": map[string]string{"type": "url"}, "Sponsorship": map[string]string{"type": "checkbox"}}})
		case r.Method == "POST" && r.URL.Path == "/v1/databases/db/query":
			json.NewEncoder(w).Encode(map[string]any{"results": []any{map[string]any{"id": "page-1", "url": "https://notion/page-1", "created_time": "2026-08-30T00:00:00Z", "last_edited_time": "2026-08-30T01:00:00Z", "properties": map[string]any{
				"Company": map[string]any{"title": []any{map[string]string{"plain_text": "Acme"}}}, "Role": map[string]any{"rich_text": []any{map[string]string{"plain_text": "SWE Intern"}}}, "Status": map[string]any{"select": map[string]string{"name": "Applied"}}, "Date Applied": map[string]any{"date": map[string]string{"start": "2026-08-29"}}, "Link": map[string]string{"url": "https://jobs/acme"}, "Sponsorship": map[string]bool{"checkbox": false}}}}, "has_more": false})
		case r.Method == "PATCH" && r.URL.Path == "/v1/pages/page-1":
			writes++
			w.Write([]byte(`{}`))
		default:
			http.Error(w, "unexpected", 500)
		}
	}))
	defer ts.Close()
	n := &Notion{token: "secret", database: "db", client: ts.Client(), baseURL: ts.URL}
	apps, err := n.ListApplications(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 1 || apps[0].Company != "Acme" || apps[0].Status != "Applied" || apps[0].Sponsorship == nil || *apps[0].Sponsorship {
		t.Fatalf("bad import: %+v", apps)
	}
	if err := n.Upsert(context.Background(), apps[0].ID, apps[0]); err != nil {
		t.Fatal(err)
	}
	if writes != 1 {
		t.Fatalf("writes=%d", writes)
	}
}

func TestNotionTokenFileFallbackRequiresPrivatePermissions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("NOTION_TOKEN", "")
	t.Setenv("NOTION_DATABASE_ID", "")
	dir := filepath.Join(home, ".amac")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "notion_token")
	if err := os.WriteFile(path, []byte("secret-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	n, err := NewNotion()
	if err != nil {
		t.Fatal(err)
	}
	if n.token != "secret-token" || n.database != DefaultNotionDatabaseID {
		t.Fatal("token fallback or harmless database default was not used")
	}
	if strings.Contains(errString(err), "secret-token") {
		t.Fatal("token leaked in error")
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
