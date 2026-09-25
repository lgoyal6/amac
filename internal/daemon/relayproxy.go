package daemon

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
)

// RelayProxyPort is where the codex-relay dashboard is served to the tailnet.
//
// It gets a listener of its own rather than a path under the main dashboard because
// the compiled page asks for /api and /assets at the root, and amac answers /api
// itself. Mounting it under a prefix would mean rewriting the built JavaScript, and a
// rewritten bundle is a copy that drifts from the thing it copies. A second port keeps
// this a proxy: every path maps one to one, and what a phone loads is the dashboard,
// not a reimplementation of it.
const RelayProxyPort = 7789

// relayProxyBlocked is the one path this proxy will not carry.
//
// /backend-api is how Codex itself spends quota. codex-relay keeps it loopback-only on
// purpose, and refuses it from anything carrying a web page's Origin, because a page in
// the browser must never be able to run turns against pooled accounts. Forwarding it
// here would hand that path to the whole tailnet and undo the guard. The dashboard
// never asks for it; only Codex does, and Codex talks to codex-relay directly.
const relayProxyBlocked = "/backend-api/"

// RelayProxy serves the real codex-relay dashboard to the tailnet.
//
// codex-relay binds loopback and refuses to bind anything else, which is correct for a
// service holding ChatGPT credentials and is also exactly why a phone cannot open it.
// This closes that gap without weakening the bind: the request arrives over the tailnet
// at amac, which already requires a token, and is handed to codex-relay across the
// loopback interface it trusts.
func (s *Server) RelayProxy() http.Handler {
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			// The wrapper below resolved the port and wrote it onto the inbound
			// URL, having already refused the request if codex-relay is not here.
			host := r.In.URL.Host
			r.Out.URL.Scheme = "http"
			r.Out.URL.Host = host

			// codex-relay refuses any Host that is not a loopback literal on its own
			// port. That check is what stops DNS rebinding, and the tailnet Host the
			// browser sent would trip it. Origin follows Host for the same reason:
			// the two disagreeing is the shape of the attack those guards look for.
			r.Out.Host = host
			if r.Out.Header.Get("Origin") != "" {
				r.Out.Header.Set("Origin", "http://"+host)
			}

			// The dashboard's token is minted into the page it serves, so the
			// browser already holds the right credential. amac's own token has no
			// meaning downstream and should not travel with the request.
			r.Out.Header.Del("X-Amac-Token")
			r.Out.Header.Del("Cookie")
		},

		// The dashboard streams /api/events. A buffered copy would hold those frames
		// until the buffer filled, which for an event stream is indistinguishable
		// from the dashboard having frozen.
		FlushInterval: -1,

		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w,
				"codex-relay is not answering on this machine. Check that the service is running.",
				http.StatusBadGateway)
		},
	}

	return http.HandlerFunc(s.auth(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, relayProxyBlocked) {
			http.Error(w,
				"this proxy carries the dashboard, not Codex traffic",
				http.StatusForbidden)
			return
		}

		port := relayPort()
		if port == "" {
			http.Error(w,
				"no codex-relay entry in the health roster, so its port is unknown",
				http.StatusServiceUnavailable)
			return
		}

		out := r.Clone(r.Context())
		out.URL.Scheme = "http"
		out.URL.Host = "127.0.0.1:" + port
		proxy.ServeHTTP(w, out)
	}))
}

// RelayProxyURL is the address to open, given the host amac is bound to.
func RelayProxyURL(host string) string {
	return fmt.Sprintf("http://%s:%d/", host, RelayProxyPort)
}

// hostOnly drops any port from a Host header, including the bracketed IPv6 form.
func hostOnly(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}
