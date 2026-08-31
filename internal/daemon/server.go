package daemon

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/lgoyal6/amac/internal/agent"
	"github.com/lgoyal6/amac/internal/apply"
	"github.com/lgoyal6/amac/internal/attention"
	"github.com/lgoyal6/amac/internal/event"
	"github.com/lgoyal6/amac/internal/health"
	"github.com/lgoyal6/amac/internal/orchestrator"
	"github.com/lgoyal6/amac/internal/queue"
	"github.com/lgoyal6/amac/internal/supervisor"
	"github.com/lgoyal6/amac/internal/tmux"
)

//go:embed ui/*
var uiFS embed.FS

type Server struct {
	sup   *supervisor.Supervisor
	log   *event.Log
	orch  *orchestrator.Orchestrator
	queue *queue.Queue
	token string
}

func New(sup *supervisor.Supervisor, log *event.Log, orch *orchestrator.Orchestrator, q *queue.Queue, token string) *Server {
	return &Server{sup: sup, log: log, orch: orch, queue: q, token: token}
}

// Token returns the shared secret, creating it on first run.
//
// The tailnet is the outer gate; this is the inner one. Anything else on the
// tailnet (a compromised device, a shared node) should still not be able to
// drive agents on this machine.
func Token() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(home, ".amac", "token")
	if b, err := os.ReadFile(path); err == nil && len(b) >= 32 {
		return string(b), nil
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(buf)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	// 0600: the token is equivalent to shell access on this machine.
	if err := os.WriteFile(path, []byte(tok), 0o600); err != nil {
		return "", err
	}
	return tok, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/sessions", s.auth(s.listSessions))
	mux.HandleFunc("POST /api/sessions", s.auth(s.startSession))
	mux.HandleFunc("POST /api/sessions/{id}/prompt", s.auth(s.promptSession))
	mux.HandleFunc("POST /api/sessions/{id}/answer", s.auth(s.answerSession))
	mux.HandleFunc("PATCH /api/sessions/{id}", s.auth(s.updateSession))
	mux.HandleFunc("DELETE /api/sessions/{id}", s.auth(s.stopSession))
	mux.HandleFunc("GET /api/sessions/{id}/pane", s.auth(s.pane))
	mux.HandleFunc("GET /api/panes", s.auth(s.panes))
	mux.HandleFunc("POST /api/sessions/{id}/keys", s.auth(s.sendKeys))
	mux.HandleFunc("GET /api/sessions/{id}/files", s.auth(s.files))
	mux.HandleFunc("GET /api/sessions/{id}/file", s.auth(s.file))
	mux.HandleFunc("GET /api/sessions/{id}/diff", s.auth(s.diff))
	mux.HandleFunc("GET /api/crew", s.auth(s.crewRuns))
	mux.HandleFunc("POST /api/crew/plan", s.auth(s.crewPlan))
	mux.HandleFunc("POST /api/crew/open", s.auth(s.crewOpen))
	mux.HandleFunc("GET /api/crew/artifact", s.auth(s.crewArtifact))
	mux.HandleFunc("GET /api/health", s.auth(s.health))
	mux.HandleFunc("GET /api/health/schedule", s.auth(s.healthSchedule))
	mux.HandleFunc("POST /api/beat/{name}", s.auth(s.beat))
	mux.HandleFunc("POST /api/health/{name}/fix", s.auth(s.healthFix))
	mux.HandleFunc("POST /api/health/{name}/shell", s.auth(s.healthShell))
	mux.HandleFunc("GET /api/spend", s.auth(s.spend))
	mux.HandleFunc("GET /api/tasks", s.auth(s.tasks))
	mux.HandleFunc("POST /api/tasks", s.auth(s.fileTask))
	mux.HandleFunc("POST /api/tasks/claim", s.auth(s.claimTask))
	mux.HandleFunc("POST /api/tasks/{id}/finish", s.auth(s.finishTask))
	mux.HandleFunc("GET /api/events", s.auth(s.events))
	mux.HandleFunc("GET /api/stream", s.auth(s.stream))
	mux.HandleFunc("GET /api/agents", s.auth(s.agents))
	mux.HandleFunc("POST /api/applications", s.auth(s.recordApplication))
	mux.HandleFunc("OPTIONS /api/", s.preflight)

	// The manifest and its icons are the only things served without a token,
	// and they have to be: iOS fetches both while adding to the home screen,
	// from a context that has none of the page's storage. Neither carries data
	// about this machine, so there is nothing to protect. The tailnet is still
	// the outer gate.
	// The manifest is generated rather than served flat, because start_url is
	// the credential. A home-screen icon pointing at "/" opens the board with
	// no token: iOS gives a standalone web app its own storage container, so
	// the token Safari stashed in localStorage is not there and the first
	// launch from the icon is always unauthorised. Worse, Safari evicts
	// script-writable storage after about a week of not opening the site, so
	// even once it works it logs you out again on its own schedule.
	//
	// Baking the token into start_url makes the icon itself the credential, so
	// a cleared localStorage cannot log anyone out. It is only echoed back to a
	// request that already presented the right token, so the open manifest
	// route still gives nothing away.
	mux.HandleFunc("GET /manifest.webmanifest", func(w http.ResponseWriter, r *http.Request) {
		b, err := uiFS.ReadFile("ui/manifest.webmanifest")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if s.tokenOK(r.URL.Query().Get("token")) {
			// A path segment rather than ?token=. Query strings on start_url
			// are the part of the manifest spec platforms are least consistent
			// about preserving, and there is no way to test that from here.
			b = bytes.Replace(b, []byte(`"start_url": "/"`),
				[]byte(`"start_url": "/app/`+url.PathEscape(s.token)+`"`), 1)
		}
		w.Header().Set("Content-Type", "application/manifest+json")
		// Never cached: one copy carries a credential and one does not, and a
		// proxy holding the wrong one installs an icon that cannot log in.
		w.Header().Set("Cache-Control", "no-store")
		w.Write(b)
	})

	for _, asset := range []struct{ path, mime string }{
		{"icon-180.png", "image/png"},
		{"icon-192.png", "image/png"},
		{"icon-512.png", "image/png"},
	} {
		mux.HandleFunc("GET /"+asset.path, func(w http.ResponseWriter, r *http.Request) {
			b, err := uiFS.ReadFile("ui/" + asset.path)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", asset.mime)
			w.Header().Set("Cache-Control", "public, max-age=86400")
			w.Write(b)
		})
	}

	// Where the home-screen icon lands. The token is in the path because that
	// is what start_url points at, and because a launch URL that carries the
	// credential is the only thing an evicted localStorage cannot undo.
	mux.HandleFunc("GET /app/{token}", func(w http.ResponseWriter, r *http.Request) {
		if !s.tokenOK(r.PathValue("token")) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		s.page(w, true)
	})

	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		s.page(w, s.tokenOK(r.URL.Query().Get("token")))
	})

	return mux
}

