package apply

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const applicationSchema = `
CREATE TABLE IF NOT EXISTS applications (
  key TEXT PRIMARY KEY,
  notion_id TEXT NOT NULL DEFAULT '',
  notion_url TEXT NOT NULL DEFAULT '',
  company TEXT NOT NULL,
  role TEXT NOT NULL,
  url TEXT NOT NULL DEFAULT '',
  ats TEXT NOT NULL DEFAULT '',
  source TEXT NOT NULL DEFAULT '',
  applied_at TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'Applied',
  category TEXT NOT NULL DEFAULT '',
  cycle TEXT NOT NULL DEFAULT '',
  location TEXT NOT NULL DEFAULT '',
  work_auth TEXT NOT NULL DEFAULT '',
  tier TEXT NOT NULL DEFAULT '',
  sponsorship INTEGER,
  follow_up_at TEXT,
  updated_at TEXT NOT NULL,
  sync_state TEXT NOT NULL DEFAULT 'local',
  sync_error TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_applications_notion_id
  ON applications(notion_id) WHERE notion_id <> '';
CREATE INDEX IF NOT EXISTS idx_applications_status_applied
  ON applications(status, applied_at DESC);
CREATE TABLE IF NOT EXISTS application_sync_meta (
  source TEXT PRIMARY KEY,
  synced_at TEXT NOT NULL,
  item_count INTEGER NOT NULL DEFAULT 0,
  error TEXT NOT NULL DEFAULT ''
);`

type Repository struct{ db *sql.DB }

func NewRepository(db *sql.DB) (*Repository, error) {
	if _, err := db.Exec(applicationSchema); err != nil {
		return nil, fmt.Errorf("create application cache: %w", err)
	}
	return &Repository{db: db}, nil
}

func (r *Repository) UpsertLocal(ctx context.Context, a Application) (Application, error) {
	if strings.TrimSpace(a.Company) == "" {
		return Application{}, fmt.Errorf("company required")
	}
	if a.Role == "" {
		a.Role = "Unspecified"
	}
	if a.Status == "" {
		a.Status = "Applied"
	}
	if a.AppliedAt.IsZero() {
		a.AppliedAt = time.Now()
	}
	if a.ID == "" {
		a.ID = a.Key()
	}
	now := time.Now().UTC()
	_, err := r.db.ExecContext(ctx, `INSERT INTO applications
      (key,notion_id,notion_url,company,role,url,ats,source,applied_at,status,category,cycle,location,work_auth,tier,sponsorship,follow_up_at,updated_at,sync_state,sync_error)
    VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
    ON CONFLICT(key) DO UPDATE SET
      notion_id=CASE WHEN excluded.notion_id<>'' THEN excluded.notion_id ELSE applications.notion_id END,
      notion_url=CASE WHEN excluded.notion_url<>'' THEN excluded.notion_url ELSE applications.notion_url END,
      company=excluded.company, role=excluded.role,
      url=CASE WHEN excluded.url<>'' THEN excluded.url ELSE applications.url END,
      ats=CASE WHEN excluded.ats<>'' THEN excluded.ats ELSE applications.ats END,
      source=CASE WHEN excluded.source<>'' THEN excluded.source ELSE applications.source END,
      applied_at=excluded.applied_at, status=excluded.status,
      category=CASE WHEN excluded.category<>'' THEN excluded.category ELSE applications.category END,
      cycle=CASE WHEN excluded.cycle<>'' THEN excluded.cycle ELSE applications.cycle END,
      location=CASE WHEN excluded.location<>'' THEN excluded.location ELSE applications.location END,
      work_auth=CASE WHEN excluded.work_auth<>'' THEN excluded.work_auth ELSE applications.work_auth END,
      tier=CASE WHEN excluded.tier<>'' THEN excluded.tier ELSE applications.tier END,
      sponsorship=COALESCE(excluded.sponsorship, applications.sponsorship),
      follow_up_at=COALESCE(excluded.follow_up_at, applications.follow_up_at),
      updated_at=excluded.updated_at, sync_state='pending', sync_error=''`,
		a.ID, a.NotionID, a.NotionURL, a.Company, a.Role, a.URL, a.ATS, string(a.Source),
		a.AppliedAt.UTC().Format(time.RFC3339Nano), a.Status, a.Category, a.Cycle, a.Location,
		a.WorkAuth, a.Tier, boolDB(a.Sponsorship), timeDB(a.FollowUpAt), now.Format(time.RFC3339Nano), "pending", "")
	if err != nil {
		return Application{}, fmt.Errorf("cache application: %w", err)
	}
	return r.Get(ctx, a.ID)
}

