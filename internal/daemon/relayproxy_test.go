package daemon

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

// relayStub stands in for codex-relay and records what actually arrived, which is the
// only way to tell a proxy that rewrote a header from one that merely claimed to.
func relayStub(t *testing.T, seen *http.Request) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = *r.Clone(r.Context())
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("dashboard"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// rosterPointingAt writes a health roster naming the stub's port, because that roster is
// where the proxy looks to find codex-relay.
func rosterPointingAt(t *testing.T, srv *httptest.Server) {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "health.json")
	body := fmt.Sprintf(
		`{"automations":[{"name":%q,"probe":"service","with":{"port":%s}}]}`,
		relayRosterName, u.Port())
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AMAC_HEALTH_CONFIG", path)
}

func proxyServer(t *testing.T) *Server {
	t.Helper()
	return &Server{token: "test-token"}
}

// TestRelayProxyAddressesRelayAsLoopback is the whole reason this proxy exists.
//
// codex-relay refuses any Host that is not a loopback literal, which is what stops DNS
// rebinding. A request from a phone arrives with a tailnet Host, so forwarding it
// unchanged would be refused by the service we are trying to reach.
func TestRelayProxyAddressesRelayAsLoopback(t *testing.T) {
	var seen http.Request
	srv := relayStub(t, &seen)
	rosterPointingAt(t, srv)

	r := httptest.NewRequest("GET", "http://100.127.168.102:7789/", nil)
	r.Host = "100.127.168.102:7789"
	r.Header.Set("Origin", "http://100.127.168.102:7788")
	r.Header.Set("X-Amac-Token", "test-token")
	w := httptest.NewRecorder()

	proxyServer(t).RelayProxy().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body)
	}
	h, _, err := net.SplitHostPort(seen.Host)
	if err != nil || (h != "127.0.0.1" && h != "::1" && h != "localhost") {
		t.Errorf("codex-relay was addressed as %q, which its Host check refuses", seen.Host)
	}
	if origin := seen.Header.Get("Origin"); origin != "http://"+seen.Host {
		t.Errorf("Origin %q disagrees with Host %q, which is the shape of the attack the guard looks for",
			origin, seen.Host)
	}
}

// TestRelayProxyRefusesCodexTraffic guards the path that spends money.
//
// codex-relay keeps /backend-api loopback-only and refuses it from anything carrying a
// web page's Origin, so a page in the browser cannot run turns against pooled accounts.
// Carrying it here would hand that path to everything on the tailnet.
func TestRelayProxyRefusesCodexTraffic(t *testing.T) {
	var seen http.Request
	srv := relayStub(t, &seen)
	rosterPointingAt(t, srv)

	for _, path := range []string{"/backend-api/codex/responses", "/backend-api/"} {
		r := httptest.NewRequest("POST", "http://100.127.168.102:7789"+path, nil)
		r.Header.Set("X-Amac-Token", "test-token")
		w := httptest.NewRecorder()

		proxyServer(t).RelayProxy().ServeHTTP(w, r)

		if w.Code != http.StatusForbidden {
			t.Errorf("%s: want 403, got %d", path, w.Code)
		}
		if seen.URL != nil {
			t.Errorf("%s reached codex-relay; it must never be forwarded", path)
		}
	}
}

// TestRelayProxyNeedsTheAmacToken checks the outer gate. The tailnet is not an
// authorization boundary on its own, and every control behind this proxy changes which
// ChatGPT account pays.
func TestRelayProxyNeedsTheAmacToken(t *testing.T) {
	var seen http.Request
	srv := relayStub(t, &seen)
	rosterPointingAt(t, srv)

	r := httptest.NewRequest("GET", "http://100.127.168.102:7789/", nil)
	w := httptest.NewRecorder()

	proxyServer(t).RelayProxy().ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("want 401 without a token, got %d", w.Code)
	}
	if seen.URL != nil {
		t.Error("an unauthenticated request reached codex-relay")
	}
}

// TestRelayProxySaysWhenRelayIsMissing keeps a common setup mistake legible. Without the
// roster entry the proxy has no port to dial, and a bare 502 would read as codex-relay
// being broken rather than never having been declared.
func TestRelayProxySaysWhenRelayIsMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "health.json")
	if err := os.WriteFile(path, []byte(`{"automations":[{"name":"something-else","probe":"service"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AMAC_HEALTH_CONFIG", path)

	r := httptest.NewRequest("GET", "http://100.127.168.102:7789/", nil)
	r.Header.Set("X-Amac-Token", "test-token")
	w := httptest.NewRecorder()

	proxyServer(t).RelayProxy().ServeHTTP(w, r)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("want 503 when codex-relay is not in the roster, got %d", w.Code)
	}
}
