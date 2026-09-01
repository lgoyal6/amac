package account

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// machine builds a fake home directory with the accounts a test names.
func machine(t *testing.T, dirs ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func writeCodexAuth(t *testing.T, dir, email, plan string) {
	t.Helper()
	claims, _ := json.Marshal(map[string]any{
		"email":                       email,
		"https://api.openai.com/auth": map[string]any{"chatgpt_plan_type": plan},
	})
	token := "h." + base64.RawURLEncoding.EncodeToString(claims) + ".sig"
	body, _ := json.Marshal(map[string]any{"tokens": map[string]any{"id_token": token}})
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeClaudeConfig(t *testing.T, path, email, uuid string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"oauthAccount": map[string]any{
		"accountUuid": uuid, "emailAddress": email, "seatTier": "team_tier_1",
	}})
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

// The roster is the point of the package: an account that is not installed
// here still has to appear, because a list of only what was found cannot say
// whether anything is missing.
func TestAbsentAccountIsReportedNotDropped(t *testing.T) {
	root := machine(t, ".codex")

	all := in(root)
	if len(all) != len(roster) {
		t.Fatalf("got %d accounts, want the whole roster (%d)", len(all), len(roster))
	}
	for _, a := range all {
		if (a.ID == "codex") != a.Local {
			t.Errorf("%s: local=%v, but only .codex exists on this machine", a.ID, a.Local)
		}
	}
}

func TestIdentityComesFromTheAccountsOwnFiles(t *testing.T) {
	root := machine(t, ".codex", ".codex-ish", ".claude")
	writeCodexAuth(t, filepath.Join(root, ".codex"), "me@ucsd.edu", "prolite")
	writeCodexAuth(t, filepath.Join(root, ".codex-ish"), "her@gmail.com", "plus")
	// The default Claude home keeps its config beside the directory.
	writeClaudeConfig(t, filepath.Join(root, ".claude.json"), "me@work.example", "uuid-gmi")

	got := map[string]Account{}
	for _, a := range in(root) {
		got[a.ID] = a
	}
	if e := got["codex"]; e.Email != "me@ucsd.edu" || e.Plan != "prolite" {
		t.Errorf("codex: got %q/%q", e.Email, e.Plan)
	}
	if e := got["codex-ish"]; e.Email != "her@gmail.com" || e.Plan != "plus" {
		t.Errorf("codex-ish: got %q/%q", e.Email, e.Plan)
	}
	if e := got["claude-gmi"]; e.Email != "me@work.example" || e.UUID != "uuid-gmi" {
		t.Errorf("claude-gmi: got %q/%q", e.Email, e.UUID)
	}
}

// A CLAUDE_CONFIG_DIR home keeps its config inside itself rather than beside
// it, so both layouts have to resolve or the second account reads as logged
// out.
func TestClaudeConfigInsideItsOwnHome(t *testing.T) {
	root := machine(t, ".claude-lgoyal")
	writeClaudeConfig(t, filepath.Join(root, ".claude-lgoyal", ".claude.json"), "me@personal.example", "uuid-lg")

	for _, a := range in(root) {
		if a.ID != "claude-lgoyal" {
			continue
		}
		if a.Email != "me@personal.example" || !a.Local {
			t.Fatalf("got email=%q local=%v", a.Email, a.Local)
		}
		return
	}
	t.Fatal("claude-lgoyal missing from the roster")
}

func TestHomeMapsToItsAccount(t *testing.T) {
	root := machine(t, ".codex", ".codex-ish")
	if id := forHome(root, filepath.Join(root, ".codex-ish")); id != "codex-ish" {
		t.Errorf("got %q, want codex-ish", id)
	}
	// A trailing separator is the same directory, and shells produce them.
	if id := forHome(root, filepath.Join(root, ".codex")+"/"); id != "codex" {
		t.Errorf("got %q, want codex", id)
	}
}

// An unknown home must stay untagged. Guessing an id here would bill one
// account for another's tokens, which is the exact overstatement this whole
// area exists to prevent.
func TestUnknownHomeIsUntagged(t *testing.T) {
	root := machine(t, ".codex")
	if id := forHome(root, filepath.Join(root, ".codex-someone-else")); id != "" {
		t.Errorf("got %q, want no tag", id)
	}
	if id := forHome(root, ""); id != "" {
		t.Errorf("empty home got %q, want no tag", id)
	}
}

func TestTranscriptOwnerReadsTheHead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	line := `{"type":"summary","ownerAccountUuid":"uuid-gmi","ownerOrganizationUuid":"org"}` + "\n"
	if err := os.WriteFile(path, []byte(`{"type":"noise"}`+"\n"+line), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := transcriptOwner(path); got != "uuid-gmi" {
		t.Errorf("got %q, want uuid-gmi", got)
	}
	if got := transcriptOwner(filepath.Join(t.TempDir(), "gone.jsonl")); got != "" {
		t.Errorf("missing transcript got %q, want empty", got)
	}
}
