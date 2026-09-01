// Package account names the agent accounts this machine can spend money on.
//
// Two CLIs is not the same as two accounts, and the difference is money. Codex
// runs here under two logins with separate homes and separate plans, and until
// this package existed the second one was invisible everywhere: its sessions
// reached no hook, its tokens reached no cost report, and nothing on any screen
// said a whole account was missing. A dashboard that silently covers half the
// accounts is worse than one that covers none, because the number it prints
// looks complete.
//
// Identity is read from each account's own files rather than written down here.
// An email in a Go constant is a fact that goes stale the first time somebody
// logs in as somebody else, and it would be a fact this repo has no business
// carrying: the roster is which homes to look in, and the accounts in them
// answer for themselves.
package account

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Account is one login: where it keeps its state, and who it turned out to be.
type Account struct {
	ID    string `json:"id"`    // stable key, safe in URLs and JSON
	Agent string `json:"agent"` // codex | claude
	Label string `json:"label"` // what a card or a table row says
	Home  string `json:"home"`
	Email string `json:"email,omitempty"`
	Plan  string `json:"plan,omitempty"`
	UUID  string `json:"uuid,omitempty"` // Claude's account id, as transcripts record it
	// Local is whether this account's home exists here. An account that is
	// known but absent is reported as absent rather than dropped: "lgoyal has
	// no sessions on this Mac" and "amac forgot lgoyal exists" look identical
	// on a screen that lists only what it found.
	Local bool `json:"local"`
}

// roster is the fixed set of logins amac knows to look for, and the launcher
// each one is reached by. The directory is the identity: `codex` and
// `codex-ish` are the same binary pointed at different CODEX_HOMEs, and Claude
// works the same way through CLAUDE_CONFIG_DIR.
var roster = []Account{
	{ID: "codex", Agent: "codex", Label: "codex", Home: ".codex"},
	{ID: "codex-ish", Agent: "codex", Label: "codex-ish", Home: ".codex-ish"},
	{ID: "claude-gmi", Agent: "claude", Label: "gmi", Home: ".claude"},
	// Not on this Mac today. Named anyway, so the money page can say the
	// account exists and has nothing here, and so it starts reporting by
	// itself the day the home appears.
	{ID: "claude-lgoyal", Agent: "claude", Label: "lgoyal", Home: ".claude-lgoyal"},
}

func home() string {
	h, _ := os.UserHomeDir()
	return h
}

// All returns the roster with each account's own identity filled in.
func All() []Account { return in(home()) }

// in is All against an explicit home directory, so tests can build a machine.
func in(root string) []Account {
	out := make([]Account, 0, len(roster))
	for _, a := range roster {
		a.Home = filepath.Join(root, a.Home)
		if st, err := os.Stat(a.Home); err == nil && st.IsDir() {
			a.Local = true
			switch a.Agent {
			case "codex":
				a.Email, a.Plan = codexIdentity(a.Home)
			case "claude":
				a.Email, a.Plan, a.UUID = claudeIdentity(root, a.Home)
			}
		}
		out = append(out, a)
	}
	return out
}

// ForHome maps a CODEX_HOME or CLAUDE_CONFIG_DIR onto an account id, returning
// "" for a home this machine has never heard of. An unknown home is left
// untagged on purpose: inventing an id for it would attribute its spend to an
// account that did not incur it.
func ForHome(dir string) string { return forHome(home(), dir) }

