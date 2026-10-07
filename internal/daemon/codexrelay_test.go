package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

// The burn rate is relay's own projection, attached to the window it was made
// for. Matching on the label instead of the window length would put a 5-hour
// burn on a weekly window the day relay renames one.
func TestRelayWindowsCarryTheirOwnBurn(t *testing.T) {
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.Write([]byte(`<script type="application/json" id="codexrelay-boot">{"token":"tok"}</script>`))
		case "/api/state":
			w.Write([]byte(`{
			  "workspaces":[{"id":"a","name":"a@x","credential_ok":true,"windows":[
			    {"minutes":300,"label":"5-hour","remaining_percent":0},
			    {"minutes":10080,"label":"weekly","remaining_percent":84},
			    {"minutes":43200,"label":"30-day","remaining_percent":100}]}],
			  "projections":[
			    {"workspace_id":"a","window_minutes":10080,"burn_percent_per_hour":4.3,"hours_to_empty":19.5,"will_run_out_before_reset":true},
			    {"workspace_id":"a","window_minutes":300,"burn_percent_per_hour":26.9,"hours_to_empty":0,"will_run_out_before_reset":true},
			    {"workspace_id":"other","window_minutes":43200,"burn_percent_per_hour":9}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer relay.Close()
	u, _ := url.Parse(relay.URL)
	roster := filepath.Join(t.TempDir(), "health.json")
	os.WriteFile(roster, []byte(`{"automations":[{"name":"codex-relay","probe":"service","with":{"port":`+u.Port()+`}}]}`), 0o600)
	t.Setenv("AMAC_HEALTH_CONFIG", roster)

	rec := httptest.NewRecorder()
	(&Server{}).codexRelay(rec, httptest.NewRequest("GET", "/api/codex-relay", nil))
	var v relayView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil || !v.Running || len(v.Workspaces) != 1 {
		t.Fatalf("relay view not read: %v %s", err, rec.Body)
	}
	wins := v.Workspaces[0].Windows
	if wins[0].Burn == nil || *wins[0].Burn != 26.9 {
		t.Errorf("5-hour window should carry the 5-hour burn, got %+v", wins[0])
	}
	if wins[1].Burn == nil || *wins[1].Burn != 4.3 || *wins[1].HoursToEmpty != 19.5 || !wins[1].RunsOut {
		t.Errorf("weekly window should carry the weekly projection, got %+v", wins[1])
	}
	if wins[2].Burn != nil {
		t.Errorf("another account's projection leaked onto this window: %v", *wins[2].Burn)
	}
}
