// Package apply tracks job applications without being told about them.
//
// Two sources, on purpose, because neither is sufficient alone:
//
//   - The browser extension fires the instant you submit on a known ATS. Fast,
//     but it only sees what you do in that browser and can be defeated by a
//     redirect or an unrecognised host.
//   - The confirmation email is ground truth. Slower by minutes, but it
//     arrives for every real application, names the company, and cannot be
//     missed by a DOM change.
//
// They are reconciled rather than merged blindly: the same application seen
// twice must produce one row. OS-level form detection was considered and
// rejected; it is fragile per-site, misfires constantly, and buys only the
// same seconds the extension already provides.
package apply

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/lgoyal6/amac/internal/event"
)

type Source string

const (
	SourceExtension Source = "extension"
	SourceEmail     Source = "email"
)

type Application struct {
	ID          string     `json:"key,omitempty"`
	NotionID    string     `json:"notionId,omitempty"`
	NotionURL   string     `json:"notionUrl,omitempty"`
	Company     string     `json:"company"`
	Role        string     `json:"role"`
	URL         string     `json:"url,omitempty"`
	ATS         string     `json:"ats,omitempty"`
	Source      Source     `json:"source"`
	AppliedAt   time.Time  `json:"appliedAt"`
	Status      string     `json:"status"`
	Category    string     `json:"category,omitempty"`
	Cycle       string     `json:"cycle,omitempty"`
	Location    string     `json:"location,omitempty"`
	WorkAuth    string     `json:"workAuth,omitempty"`
	Tier        string     `json:"tier,omitempty"`
	Sponsorship *bool      `json:"sponsorship,omitempty"`
	FollowUpAt  *time.Time `json:"followUpAt,omitempty"`
	UpdatedAt   time.Time  `json:"updatedAt,omitempty"`
	SyncState   string     `json:"syncState,omitempty"`
	SyncError   string     `json:"syncError,omitempty"`
}

// Key is the reconciliation identity. Company plus a normalised role, because
// those are the two fields both sources reliably produce: URLs differ between
// the apply page and the confirmation email, and timestamps differ by minutes.
func (a Application) Key() string {
	norm := func(s string) string {
		s = strings.ToLower(strings.TrimSpace(s))
		s = regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(s, " ")
		return strings.TrimSpace(s)
	}
	sum := sha256.Sum256([]byte(norm(a.Company) + "|" + norm(a.Role)))
	return hex.EncodeToString(sum[:8])
}

// ---------------------------------------------------------------- ATS -------

// Known applicant tracking systems. Matching the host is what makes extension
// detection precise: these domains only appear when you are actually applying.
var atsHosts = map[string]string{
	"greenhouse.io":     "Greenhouse",
	"boards.greenhouse": "Greenhouse",
	"lever.co":          "Lever",
	"jobs.lever.co":     "Lever",
	"ashbyhq.com":       "Ashby",
	"myworkdayjobs.com": "Workday",
	"workday.com":       "Workday",
	"smartrecruiters":   "SmartRecruiters",
	"icims.com":         "iCIMS",
	"bamboohr.com":      "BambooHR",
	"rippling.com":      "Rippling",
	"jobvite.com":       "Jobvite",
	"taleo.net":         "Taleo",
}

func DetectATS(url string) (string, bool) {
	u := strings.ToLower(url)
	for host, name := range atsHosts {
		if strings.Contains(u, host) {
			return name, true
		}
	}
	return "", false
}

// ---------------------------------------------------------------- email -----

var (
	reThanks = regexp.MustCompile(`(?i)\b(thank you for (your )?(applying|application|interest)|we('| ha)ve received your application|application (has been )?received|thanks for applying)\b`)
	// Two traps here, both of which produced a role of "applying to the
	// Backend Engineer" before being fixed:
	//
	//  1. The article must be consumed and followed by whitespace. Written as
	//     `(?:the|a|an)?\s*`, the bare "a" matches the first letter of
	//     "applying".
	//  2. The first captured character must be genuinely uppercase. Under
	//     `(?i)` the class [A-Z] matches lowercase too, so the role title
	//     could start anywhere in the sentence. `(?-i:...)` re-enables case
	//     sensitivity for exactly that one character.
	reRole = regexp.MustCompile(`(?i)\b(?:for|position of|role of|applying to)\s+(?:(?:the|our|a|an)\s+)?((?-i:[A-Z])[A-Za-z0-9 ,/&+-]{3,60}?)\s*(?:position|role|opening|\(|\.|,|$)`)

	// Employers send careers mail from a branded sub-identity. "Vercel Careers"
	// and "Vercel" are the same company, and letting both through would file
	// one employer under two names.
	reOrgSuffix = regexp.MustCompile(`(?i)\s+(careers?|recruiting|recruitment|talent|hiring|jobs?|people|hr|team)$`)
	reAt        = regexp.MustCompile(`(?i)\bat\s+([A-Z][A-Za-z0-9.&' -]{1,40})`)
)