func forHome(root, dir string) string {
	if dir == "" {
		return ""
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	dir = filepath.Clean(dir)
	for _, a := range in(root) {
		if filepath.Clean(a.Home) == dir {
			return a.ID
		}
	}
	return ""
}

// Codex resolves the account a Codex process belongs to.
//
// CODEX_HOME is set by the shell launcher and inherited by everything Codex
// spawns, which includes the notify program. So the hook that tells amac a turn
// finished can also tell it whose turn it was, without amac inspecting a
// process it did not start.
func Codex(env string) string {
	if env == "" {
		env = filepath.Join(home(), ".codex")
	}
	return ForHome(env)
}

// Claude resolves the account a Claude Code session belongs to, preferring the
// account id its transcript records over the environment.
//
// The transcript is the stronger source: CLAUDE_CONFIG_DIR is absent for the
// default home and says only which config was loaded, whereas ownerAccountUuid
// is written by the CLI on the turn itself and survives a login switch inside
// one home.
func Claude(env, transcript string) string {
	if id := byUUID(transcriptOwner(transcript)); id != "" {
		return id
	}
	if env == "" {
		env = filepath.Join(home(), ".claude")
	}
	return ForHome(env)
}

func byUUID(uuid string) string {
	if uuid == "" {
		return ""
	}
	for _, a := range All() {
		if a.UUID != "" && a.UUID == uuid {
			return a.ID
		}
	}
	return ""
}

// ---------------------------------------------------------------- identity --

// codexIdentity reads the login out of the account's own auth file. The email
// and plan live in the id token's claims, which is signed state the CLI put
// there; nothing here refreshes or validates it, because the question is only
// "who is logged in", not "may this token be used".
func codexIdentity(dir string) (email, plan string) {
	b, err := os.ReadFile(filepath.Join(dir, "auth.json"))
	if err != nil {
		return "", ""
	}
	var auth struct {
		Tokens struct {
			IDToken string `json:"id_token"`
		} `json:"tokens"`
	}
	if json.Unmarshal(b, &auth) != nil {
		return "", ""
	}
	parts := strings.Split(auth.Tokens.IDToken, ".")
	if len(parts) < 2 {
		return "", ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", ""
	}
	var claims struct {
		Email string `json:"email"`
		Auth  struct {
			Plan string `json:"chatgpt_plan_type"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(raw, &claims) != nil {
		return "", ""
	}
	return claims.Email, claims.Auth.Plan
}

// claudeIdentity reads the logged-in account out of Claude Code's config.
//
// The default home keeps its config beside the directory rather than inside it
// (~/.claude.json next to ~/.claude), while a CLAUDE_CONFIG_DIR home keeps it
// within. Both are checked, inner first, so neither layout needs a special case
// anywhere else.
func claudeIdentity(root, dir string) (email, plan, uuid string) {
	for _, p := range []string{
		filepath.Join(dir, ".claude.json"),
		filepath.Join(root, filepath.Base(dir)+".json"),
	} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var cfg struct {
			OAuth struct {
				UUID     string `json:"accountUuid"`
				Email    string `json:"emailAddress"`
				SeatTier string `json:"seatTier"`
			} `json:"oauthAccount"`
		}
		if json.Unmarshal(b, &cfg) != nil {
			continue
		}
		if cfg.OAuth.Email != "" || cfg.OAuth.UUID != "" {
			return cfg.OAuth.Email, cfg.OAuth.SeatTier, cfg.OAuth.UUID
		}
	}
	return "", "", ""
}

// transcriptOwner reads the account id Claude Code stamps on its own records.
//
// Only the head is read. The field appears on session-level records written at
// the start, and a transcript that has run for hours is tens of megabytes; the
// answer does not change partway down a file, so reading further would cost
// more every hour for the same string.
const transcriptHead = 64 << 10

func transcriptOwner(path string) string {
	if path == "" {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	buf := make([]byte, transcriptHead)
	n, _ := f.Read(buf)
	for _, line := range strings.Split(string(buf[:n]), "\n") {
		if !strings.Contains(line, "ownerAccountUuid") {
			continue
		}
		var rec struct {
			Owner string `json:"ownerAccountUuid"`
		}
		if json.Unmarshal([]byte(line), &rec) == nil && rec.Owner != "" {
			return rec.Owner
		}
	}
	return ""
}

// Labels maps account id to the short name a card or a table row shows. One
// roster read serves a whole page: the board lists twenty sessions and re-reads
// them on every event, and resolving each one separately would stat four
// directories and parse two files per card.
func Labels() map[string]string {
	all := All()
	out := make(map[string]string, len(all))
	for _, a := range all {
		out[a.ID] = a.Label
	}
	return out
}

// Default is the account an agent runs as when nothing says otherwise, which is
// what the daemon's own ACP subprocesses inherit.
func Default(agent string) string {
	switch agent {
	case "codex":
		return Codex(os.Getenv("CODEX_HOME"))
	case "claude":
		return Claude(os.Getenv("CLAUDE_CONFIG_DIR"), "")
	}
	return ""
}
