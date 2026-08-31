package apply

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// The database identifier is not a credential. Keeping the known tracker
	// as the default removes one launchd setting while the bearer token remains
	// file- or environment-only.
	DefaultNotionDatabaseID        = "d0c2a411-b2a0-4376-84b8-f0515a05ba08"
	SourceNotion            Source = "notion"
)

// Notion mirrors the local application cache into the user's existing
// Internship Tracker and can hydrate that cache on startup/manual sync.
type Notion struct {
	token, database string
	client          *http.Client
	baseURL         string
	schema          map[string]string
}

func NewNotion() (*Notion, error) {
	tok := strings.TrimSpace(os.Getenv("NOTION_TOKEN"))
	db := strings.TrimSpace(os.Getenv("NOTION_DATABASE_ID"))
	home, _ := os.UserHomeDir()
	if tok == "" && home != "" {
		tok = readTokenFile(filepath.Join(home, ".amac", "notion_token"))
	}
	if db == "" && home != "" {
		db = readSecretFile(filepath.Join(home, ".amac", "notion_database_id"))
	}
	if db == "" {
		db = DefaultNotionDatabaseID
	}
	if tok == "" {
		return nil, fmt.Errorf("Notion backup is not connected")
	}
	return &Notion{token: tok, database: db, client: &http.Client{Timeout: 30 * time.Second}, baseURL: "https://api.notion.com"}, nil
}

func readSecretFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func readTokenFile(path string) string {
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm()&0o077 != 0 {
		return ""
	}
	return readSecretFile(path)
}

func (n *Notion) Name() string { return "notion" }

func (n *Notion) Upsert(ctx context.Context, _ string, a Application) error {
	if err := n.loadSchema(ctx); err != nil {
		return err
	}
	id, err := n.findExisting(ctx, a)
	if err != nil {
		return err
	}
	props := n.properties(a)
	if id != "" {
		return n.do(ctx, http.MethodPatch, n.baseURL+"/v1/pages/"+id, map[string]any{"properties": props}, nil)
	}
	return n.do(ctx, http.MethodPost, n.baseURL+"/v1/pages", map[string]any{"parent": map[string]string{"database_id": n.database}, "properties": props}, nil)
}

func (n *Notion) loadSchema(ctx context.Context) error {
	if n.schema != nil {
		return nil
	}
	var out struct {
		Properties map[string]struct {
			Type string `json:"type"`
		} `json:"properties"`
	}
	if err := n.do(ctx, http.MethodGet, n.baseURL+"/v1/databases/"+n.database, nil, &out); err != nil {
		return fmt.Errorf("read Notion tracker schema: %w", err)
	}
	n.schema = map[string]string{}
	for k, v := range out.Properties {
		n.schema[k] = v.Type
	}
	if n.schema["Company"] != "title" || n.schema["Role"] != "rich_text" {
		return fmt.Errorf("Notion tracker needs Company (title) and Role (text)")
	}
	return nil
}

func (n *Notion) findExisting(ctx context.Context, a Application) (string, error) {
	if a.NotionID != "" {
		return a.NotionID, nil
	}
	body := map[string]any{"filter": map[string]any{"and": []any{
		map[string]any{"property": "Company", "title": map[string]string{"equals": a.Company}},
		map[string]any{"property": "Role", "rich_text": map[string]string{"equals": a.Role}},
	}}, "page_size": 1}
	var out struct {
		Results []struct {
			ID string `json:"id"`
		} `json:"results"`
	}
	if err := n.do(ctx, http.MethodPost, n.baseURL+"/v1/databases/"+n.database+"/query", body, &out); err != nil {
		return "", err
	}
	if len(out.Results) == 0 {
		return "", nil
	}
	return out.Results[0].ID, nil
}

func (n *Notion) properties(a Application) map[string]any {
	p := map[string]any{"Company": title(a.Company), "Role": richText(a.Role)}
	if n.has("Status") && a.Status != "" {
		p["Status"] = selectProp(a.Status, n.schema["Status"])
	}
	if n.has("Date Applied") && !a.AppliedAt.IsZero() {
		p["Date Applied"] = map[string]any{"date": map[string]string{"start": a.AppliedAt.Format("2006-01-02")}}
	}
	if n.has("Link") && a.URL != "" {
		p["Link"] = map[string]any{"url": a.URL}
	}
	for name, value := range map[string]string{"Category": a.Category, "Cycle": a.Cycle, "Location": a.Location, "Work Auth": a.WorkAuth, "Tier": a.Tier} {
		if value == "" || !n.has(name) {
			continue
		}
		typ := n.schema[name]
		if typ == "select" || typ == "status" {
			p[name] = selectProp(value, typ)
		} else {
			p[name] = richText(value)
		}
	}
	if n.has("Sponsorship") && a.Sponsorship != nil {
		p["Sponsorship"] = map[string]any{"checkbox": *a.Sponsorship}
	}
	if name := strings.TrimSpace(os.Getenv("NOTION_FOLLOW_UP_PROPERTY")); name != "" && n.has(name) && a.FollowUpAt != nil {
		p[name] = map[string]any{"date": map[string]string{"start": a.FollowUpAt.Format("2006-01-02")}}
	}
	return p
}

func (n *Notion) has(name string) bool { _, ok := n.schema[name]; return ok }
func title(s string) map[string]any {
	return map[string]any{"title": []any{map[string]any{"text": map[string]string{"content": s}}}}
}
func richText(s string) map[string]any {
	return map[string]any{"rich_text": []any{map[string]any{"text": map[string]string{"content": s}}}}
}
func selectProp(s, typ string) map[string]any {
	if typ == "status" {
		return map[string]any{"status": map[string]string{"name": s}}
	}
	return map[string]any{"select": map[string]string{"name": s}}
}

