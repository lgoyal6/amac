package discord

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Delivery had no test, which matters because this is the last hop: everything
// amac decides about attention ends as one POST, and a message that Discord
// rejects is a notification nobody gets.

func withServer(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	old := api
	api = srv.URL
	t.Cleanup(func() { api = old })
	t.Setenv("AMAC_DISCORD_TOKEN", "bot-token")
	t.Setenv("AMAC_DISCORD_CHANNEL", "chan-1")
}

func TestSendPostsToTheChannelAsABot(t *testing.T) {
	var gotPath, gotAuth string
	var body map[string]any
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(200)
	})

	if err := Send(context.Background(), "an automation went red"); err != nil {
		t.Fatalf("send: %v", err)
	}
	if gotPath != "/channels/chan-1/messages" {
		t.Errorf("posted to %q", gotPath)
	}
	// Bot rather than Bearer. A user token would work in testing and be the
	// wrong credential in production.
	if gotAuth != "Bot bot-token" {
		t.Errorf("authorization = %q, want a bot token", gotAuth)
	}
	if body["content"] != "an automation went red" {
		t.Errorf("content = %v", body["content"])
	}
	if _, hasButton := body["components"]; hasButton {
		t.Error("a plain send should carry no components")
	}
}

// Discord rejects the whole request past 2000 characters, so a long message has
// to be trimmed rather than dropped. Dropping it is the failure that matters:
// the alert you never see is the one about the thing that broke.
func TestAnOverlongMessageIsTrimmedNotRejected(t *testing.T) {
	var body map[string]any
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(200)
	})

	long := strings.Repeat("x", 5000)
	if err := Send(context.Background(), long); err != nil {
		t.Fatalf("an overlong message must still send: %v", err)
	}
	content, _ := body["content"].(string)
	if len([]rune(content)) >= 2000 {
		t.Errorf("content is %d runes, which Discord would reject", len([]rune(content)))
	}
	if !strings.HasSuffix(content, "…") {
		t.Error("a trimmed message should show that it was trimmed")
	}
}

// The handoff button is why this is not a webhook: Discord cannot invoke a
// private POST endpoint itself, so the link opens the already-authenticated
// board, whose first action is the local handoff.
func TestAHandoffCarriesALinkButton(t *testing.T) {
	var body map[string]any
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(200)
	})

	const url = "http://100.64.0.1:7788/handoff?session=x&sig=y"
	if err := SendHandoff(context.Background(), "a session needs you", url); err != nil {
		t.Fatal(err)
	}
	rows, _ := body["components"].([]any)
	if len(rows) != 1 {
		t.Fatalf("expected one action row: %v", body["components"])
	}
	row, _ := rows[0].(map[string]any)
	inner, _ := row["components"].([]any)
	button, _ := inner[0].(map[string]any)
	// Style 5 is a link button, the only kind that opens a URL rather than
	// posting an interaction back to a server amac does not run.
	if button["style"] != float64(5) || button["url"] != url {
		t.Errorf("button = %v, want a link to the handoff", button)
	}
}

// A refusal from Discord must be an error. Swallowing it means amac believes it
// notified you, which is exactly the failure the health monitor was built to
// stop happening elsewhere.
func TestAnHTTPFailureIsReported(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	err := Send(context.Background(), "hello")
	if err == nil {
		t.Fatal("a 403 was reported as a successful send")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("the error should name the status: %v", err)
	}
}

// Missing credentials are a clear error naming what to set, not a silent
// no-op. A no-op here is an alerting system that quietly does nothing.
func TestMissingCredentialsSayWhatToSet(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	t.Setenv("AMAC_DISCORD_TOKEN", "")
	t.Setenv("HOME", t.TempDir()) // so the keychain and file fallbacks find nothing

	err := Send(context.Background(), "hello")
	if err == nil {
		t.Skip("a token was found elsewhere on this machine")
	}
	if !strings.Contains(err.Error(), "AMAC_DISCORD_TOKEN") {
		t.Errorf("error %q does not say what to set", err)
	}
}
