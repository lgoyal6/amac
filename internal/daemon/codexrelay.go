package daemon

// The codex-relay tab.
//
// codex-relay pools several ChatGPT sign-ins behind one Codex client and decides which
// account serves each conversation. Its own dashboard already answers every question about
// that, so nothing here re-derives any of it: this asks the relay and reshapes the answer.
// Re-deriving would mean two implementations of the routing rules, and the one on this side
// would be wrong the first time the other changed.
//
// It is a read through amac's own server rather than a window onto the relay. That is the
// whole reason this exists in native form: codex-relay binds to loopback and refuses to do
// otherwise, so the only way to reach it from a phone is for something already on the
// machine to fetch it. amac is that something. Nothing new is exposed, because the relay is
// still only ever addressed from localhost.
//
// codex-relay knows nothing about amac, and no change to its repository is needed.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/lgoyal6/amac/internal/health"
)

// relayRosterName ties this tab to the roster entry that already states the port. A port
// written here as well would report the relay down the day it moves, which is the same trap
// the service probe avoids.
const relayRosterName = "codex-relay"

// bootToken finds the session token codex-relay mints into its index page. Every one of its
// API routes demands the token, and it is regenerated per service start, so it is read at
// call time rather than stored.
var bootToken = regexp.MustCompile(`id="codexrelay-boot"[^>]*>(.*?)</script>`)

type relayWindow struct {
	Label     string  `json:"label"`
	Remaining float64 `json:"remaining_percent"`
	ResetsAt  string  `json:"resets_at"`
	Evidence  string  `json:"evidence"`
	Reserve   float64 `json:"reserve_marker_percent,omitempty"`
}

type relayWorkspace struct {
	ID           string        `json:"id"`
	Name         string        `json:"name"`
	AccountID    string        `json:"account_id,omitempty"`
	Paused       bool          `json:"paused"`
	CredentialOK bool          `json:"credential_ok"`
	Protected    bool          `json:"protected"`
	IsDefault    bool          `json:"is_default"`
	IsNext       bool          `json:"is_next"`
	Windows      []relayWindow `json:"windows"`
}

type relayProfile struct {
	Name     string `json:"name"`
	Command  string `json:"command"`
	Active   bool   `json:"active"`
	Sentence string `json:"sentence"`
}

type relayRule struct {
	Kind     string `json:"kind"`
	Enabled  bool   `json:"enabled"`
	Sentence string `json:"sentence"`
}

type relayTurn struct {
	At      string `json:"at"`
	Name    string `json:"workspace"`
	Outcome string `json:"outcome"`
	Reason  string `json:"reason"`
	Error   string `json:"error,omitempty"`
	Summary string `json:"summary,omitempty"`
}

type relaySummary struct {
	Turns        int     `json:"turns"`
	Served       int     `json:"turns_served"`
	Blocked      int     `json:"turns_blocked"`
	Conversation int     `json:"conversations"`
	MedianFirst  int     `json:"median_first_token_ms"`
	ErrorRate    float64 `json:"error_rate_percent"`
	TotalTokens  int64   `json:"total_tokens"`
	Cost         float64 `json:"estimated_cost"`
}

type relayView struct {
	Running    bool             `json:"running"`
	Detail     string           `json:"detail"`
	Addr       string           `json:"addr,omitempty"`
	Version    string           `json:"version,omitempty"`
	Next       string           `json:"next,omitempty"`
	Workspaces []relayWorkspace `json:"workspaces,omitempty"`
	Profiles   []relayProfile   `json:"profiles,omitempty"`
	Rules      []relayRule      `json:"rules,omitempty"`
	Summary    *relaySummary    `json:"summary,omitempty"`
	Problems   []string         `json:"problems,omitempty"`
	Credential string           `json:"credential_storage,omitempty"`
}