type notionPage struct {
	ID          string                     `json:"id"`
	URL         string                     `json:"url"`
	CreatedTime string                     `json:"created_time"`
	LastEdited  string                     `json:"last_edited_time"`
	Properties  map[string]json.RawMessage `json:"properties"`
}

func (n *Notion) ListApplications(ctx context.Context) ([]Application, error) {
	if err := n.loadSchema(ctx); err != nil {
		return nil, err
	}
	var out []Application
	cursor := ""
	for {
		body := map[string]any{"page_size": 100}
		if cursor != "" {
			body["start_cursor"] = cursor
		}
		var page struct {
			Results    []notionPage `json:"results"`
			HasMore    bool         `json:"has_more"`
			NextCursor *string      `json:"next_cursor"`
		}
		if err := n.do(ctx, http.MethodPost, n.baseURL+"/v1/databases/"+n.database+"/query", body, &page); err != nil {
			return nil, err
		}
		for _, p := range page.Results {
			a := applicationFromNotion(p)
			if ValidStatus(a.Status) && a.Status != "Collected" && a.Status != "New" {
				out = append(out, a)
			}
		}
		if !page.HasMore || page.NextCursor == nil {
			break
		}
		cursor = *page.NextCursor
	}
	return out, nil
}

func applicationFromNotion(p notionPage) Application {
	a := Application{
		NotionID: p.ID, NotionURL: p.URL, Source: SourceNotion,
		Company: propText(p.Properties["Company"]), Role: propText(p.Properties["Role"]),
		URL: propURL(p.Properties["Link"]), Status: propSelect(p.Properties["Status"]),
		Category: propSelect(p.Properties["Category"]), Cycle: propSelect(p.Properties["Cycle"]),
		Location: propText(p.Properties["Location"]), WorkAuth: propSelect(p.Properties["Work Auth"]),
		Tier: propSelect(p.Properties["Tier"]), Sponsorship: propBool(p.Properties["Sponsorship"]),
	}
	a.AppliedAt = propDate(p.Properties["Date Applied"])
	if a.AppliedAt.IsZero() {
		a.AppliedAt, _ = time.Parse(time.RFC3339, p.CreatedTime)
	}
	a.UpdatedAt, _ = time.Parse(time.RFC3339, p.LastEdited)
	a.ID = a.Key()
	return a
}

func propText(raw json.RawMessage) string {
	var p struct {
		Title []struct {
			Plain string `json:"plain_text"`
		} `json:"title"`
		Rich []struct {
			Plain string `json:"plain_text"`
		} `json:"rich_text"`
	}
	_ = json.Unmarshal(raw, &p)
	var b strings.Builder
	for _, x := range p.Title {
		b.WriteString(x.Plain)
	}
	for _, x := range p.Rich {
		b.WriteString(x.Plain)
	}
	return b.String()
}
func propSelect(raw json.RawMessage) string {
	var p struct {
		Select *struct {
			Name string `json:"name"`
		} `json:"select"`
		Status *struct {
			Name string `json:"name"`
		} `json:"status"`
	}
	_ = json.Unmarshal(raw, &p)
	if p.Select != nil {
		return p.Select.Name
	}
	if p.Status != nil {
		return p.Status.Name
	}
	return ""
}
func propURL(raw json.RawMessage) string {
	var p struct {
		URL string `json:"url"`
	}
	_ = json.Unmarshal(raw, &p)
	return p.URL
}
func propDate(raw json.RawMessage) time.Time {
	var p struct {
		Date *struct {
			Start string `json:"start"`
		} `json:"date"`
	}
	_ = json.Unmarshal(raw, &p)
	if p.Date == nil {
		return time.Time{}
	}
	v, _ := time.Parse("2006-01-02", p.Date.Start)
	if v.IsZero() {
		v, _ = time.Parse(time.RFC3339, p.Date.Start)
	}
	return v
}
func propBool(raw json.RawMessage) *bool {
	if len(raw) == 0 {
		return nil
	}
	var p struct {
		Checkbox bool `json:"checkbox"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return nil
	}
	return &p.Checkbox
}

func SyncFromNotion(ctx context.Context, r *Repository, n *Notion) (SyncMeta, error) {
	apps, err := n.ListApplications(ctx)
	if err != nil {
		m := SyncMeta{SyncedAt: time.Now(), Error: err.Error()}
		_ = r.SetSyncMeta(ctx, m)
		return m, err
	}
	for _, a := range apps {
		if _, err := r.UpsertFromNotion(ctx, a); err != nil {
			return SyncMeta{}, err
		}
	}
	pending, err := r.Pending(ctx)
	if err != nil {
		return SyncMeta{}, err
	}
	var firstErr error
	for _, a := range pending {
		if err := n.Upsert(ctx, a.ID, a); err != nil {
			_ = r.MarkSyncError(ctx, a.ID, err.Error())
			if firstErr == nil {
				firstErr = err
			}
		} else {
			_ = r.MarkSynced(ctx, a.ID, time.Now())
		}
	}
	m := SyncMeta{SyncedAt: time.Now().UTC(), ItemCount: len(apps)}
	if firstErr != nil {
		m.Error = firstErr.Error()
	}
	_ = r.SetSyncMeta(ctx, m)
	return m, firstErr
}

func (n *Notion) do(ctx context.Context, method, url string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+n.token)
	req.Header.Set("Notion-Version", "2022-06-28")
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("Notion API %s: %.300s", resp.Status, raw)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}