// FromEmail extracts an application from a confirmation email.
//
// Deliberately conservative: it returns false unless the body actually reads
// like a confirmation. A tracker that invents applications from newsletters is
// worse than one that occasionally misses, because you stop trusting it.
func FromEmail(subject, from, body string, received time.Time) (Application, bool) {
	text := subject + "\n" + body
	if !reThanks.MatchString(text) {
		return Application{}, false
	}

	app := Application{Source: SourceEmail, AppliedAt: received}

	// The sending domain is the most reliable company signal; the display name
	// is the fallback since ATS mail often comes from the ATS's own domain.
	if company := companyFromSender(from); company != "" {
		app.Company = company
	}
	if m := reAt.FindStringSubmatch(subject); len(m) > 1 && app.Company == "" {
		app.Company = strings.TrimSpace(m[1])
	}
	if m := reRole.FindStringSubmatch(text); len(m) > 1 {
		app.Role = strings.TrimSpace(m[1])
	}
	if ats, ok := DetectATS(from + " " + body); ok {
		app.ATS = ats
	}

	if app.Company == "" {
		return app, false
	}
	if app.Role == "" {
		app.Role = "Unspecified"
	}
	return app, true
}

func companyFromSender(from string) string {
	// Prefer the display name when it is not the ATS.
	if i := strings.Index(from, "<"); i > 0 {
		name := strings.Trim(strings.TrimSpace(from[:i]), `"`)
		name = strings.TrimSpace(reOrgSuffix.ReplaceAllString(name, ""))
		if name != "" && !isATSName(name) && !strings.EqualFold(name, "no-reply") {
			return name
		}
	}
	at := strings.LastIndex(from, "@")
	if at < 0 {
		return ""
	}
	domain := strings.Trim(from[at+1:], "> ")
	if _, isATS := DetectATS(domain); isATS {
		return "" // the ATS is not the employer
	}
	parts := strings.Split(domain, ".")
	if len(parts) < 2 {
		return ""
	}
	c := parts[len(parts)-2]
	return strings.ToUpper(c[:1]) + c[1:]
}

func isATSName(s string) bool {
	l := strings.ToLower(s)
	for _, n := range atsHosts {
		if strings.Contains(l, strings.ToLower(n)) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- tracker ---

type Tracker struct {
	log  *event.Log
	sink Sink
	repo *Repository
}

// Sink is where confirmed applications go. Notion is one implementation;
// keeping it an interface means the detection logic is testable without a
// network or a token, which is most of why it is an interface.
type Sink interface {
	Upsert(ctx context.Context, key string, a Application) error
	Name() string
}

func NewTracker(log *event.Log, sink Sink) *Tracker {
	repo, _ := NewRepository(log.DB())
	return &Tracker{log: log, sink: sink, repo: repo}
}

// Record reconciles and persists. Returns whether this was new.
func (t *Tracker) Record(ctx context.Context, a Application) (bool, error) {
	key := a.Key()
	seen, err := t.seen(ctx, key)
	if err != nil {
		return false, err
	}

	ev, err := event.New(event.KindApplication, "apply", "", map[string]any{
		"key": key, "company": a.Company, "role": a.Role, "url": a.URL,
		"ats": a.ATS, "source": string(a.Source), "duplicate": seen,
	})
	if err != nil {
		return false, err
	}
	if _, err := t.log.Append(ctx, ev); err != nil {
		return false, err
	}
	if a.Status == "" {
		a.Status = "Applied"
	}
	if t.repo != nil {
		// A second signal (usually the confirmation email after the browser
		// extension) is evidence about the same application, not a request to
		// move an Interview or Offer back to Applied.
		if seen {
			if current, err := t.repo.Get(ctx, key); err == nil {
				a.Status = current.Status
				a.AppliedAt = current.AppliedAt
				a.FollowUpAt = current.FollowUpAt
				a.NotionID = current.NotionID
				a.NotionURL = current.NotionURL
			}
		}
		if _, err := t.repo.UpsertLocal(ctx, a); err != nil {
			return false, err
		}
	}
	if t.sink != nil {
		if err := t.sink.Upsert(ctx, key, a); err != nil {
			// The local cache is the primary store. Notion is a backup, so an
			// outage is recorded for the dashboard and retried by the next sync;
			// it must not turn a successfully captured application into a 500.
			if t.repo != nil {
				_ = t.repo.MarkSyncError(ctx, key, fmt.Sprintf("%s: %v", t.sink.Name(), err))
			}
			return !seen, nil
		}
		if t.repo != nil {
			_ = t.repo.MarkSynced(ctx, key, time.Now())
		}
	}
	return !seen, nil
}

func (t *Tracker) seen(ctx context.Context, key string) (bool, error) {
	var n int
	err := t.log.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events WHERE kind='application' AND json_extract(payload,'$.key')=? AND json_extract(payload,'$.duplicate')=0`,
		key).Scan(&n)
	return n > 0, err
}