func (s *Server) tokenOK(got string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) == 1
}

// page serves the board, pointing the manifest link at a manifest that knows
// the token when the caller has proved they have one.
//
// The link lives in <head>, so Safari fetches it while parsing and has the
// answer cached before any script in the body runs. Rewriting the href from JS
// was therefore too late by construction: the manifest iOS installed was always
// the untokened one, and the icon always opened a board that had never seen a
// token. It has to be right in the bytes that go out.
func (s *Server) page(w http.ResponseWriter, tokened bool) {
	b, err := uiFS.ReadFile("ui/index.html")
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if tokened {
		b = bytes.Replace(b,
			[]byte(`<link rel="manifest" href="/manifest.webmanifest">`),
			[]byte(`<link rel="manifest" href="/manifest.webmanifest?token=`+
				url.QueryEscape(s.token)+`">`), 1)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The page can carry a manifest link with the token in it, so it is never
	// a shared cache's to hold.
	w.Header().Set("Cache-Control", "no-store")
	w.Write(b)
}

// auth accepts the token in a header or a query parameter. The query form is
// not laziness: EventSource cannot set headers, so an SSE stream has no other
// way to authenticate. Compared in constant time either way.
//
// It also applies CORS, because every API route can legitimately be called
// from the browser extension, not just the one that records applications. The
// options page's connection check hits /api/agents, and when only
// /api/applications carried the headers that check failed with a bare
// TypeError indistinguishable from the daemon being down.
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		allowExtensionOrigin(w, r)

		got := r.Header.Get("X-Amac-Token")
		if got == "" {
			got = r.URL.Query().Get("token")
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// allowExtensionOrigin echoes the caller's origin only when it is a browser
// extension. Not "*": these routes start agents and approve their tool calls,
// so an arbitrary website must never be handed a CORS grant even if it somehow
// learned the token.
func allowExtensionOrigin(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if !strings.HasPrefix(origin, "chrome-extension://") &&
		!strings.HasPrefix(origin, "moz-extension://") &&
		!strings.HasPrefix(origin, "safari-web-extension://") {
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Vary", "Origin")
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// ---------------------------------------------------------------- sessions --

type sessionView struct {
	ID    string `json:"id"`
	Name  string `json:"name,omitempty"`
	Agent string `json:"agent"`
	Dir   string `json:"dir"`
	State string `json:"state"`
	// Kind separates the sessions amac owns and can drive from the ones it
	// only observes. The board shows both; only "acp" accepts a prompt or an
	// answer over the API, and the UI has to know which is which or it will
	// offer buttons that cannot work.
	Kind     string       `json:"kind"`
	Attached bool         `json:"attached,omitempty"`
	Detail   string       `json:"detail"`
	Started  time.Time    `json:"started"`
	Pending  *pendingView `json:"pending,omitempty"`
	// Since is when the state was last established. It is on the card because
	// for a session amac only watches, the state is the newest thing an agent
	// said about itself and nothing refutes it afterwards. Codex has no signal
	// for "the human answered", so a session it reported blocked stays blocked
	// on the board until its next turn ends. Saying "blocked, asked 40m ago"
	// hands that judgement to the person reading it; saying "blocked" alone
	// makes a claim about now that amac cannot support.
	// A pointer, because `omitempty` does nothing for a zero time.Time: it is a
	// struct, not a basic type, so an unset one serialises as year 1 and the
	// board rendered a session nobody had heard from as "739855d ago".
	Since           *time.Time `json:"since,omitempty"`
	PermissionMode  string     `json:"permissionMode,omitempty"`
	ContinueCommand string     `json:"continueCommand,omitempty"`
}

type pendingView struct {
	Title   string       `json:"title"`
	AskedAt time.Time    `json:"askedAt"`
	Options []optionView `json:"options"`
}

type optionView struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

func view(sess *supervisor.Session) sessionView {
	st, detail := sess.State()
	name, mode := sess.Presentation()
	v := sessionView{
		ID: sess.ID, Name: name, Agent: sess.Agent, Dir: sess.Dir, Kind: "acp",
		State: string(st), Detail: detail, Started: sess.Started,
		PermissionMode: string(mode), ContinueCommand: resumeCommand(sess.Agent, sess.ACPID),
	}
	if p := sess.Pending(); p != nil {
		pv := &pendingView{Title: p.Title, AskedAt: p.AskedAt}
		for _, o := range p.Options {
			pv.Options = append(pv.Options, optionView{OptionID: o.OptionID, Name: o.Name, Kind: o.Kind})
		}
		v.Pending = pv
	}
	return v
}

func resumeCommand(agentName, id string) string {
	if id == "" {
		return ""
	}
	switch agentName {
	case "claude":
		return "claude --resume " + id
	case "codex":
		return "codex resume " + id
	default:
		return ""
	}
}

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	out := []sessionView{}
	for _, sess := range s.sup.List() {
		out = append(out, view(sess))
	}
	out = append(out, s.tmuxSessions(r.Context())...)
	writeJSON(w, 200, out)
}

// tmuxSessions lists the sessions amac did not start.
//
// Their state comes from the attention events their hooks recorded, never from
// reading the pane. That means the honest answer is often "unknown": tmux can
// prove a session exists, and the hooks can prove it asked for something, but
// nothing here can prove an agent is mid-thought. Saying "unknown" is the
// point. The predecessor guessed with a regex and was confidently wrong.
func (s *Server) tmuxSessions(ctx context.Context) []sessionView {
	list, err := tmux.List()
	if err != nil {
		// Recorded, not swallowed. An empty board and an unreadable tmux look
		// identical on screen, and this exact confusion cost an hour: the
		// daemon showed nothing while seventeen sessions were running, because
		// launchd gives an agent no locale and tmux quietly mangled its own
		// output. A board that cannot read tmux has to say so.
		if ev, e := event.New(event.KindDaemon, "daemon", "", map[string]any{
			"op": "tmux.list", "error": err.Error(),
		}); e == nil {
			_, _ = s.log.Append(ctx, ev)
		}
		return nil
	}
	if len(list) == 0 {
		return nil
	}
	last := s.lastAttention(ctx)
	states := attention.States(ctx, s.log)

	out := make([]sessionView, 0, len(list))
	for _, t := range list {
		v := sessionView{
			ID: t.Name, Agent: t.Agent(), Dir: t.Dir, Kind: "tmux",
			Attached: t.Attached, Started: t.Created,
			State: "unknown", Detail: t.Command,
			ContinueCommand: "tmux attach -t " + t.Name,
		}
		// Two sources, and the better one wins where it exists. An attention
		// event is raised only when a session wants something, so on its own
		// it leaves a session reading "blocked" long after the ask was
		// answered. Claude's hooks report every transition including the one
		// that clears it, so where they exist they are authoritative.
		if a, ok := last[t.Name]; ok {
			v.State, v.Detail = a.state, a.detail
			v.Since = when(a.at)
		}
		if st, ok := states[t.Name]; ok {
			v.State, v.Since = st.State, when(st.At)
			// The newer state's own detail, or none. Keeping the attention
			// event's text here left a card reading "working" above "Claude is
			// waiting for your input": that sentence described the moment the
			// hook state has just superseded, and a stale explanation under a
			// fresh state is worse than no explanation, because it reads as
			// current.
			v.Detail = st.Detail
			if v.Detail == "" {
				v.Detail = t.Command
			}
			// The pane command is a fact, but a wrapper or a resumed session
			// shows up as its interpreter. A hook that named itself knows
			// better than the process table does.
			if st.Agent != "" && (v.Agent == "" || v.Agent == "node") {
				v.Agent = st.Agent
			}
		}
		out = append(out, v)
	}
	return out
}

// health hands back the newest automation sweep exactly as it was recorded.
//
// The dashboard exists to replace a Discord channel, and the automation digest
// is the part of that channel that works. Re-deriving the verdicts here would
// mean two implementations of "is this automation delivering" that can
// disagree, so this is a read of what the sweep already decided.
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	// at is TEXT in the store, so it is parsed here rather than scanned into a
	// time.Time. The driver does not convert it, and a failed scan would look
	// exactly like "no sweep has ever run".
	var at string
	var payload []byte
	err := s.log.DB().QueryRowContext(r.Context(),
		`SELECT at, payload FROM events WHERE kind = ? ORDER BY seq DESC LIMIT 1`,
		string(event.KindAutomationCheck)).Scan(&at, &payload)
	if err != nil {
		// No sweep on record is not an error. It means the launchd job has
		// not run yet, and the board should say so rather than show a failure.
		writeJSON(w, 200, map[string]any{"reports": []any{}})
		return
	}
	var body struct {
		Reports []health.Report `json:"reports"`
	}
	if json.Unmarshal(payload, &body) != nil {
		writeJSON(w, 200, map[string]any{"reports": []any{}})
		return
	}

	// Home is joined in from the registry rather than read back from the log.
	// It is static configuration, so recording a copy of it on every sweep
	// would be storing the same string 96 times a day, and a log that repeats
	// itself is a log with less signal in it.
	type reportView struct {
		health.Report
		Home string `json:"home,omitempty"`
	}
	out := make([]reportView, 0, len(body.Reports))
	for _, rep := range body.Reports {
		v := reportView{Report: rep}
		if a, ok := health.Find(s.log, rep.Name); ok {
			v.Home = a.Home
		}
		out = append(out, v)
	}

	swept, _ := time.Parse(time.RFC3339Nano, at)
	writeJSON(w, 200, map[string]any{"at": swept, "reports": out})
}

type scheduleView struct {
	Name  string `json:"name"`
	What  string `json:"what"`
	Every string `json:"every,omitempty"`
	Grace string `json:"grace,omitempty"`
	Probe string `json:"probe"`
	Check string `json:"check"`
	Role  string `json:"role"`
}

func (s *Server) healthSchedule(w http.ResponseWriter, r *http.Request) {
	decls, err := health.Declarations(health.ConfigPath())
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	out := make([]scheduleView, 0, len(decls))
	for _, d := range decls {
		out = append(out, scheduleView{
			Name: d.Name, What: d.What, Every: d.Every, Grace: d.Grace,
			Probe: d.Probe, Check: probeExplanation(d.Probe),
			Role: "AMAC monitors this; the job itself runs elsewhere.",
		})
	}
	writeJSON(w, 200, map[string]any{"automations": out})
}

func probeExplanation(probe string) string {
	switch probe {
	case "launchd_marker":
		return "Reads the job's launchd status and its completion line in a log."
	case "marker_fields":
		return "Reads named measurements from a completion line in a log."
	case "service":
		return "Checks that the service process is loaded and its port answers."
	case "spend_snapshot":
		return "Reads the latest saved spend report."
	case "github_delivery_file":
		return "Reads a committed delivery marker from GitHub."
	case "github_newest_file":
		return "Finds the newest matching delivery file on GitHub."
	case "n8n":
		return "Reads the workflow's recent runs from n8n."
	case "heartbeat":
		return "Reads the latest heartbeat sent to AMAC."
	case "systemd_unit":
		return "Reads the service and timer state from systemd."
	default:
		return "Runs the configured evidence check."
	}
}

// when returns a timestamp only when there is one. A zero time is not a very
// old fact, it is the absence of a fact, and rendering it as an age says
// something false with total confidence.
func when(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

type attnState struct {
	state, detail string
	at            time.Time
}

// lastAttention reads the newest usable attention event per session.
//
// Usable is doing real work here. A finished Codex turn fires both signals
// within the same second: the notify hook, which knows what the agent said, and
// the terminal bell, which knows only that something happened. The bell arrives
// second, amac recognises it as a duplicate and withholds it, and that decision
// is correct and was working.
//
// The board then read the newest event regardless, so the withheld bell
// overwrote the turn-complete it duplicated and the card read blocked. Not for a
// moment: until the next turn, which for a session waiting on a human is
// indefinitely. Sessions sat there claiming to want something four hours after
// they had finished and said so.
//
// A signal suppressed *because it duplicates another* is not independent
// evidence of anything, so it is skipped and the one it duplicated is used.
// Suppression for any other reason is left alone: "you are looking at it" means
// the session genuinely did want attention and amac merely declined to
// interrupt, which is a real block and should still show as one.
//
// Five per session rather than one, because the duplicate can itself be
// preceded by another. One query still, for a page that refreshes constantly.
func (s *Server) lastAttention(ctx context.Context) map[string]attnState {
	rows, err := s.log.DB().QueryContext(ctx, `
		SELECT session, at, payload FROM (
		  SELECT session, at, payload, seq,
		         ROW_NUMBER() OVER (PARTITION BY session ORDER BY seq DESC) AS rn
		    FROM events WHERE kind = ? AND session != ''
		) WHERE rn <= 5 ORDER BY session, seq DESC`,
		string(event.KindAttention))
	if err != nil {
		return nil
	}
	defer rows.Close()

	out := map[string]attnState{}
	for rows.Next() {
		var sess, at string
		var payload []byte
		if err := rows.Scan(&sess, &at, &payload); err != nil {
			continue
		}
		if _, taken := out[sess]; taken {
			continue // rows are newest-first, so the first usable one wins
		}
		var body struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
			Outcome struct {
				Sent bool   `json:"sent"`
				Why  string `json:"why"`
			} `json:"outcome"`
		}
		if err := json.Unmarshal(payload, &body); err != nil {
			continue
		}
		if isDuplicateSignal(body.Outcome.Sent, body.Outcome.Why) {
			continue
		}
		a := attnState{state: "idle", detail: body.Message}
		if body.Reason == "wants-attention" {
			a.state = "blocked"
			if a.detail == "" {
				a.detail = "asked for you"
			}
		}
		if a.detail == "" {
			a.detail = "finished its turn"
		}
		a.at, _ = time.Parse(time.RFC3339Nano, at)
		out[sess] = a
	}
	return out
}

// dedupePrefix is the reason attention.Handle records when it withholds a
// signal for being the same event it has already reported. Matching on the
// string is not ideal and is still better than the alternative on offer, which
// was a board that says blocked about sessions that are not.
const dedupePrefix = "already notified"

func isDuplicateSignal(sent bool, why string) bool {
	return !sent && strings.HasPrefix(why, dedupePrefix)
}

func (s *Server) agents(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, agent.Names())
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Agent          string                    `json:"agent"`
		Dir            string                    `json:"dir"`
		Prompt         string                    `json:"prompt"`
		Name           string                    `json:"name"`
		PermissionMode supervisor.PermissionMode `json:"permissionMode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if body.Agent == "" {
		body.Agent = "claude"
	}
	if body.Dir == "" {
		home, _ := os.UserHomeDir()
		body.Dir = home
	}
	if body.PermissionMode == "" {
		body.PermissionMode = supervisor.PermissionAsk
	}
	if strings.TrimSpace(body.Name) == "" {
		body.Name = defaultSessionName(body.Agent, time.Now())
	}

	// Spawning is slow enough (adapter start plus handshake) that holding the
	// request open would look like a hang on a phone. Detach from the request
	// context so a client that navigates away does not kill the session it
	// just asked for.
	sess, err := s.sup.Start(context.Background(), body.Agent, body.Dir)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	if err := sess.SetPresentation(body.Name, body.PermissionMode); err != nil {
		sess.Close()
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if body.Prompt != "" {
		go func() { _, _ = sess.Prompt(context.Background(), body.Prompt) }()
	}
	writeJSON(w, 200, view(sess))
}

func defaultSessionName(agentName string, now time.Time) string {
	return fmt.Sprintf("%s %s", agentName, now.Local().Format("15:04:05"))
}

func (s *Server) updateSession(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sup.Get(r.PathValue("id"))
	if !ok {
		writeJSON(w, 404, map[string]string{"error": "no such managed session"})
		return
	}
	name, mode := sess.Presentation()
	var body struct {
		Name           *string                    `json:"name"`
		PermissionMode *supervisor.PermissionMode `json:"permissionMode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if body.Name != nil {
		name = *body.Name
	}
	if body.PermissionMode != nil {
		mode = *body.PermissionMode
	}
	if err := sess.SetPresentation(name, mode); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, view(sess))
}