// UpsertFromNotion hydrates the cache without overwriting a local edit that
// has not yet reached Notion. That ordering makes an explicit status change in
// AMAC win over the stale backup value during a reconciliation pass.
func (r *Repository) UpsertFromNotion(ctx context.Context, a Application) (Application, error) {
	if a.ID == "" {
		a.ID = a.Key()
	}
	if a.AppliedAt.IsZero() {
		a.AppliedAt = time.Now()
	}
	if a.Status == "" {
		a.Status = "Applied"
	}
	now := time.Now().UTC()
	_, err := r.db.ExecContext(ctx, `INSERT INTO applications
      (key,notion_id,notion_url,company,role,url,ats,source,applied_at,status,category,cycle,location,work_auth,tier,sponsorship,updated_at,sync_state)
    VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
    ON CONFLICT(key) DO UPDATE SET
      notion_id=excluded.notion_id, notion_url=excluded.notion_url,
      company=excluded.company, role=excluded.role, url=excluded.url,
      applied_at=CASE WHEN applications.sync_state IN ('pending','error') THEN applications.applied_at ELSE excluded.applied_at END,
      status=CASE WHEN applications.sync_state IN ('pending','error') THEN applications.status ELSE excluded.status END,
      category=excluded.category, cycle=excluded.cycle, location=excluded.location,
      work_auth=excluded.work_auth, tier=excluded.tier, sponsorship=excluded.sponsorship,
      updated_at=excluded.updated_at,
      sync_state=CASE WHEN applications.sync_state IN ('pending','error') THEN applications.sync_state ELSE 'synced' END,
      sync_error=CASE WHEN applications.sync_state IN ('pending','error') THEN applications.sync_error ELSE '' END`,
		a.ID, a.NotionID, a.NotionURL, a.Company, a.Role, a.URL, a.ATS, string(SourceNotion),
		a.AppliedAt.UTC().Format(time.RFC3339Nano), a.Status, a.Category, a.Cycle, a.Location,
		a.WorkAuth, a.Tier, boolDB(a.Sponsorship), now.Format(time.RFC3339Nano), "synced")
	if err != nil {
		return Application{}, fmt.Errorf("import notion application: %w", err)
	}
	return r.Get(ctx, a.ID)
}

type ListOptions struct {
	Query, Status string
	Limit         int
}