// relayState is the slice of codex-relay's own state document this tab needs. Decoding a
// subset keeps the tab working when the relay adds fields, which it does often.
type relayState struct {
	Version  int64 `json:"version"`
	Proposed struct {
		Summary     string `json:"summary"`
		WorkspaceID string `json:"workspace_id"`
	} `json:"proposed"`
	DefaultWorkspaceID string `json:"default_workspace_id"`
	ActiveProfileID    string `json:"active_profile_id"`
	AppVersion         string `json:"app_version"`
	CredentialStorage  struct {
		OK     bool   `json:"ok"`
		Kind   string `json:"kind"`
		Detail string `json:"detail"`
	} `json:"credential_storage"`
	Workspaces []struct {
		ID           string `json:"id"`
		Name         string `json:"name"`
		AccountID    string `json:"account_id"`
		Paused       bool   `json:"paused"`
		CredentialOK bool   `json:"credential_ok"`
		Protected    bool   `json:"protected"`
		Windows      []struct {
			Label     string  `json:"label"`
			Remaining float64 `json:"remaining_percent"`
			ResetsAt  string  `json:"resets_at"`
			Evidence  string  `json:"evidence"`
			Reserve   float64 `json:"reserve_marker_percent"`
		} `json:"windows"`
	} `json:"workspaces"`
	Profiles []struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Command  string `json:"command"`
		Active   bool   `json:"active"`
		Sentence string `json:"sentence"`
	} `json:"profiles"`
	Rules []struct {
		Kind     string `json:"kind"`
		Enabled  bool   `json:"enabled"`
		Sentence string `json:"sentence"`
	} `json:"rules"`
	Summary  relaySummary `json:"summary"`
	Problems []struct {
		Kind    string `json:"kind"`
		Message string `json:"message"`
		Detail  string `json:"detail"`
	} `json:"problems"`
}

