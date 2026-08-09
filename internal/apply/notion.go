package apply

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// Notion writes applications into a database.
//
// Credentials come from the environment, never a config file: this token can
// read and write a workspace.
//
//	NOTION_TOKEN        internal integration secret
//	NOTION_DATABASE_ID  the target database
//
// The database needs these properties, and the writer fails loudly rather than
// guessing if they are missing:
//
//	Company  (title)   Role (rich_text)   Status (select)
//	Applied  (date)    URL  (url)         ATS    (rich_text)
type Notion struct {
	token, database string
	client          *http.Client
}

func NewNotion() (*Notion, error) {
	tok, db := os.Getenv("NOTION_TOKEN"), os.Getenv("NOTION_DATABASE_ID")
	if tok == "" || db == "" {
		return nil, fmt.Errorf("NOTION_TOKEN and NOTION_DATABASE_ID must be set")
	}
	return &Notion{token: tok, database: db, client: &http.Client{Timeout: 30 * time.Second}}, nil
}

func (n *Notion) Name() string { return "notion" }

func (n *Notion) Upsert(ctx context.Context, key string, a Application) error {
	// Query first. The tracker already deduplicates against the event log, but
	// Notion is shared mutable state that a phone or another machine may have
	// written to, so the local log is not authoritative about what is there.
	existing, err := n.findByKey(ctx, key)
	if err != nil {
		return err
	}
	if existing != "" {
		return nil
	}
	return n.create(ctx, key, a)
}

func (n *Notion) findByKey(ctx context.Context, key string) (string, error) {
	body := map[string]any{
		"filter": map[string]any{
			"property": "Key",
			"rich_text": map[string]any{
				"equals": key,
			},
		},
		"page_size": 1,
	}
	var out struct {
		Results []struct {
			ID string `json:"id"`
		} `json:"results"`
	}
	err := n.do(ctx, http.MethodPost,
		fmt.Sprintf("https://api.notion.com/v1/databases/%s/query", n.database), body, &out)
	if err != nil {
		// A missing Key property is a setup problem, not a runtime one. Say so
		// rather than silently creating duplicates forever.
		return "", fmt.Errorf("query notion (does the database have a rich_text property named Key?): %w", err)
	}
	if len(out.Results) == 0 {
		return "", nil
	}
	return out.Results[0].ID, nil
}

func (n *Notion) create(ctx context.Context, key string, a Application) error {
	props := map[string]any{
		"Company": map[string]any{"title": []map[string]any{{"text": map[string]string{"content": a.Company}}}},
		"Role":    richText(a.Role),
		"Key":     richText(key),
		"Status":  map[string]any{"select": map[string]string{"name": "Applied"}},
		"Applied": map[string]any{"date": map[string]string{"start": a.AppliedAt.Format(time.RFC3339)}},
	}
	if a.URL != "" {
		props["URL"] = map[string]any{"url": a.URL}
	}
	if a.ATS != "" {
		props["ATS"] = richText(a.ATS)
	}

	body := map[string]any{
		"parent":     map[string]string{"database_id": n.database},
		"properties": props,
	}
	return n.do(ctx, http.MethodPost, "https://api.notion.com/v1/pages", body, nil)
}

func richText(s string) map[string]any {
	return map[string]any{"rich_text": []map[string]any{{"text": map[string]string{"content": s}}}}
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
		return fmt.Errorf("%s: %.300s", resp.Status, raw)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}