func (s *Server) promptSession(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sup.Get(r.PathValue("id"))
	if !ok {
		writeJSON(w, 404, map[string]string{"error": "no such session"})
		return
	}
	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Text == "" {
		writeJSON(w, 400, map[string]string{"error": "text required"})
		return
	}
	// A turn can run for minutes and will block on permission requests. Return
	// immediately; progress arrives on the event stream.
	go func() { _, _ = sess.Prompt(context.Background(), body.Text) }()
	writeJSON(w, 202, map[string]string{"status": "accepted"})
}

func (s *Server) answerSession(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sup.Get(r.PathValue("id"))
	if !ok {
		writeJSON(w, 404, map[string]string{"error": "no such session"})
		return
	}
	var body struct {
		OptionID string `json:"optionId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if err := sess.Answer(body.OptionID); err != nil {
		writeJSON(w, 409, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]string{"status": "answered"})
}

func (s *Server) stopSession(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sup.Get(r.PathValue("id"))
	if !ok {
		writeJSON(w, 404, map[string]string{"error": "no such session"})
		return
	}
	sess.Close()
	writeJSON(w, 200, map[string]string{"status": "stopped"})
}

// ------------------------------------------------------------ applications --

// preflight answers the browser extension's CORS check.
//
// The allowed origin is chrome-extension://*, not "*": this endpoint writes to
// a tracker and, with a token, is reachable from any page the browser loads.
// Echoing the extension origin keeps a random website from posting here even
// if it somehow learned the token.
func (s *Server) preflight(w http.ResponseWriter, r *http.Request) {
	allowExtensionOrigin(w, r)
	if w.Header().Get("Access-Control-Allow-Origin") != "" {
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Amac-Token")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) recordApplication(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Company string `json:"company"`
		Role    string `json:"role"`
		URL     string `json:"url"`
		ATS     string `json:"ats"`
		Source  string `json:"source"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if body.Company == "" {
		writeJSON(w, 400, map[string]string{"error": "company required"})
		return
	}

	app := apply.Application{
		Company: body.Company, Role: body.Role, URL: body.URL, ATS: body.ATS,
		Source: apply.Source(body.Source), AppliedAt: time.Now(),
	}
	if app.Role == "" {
		app.Role = "Unspecified"
	}
	if app.ATS == "" {
		if ats, ok := apply.DetectATS(body.URL); ok {
			app.ATS = ats
		}
	}

	// The Notion sink is optional so a missing token degrades to local-only
	// tracking rather than dropping the detection entirely.
	var sink apply.Sink
	if n, err := apply.NewNotion(); err == nil {
		sink = n
	}
	isNew, err := apply.NewTracker(s.log, sink).Record(r.Context(), app)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"recorded": isNew, "key": app.Key(), "company": app.Company, "role": app.Role})
}

