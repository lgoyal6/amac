package discord

import (
	"net/url"
	"testing"
)

// The notification's id has to reach the link, or nothing that happens after a
// notification can be attributed to it, which is what made the whole
// engagement question unanswerable.
func TestHandoffURLCarriesTheNoticeOutsideTheSignature(t *testing.T) {
	t.Setenv("AMAC_BOARD_URL", "http://100.64.0.1:7788")
	t.Setenv("AMAC_HANDOFF_SECRET", "s3cret")

	q := func(raw string) url.Values {
		t.Helper()
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("not a URL: %v", err)
		}
		return u.Query()
	}
	with, without := q(HandoffURL("am-claude-9", "abc123")), q(HandoffURL("am-claude-9", ""))

	if got := with.Get("n"); got != "abc123" {
		t.Errorf("n = %q, want the notification id", got)
	}
	// Parsed rather than substring-matched: "n=" is inside "session=", and a
	// check that cannot tell those apart passes on anything.
	if _, present := without["n"]; present {
		t.Errorf("a link with no notification carries the tag anyway: %v", without)
	}

	// The tag must not change the signature. It grants nothing and opens
	// nothing; signing it would imply it were a capability rather than a label,
	// and would make an edited tag a 403 instead of one mislabelled row.
	if with.Get("sig") == "" {
		t.Fatal("no signature was produced")
	}
	if with.Get("sig") != without.Get("sig") {
		t.Errorf("the notice tag changed the signature")
	}
	if with.Get("session") != "am-claude-9" || with.Get("expires") == "" {
		t.Errorf("the link lost what the signature actually covers: %v", with)
	}
}