func (r *Repository) List(ctx context.Context, o ListOptions) ([]Application, error) {
	if o.Limit <= 0 || o.Limit > 1000 {
		o.Limit = 200
	}
	where, args := []string{"1=1"}, []any{}
	if o.Query != "" {
		where = append(where, `(lower(company) LIKE ? OR lower(role) LIKE ? OR lower(location) LIKE ?)`)
		q := "%" + strings.ToLower(o.Query) + "%"
		args = append(args, q, q, q)
	}
	if o.Status != "" {
		where = append(where, "status=?")
		args = append(args, o.Status)
	}
	args = append(args, o.Limit)
	rows, err := r.db.QueryContext(ctx, `SELECT key,notion_id,notion_url,company,role,url,ats,source,applied_at,status,category,cycle,location,work_auth,tier,sponsorship,follow_up_at,updated_at,sync_state,sync_error
      FROM applications WHERE `+strings.Join(where, " AND ")+` ORDER BY applied_at DESC, company LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Application{}
	for rows.Next() {
		a, err := scanApplication(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (r *Repository) Get(ctx context.Context, key string) (Application, error) {
	row := r.db.QueryRowContext(ctx, `SELECT key,notion_id,notion_url,company,role,url,ats,source,applied_at,status,category,cycle,location,work_auth,tier,sponsorship,follow_up_at,updated_at,sync_state,sync_error FROM applications WHERE key=?`, key)
	return scanApplication(row)
}

type scanner interface{ Scan(...any) error }

func scanApplication(s scanner) (Application, error) {
	var a Application
	var source, applied, updated string
	var sponsor sql.NullBool
	var follow sql.NullString
	err := s.Scan(&a.ID, &a.NotionID, &a.NotionURL, &a.Company, &a.Role, &a.URL, &a.ATS, &source, &applied, &a.Status, &a.Category, &a.Cycle, &a.Location, &a.WorkAuth, &a.Tier, &sponsor, &follow, &updated, &a.SyncState, &a.SyncError)
	if err != nil {
		return a, err
	}
	a.Source = Source(source)
	a.AppliedAt, _ = time.Parse(time.RFC3339Nano, applied)
	a.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	if sponsor.Valid {
		v := sponsor.Bool
		a.Sponsorship = &v
	}
	if follow.Valid && follow.String != "" {
		if v, e := time.Parse("2006-01-02", follow.String); e == nil {
			a.FollowUpAt = &v
		}
	}
	return a, nil
}

type Update struct {
	Status     *string `json:"status"`
	FollowUpAt *string `json:"followUpAt"`
}

func (r *Repository) Update(ctx context.Context, key string, u Update) (Application, error) {
	if u.Status == nil && u.FollowUpAt == nil {
		return Application{}, fmt.Errorf("status or followUpAt required")
	}
	sets := []string{"updated_at=?", "sync_state='pending'", "sync_error=''"}
	args := []any{time.Now().UTC().Format(time.RFC3339Nano)}
	if u.Status != nil {
		if !ValidStatus(*u.Status) {
			return Application{}, fmt.Errorf("unknown status %q", *u.Status)
		}
		sets = append(sets, "status=?")
		args = append(args, *u.Status)
	}
	if u.FollowUpAt != nil {
		if *u.FollowUpAt != "" {
			if _, e := time.Parse("2006-01-02", *u.FollowUpAt); e != nil {
				return Application{}, fmt.Errorf("followUpAt must be YYYY-MM-DD")
			}
		}
		sets = append(sets, "follow_up_at=?")
		if *u.FollowUpAt == "" {
			args = append(args, nil)
		} else {
			args = append(args, *u.FollowUpAt)
		}
	}
	args = append(args, key)
	res, err := r.db.ExecContext(ctx, "UPDATE applications SET "+strings.Join(sets, ",")+" WHERE key=?", args...)
	if err != nil {
		return Application{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Application{}, sql.ErrNoRows
	}
	return r.Get(ctx, key)
}

func ValidStatus(s string) bool {
	switch s {
	case "Collected", "Applied", "In Review", "Interview", "Offer", "Rejected", "New":
		return true
	}
	return false
}

func (r *Repository) MarkSynced(ctx context.Context, key string, at time.Time) error {
	_, e := r.db.ExecContext(ctx, "UPDATE applications SET sync_state='synced',sync_error='',updated_at=? WHERE key=?", at.UTC().Format(time.RFC3339Nano), key)
	return e
}
func (r *Repository) MarkSyncError(ctx context.Context, key, msg string) error {
	_, e := r.db.ExecContext(ctx, "UPDATE applications SET sync_state='error',sync_error=? WHERE key=?", msg, key)
	return e
}
func (r *Repository) Pending(ctx context.Context) ([]Application, error) {
	rows, e := r.List(ctx, ListOptions{Limit: 1000})
	if e != nil {
		return nil, e
	}
	out := rows[:0]
	for _, a := range rows {
		if a.SyncState == "pending" || a.SyncState == "error" || a.SyncState == "local" {
			out = append(out, a)
		}
	}
	return out, nil
}

type SyncMeta struct {
	SyncedAt  time.Time `json:"syncedAt,omitempty"`
	ItemCount int       `json:"itemCount"`
	Error     string    `json:"error,omitempty"`
}

func (r *Repository) SetSyncMeta(ctx context.Context, m SyncMeta) error {
	_, e := r.db.ExecContext(ctx, `INSERT INTO application_sync_meta(source,synced_at,item_count,error) VALUES('notion',?,?,?) ON CONFLICT(source) DO UPDATE SET synced_at=excluded.synced_at,item_count=excluded.item_count,error=excluded.error`, m.SyncedAt.UTC().Format(time.RFC3339Nano), m.ItemCount, m.Error)
	return e
}
func (r *Repository) SyncMeta(ctx context.Context) (SyncMeta, error) {
	var m SyncMeta
	var at string
	e := r.db.QueryRowContext(ctx, "SELECT synced_at,item_count,error FROM application_sync_meta WHERE source='notion'").Scan(&at, &m.ItemCount, &m.Error)
	if e == sql.ErrNoRows {
		return m, nil
	}
	if e != nil {
		return m, e
	}
	m.SyncedAt, _ = time.Parse(time.RFC3339Nano, at)
	return m, nil
}

func boolDB(v *bool) any {
	if v == nil {
		return nil
	}
	return *v
}
func timeDB(v *time.Time) any {
	if v == nil {
		return nil
	}
	return v.Format("2006-01-02")
}