// ---------------------------------------------------------------- events ----

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	evs, err := s.log.Since(r.Context(), since, limit)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	if evs == nil {
		evs = []event.Event{}
	}
	writeJSON(w, 200, evs)
}

// stream is Server-Sent Events rather than a WebSocket. The choice is not
// arbitrary: SSE carries an event id, and browsers resend the last one they
// saw as Last-Event-ID after a drop. Since our ids are the log's sequence
// numbers, reconnect-with-replay is the protocol's default behaviour instead
// of something to hand-roll, and phones drop connections constantly.
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", 500)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)

	// Subscribe before replaying, so an event appended between the two is
	// delivered late rather than lost. Duplicates are fine for a client that
	// tracks the last sequence it rendered; a gap is not.
	live, unsub := s.log.Subscribe(512)
	defer unsub()

	from, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	if lastID := r.Header.Get("Last-Event-ID"); lastID != "" {
		if n, err := strconv.ParseInt(lastID, 10, 64); err == nil {
			from = n
		}
	}

	sent := from
	if backlog, err := s.log.Since(r.Context(), from, 500); err == nil {
		for _, e := range backlog {
			writeEvent(w, e)
			sent = e.Seq
		}
	}
	flusher.Flush()

	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return

		case e, ok := <-live:
			if !ok {
				return
			}
			if e.Seq <= sent {
				continue // already delivered in the backlog
			}
			writeEvent(w, e)
			sent = e.Seq
			flusher.Flush()

		case <-ticker.C:
			// Comment frame: keeps intermediaries and sleeping phones from
			// silently dropping an idle connection.
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

func writeEvent(w http.ResponseWriter, e event.Event) {
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", e.Seq, e.Kind, b)
}