// codexRelay answers the tab. A relay that is not running is a normal state to report, not
// an error to fail on: the tab says so and shows nothing else, rather than rendering an
// empty pool that looks like a pool with no accounts.
func (s *Server) codexRelay(w http.ResponseWriter, r *http.Request) {
	base, token, err := relayDial(r.Context())
	if err != nil {
		writeJSON(w, 200, relayView{Detail: err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	var state relayState
	if err := relayGet(ctx, base, "/api/state", token, &state); err != nil {
		writeJSON(w, 200, relayView{Detail: "codex-relay answered, but its state could not be read: " + err.Error(), Addr: base})
		return
	}

	out := relayView{
		Running: true, Addr: base, Version: state.AppVersion, Next: state.Proposed.Summary,
		Detail:  fmt.Sprintf("%d account(s) connected", len(state.Workspaces)),
		Summary: &state.Summary,
	}
	if cs := state.CredentialStorage; cs.Kind != "" {
		out.Credential = cs.Kind
		if !cs.OK {
			out.Credential += " (" + cs.Detail + ")"
		}
	}
	for _, ws := range state.Workspaces {
		v := relayWorkspace{
			ID: ws.ID, Name: ws.Name, AccountID: ws.AccountID, Paused: ws.Paused,
			CredentialOK: ws.CredentialOK, Protected: ws.Protected,
			IsDefault: ws.ID == state.DefaultWorkspaceID,
			IsNext:    ws.ID == state.Proposed.WorkspaceID,
		}
		for _, win := range ws.Windows {
			v.Windows = append(v.Windows, relayWindow{
				Label: win.Label, Remaining: win.Remaining, ResetsAt: win.ResetsAt,
				Evidence: win.Evidence, Reserve: win.Reserve,
			})
		}
		out.Workspaces = append(out.Workspaces, v)
	}
	for _, p := range state.Profiles {
		out.Profiles = append(out.Profiles, relayProfile{
			Name: p.Name, Command: p.Command,
			Active: p.Active || p.ID == state.ActiveProfileID, Sentence: p.Sentence,
		})
	}
	for _, rule := range state.Rules {
		out.Rules = append(out.Rules, relayRule{Kind: rule.Kind, Enabled: rule.Enabled, Sentence: rule.Sentence})
	}
	for _, p := range state.Problems {
		msg := p.Message
		if p.Detail != "" {
			msg += " " + p.Detail
		}
		out.Problems = append(out.Problems, strings.TrimSpace(msg))
	}
	writeJSON(w, 200, out)
}

// codexRelayActivity is a separate call because history is paged and the pool view does not
// need it. Fetching it with the overview would make the common case pay for the rare one.
func (s *Server) codexRelayActivity(w http.ResponseWriter, r *http.Request) {
	base, token, err := relayDial(r.Context())
	if err != nil {
		writeJSON(w, 200, map[string]any{"running": false, "detail": err.Error()})
		return
	}
	limit := 40
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= 200 {
		limit = v
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	var state relayState
	_ = relayGet(ctx, base, "/api/state", token, &state)
	names := map[string]string{}
	for _, ws := range state.Workspaces {
		names[ws.ID] = ws.Name
	}

	var activity struct {
		Activity []struct {
			At        string `json:"at"`
			Workspace string `json:"workspace_id"`
			Outcome   string `json:"outcome"`
			Reason    string `json:"reason"`
			ErrClass  string `json:"error_class"`
			Summary   string `json:"summary"`
		} `json:"activity"`
	}
	if err := relayGet(ctx, base, fmt.Sprintf("/api/activity?limit=%d", limit), token, &activity); err != nil {
		writeJSON(w, 200, map[string]any{"running": false, "detail": err.Error()})
		return
	}
	turns := []relayTurn{}
	for _, a := range activity.Activity {
		name := names[a.Workspace]
		if name == "" {
			name = relayShort(a.Workspace)
		}
		turns = append(turns, relayTurn{
			At: a.At, Name: name, Outcome: a.Outcome, Reason: a.Reason,
			Error: a.ErrClass, Summary: a.Summary,
		})
	}
	writeJSON(w, 200, map[string]any{"running": true, "turns": turns})
}

// codexRelayActivate switches the routing profile. This is the one write the tab offers, and
// it is the reason the tab is worth having on a phone: deciding which account burns next is
// a thing you want to change when you are away from the machine, not when you are sitting at
// it. Everything else here stays a read.
func (s *Server) codexRelayActivate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Command string `json:"command"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Command) == "" {
		writeJSON(w, 400, map[string]any{"error": "which profile command should be activated?"})
		return
	}
	base, token, err := relayDial(r.Context())
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "detail": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var out struct {
		Decision struct {
			Summary string `json:"summary"`
		} `json:"decision"`
	}
	path := "/api/profiles/" + url.PathEscape(strings.TrimSpace(body.Command)) + "/activate"
	if err := relayPost(ctx, base, path, token, nil, &out); err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "detail": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "summary": out.Decision.Summary})
}

// relayShort keeps an unmatched workspace id readable. The ids are account-scoped and long,
// and an id on a board a human reads is not an identification.
func relayShort(id string) string {
	if i := strings.Index(id, ":"); i > 0 {
		return id[:i]
	}
	return id
}

// relayDial resolves the relay's address and session token, or explains why it cannot.
func relayDial(ctx context.Context) (base, token string, err error) {
	port := relayPort()
	if port == "" {
		return "", "", fmt.Errorf("no codex-relay entry in the health roster, so its port is unknown")
	}
	base = "http://127.0.0.1:" + port
	c, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	token, err = relayToken(c, base)
	if err != nil {
		return "", "", fmt.Errorf("codex-relay is not answering on %s", base)
	}
	return base, token, nil
}

func relayPort() string {
	decls, err := health.Declarations(health.ConfigPath())
	if err != nil {
		return ""
	}
	for _, d := range decls {
		if d.Name != relayRosterName {
			continue
		}
		if p, ok := d.With["port"].(float64); ok && p > 0 {
			return fmt.Sprintf("%d", int(p))
		}
	}
	return ""
}

func relayToken(ctx context.Context, base string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", base+"/", nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	m := bootToken.FindSubmatch(body)
	if m == nil {
		return "", fmt.Errorf("no session token in the codex-relay index page")
	}
	var boot struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(m[1], &boot); err != nil {
		return "", err
	}
	if boot.Token == "" {
		return "", fmt.Errorf("empty codex-relay session token")
	}
	return boot.Token, nil
}

func relayGet(ctx context.Context, base, path, token string, into any) error {
	return relayCall(ctx, "GET", base, path, token, nil, into)
}

func relayPost(ctx context.Context, base, path, token string, body, into any) error {
	return relayCall(ctx, "POST", base, path, token, body, into)
}

func relayCall(ctx context.Context, method, base, path, token string, body, into any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("X-Codex-Pool-Token", token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return fmt.Errorf("%s returned %d: %s", path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if into == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(into)
}
