package health

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Delivery reuses the Discord bot agentmon already registered: same token in
// the login keychain, same cached DM channel. Standing up a second bot would
// mean a second token to rotate and a second DM thread to check, for a channel
// that is already proven to reach his phone.
const discordAPI = "https://discord.com/api/v10"

func discordToken() string {
	if t := os.Getenv("AGENTMON_DISCORD_TOKEN"); t != "" {
		return t
	}
	out, err := exec.Command("security", "find-generic-password",
		"-w", "-s", "AGENTMON_DISCORD_TOKEN", "-a", os.Getenv("USER")).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func discordChannel() string {
	b, err := os.ReadFile(os.Getenv("HOME") + "/.agentmon/state/.discord_dm")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// Send posts one message to the DM channel.
func Send(ctx context.Context, content string) error {
	tok, ch := discordToken(), discordChannel()
	if tok == "" {
		return fmt.Errorf("no Discord token (keychain AGENTMON_DISCORD_TOKEN)")
	}
	if ch == "" {
		return fmt.Errorf("no cached DM channel (~/.agentmon/state/.discord_dm)")
	}
	// Discord hard-caps a message at 2000 characters and rejects the whole
	// request past it, so a long digest must be trimmed rather than dropped.
	const limit = 1990
	if len(content) > limit {
		cut := content[:limit]
		// Cut back to the last line break so the message never ends mid-word.
		if i := strings.LastIndexByte(cut, '\n'); i > limit/2 {
			cut = cut[:i]
		}
		content = cut + "\n…"
	}
	body, _ := json.Marshal(map[string]string{"content": content})
	req, err := http.NewRequestWithContext(ctx, "POST",
		discordAPI+"/channels/"+ch+"/messages", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bot "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("discord: http %d", resp.StatusCode)
	}
	return nil
}

// Digest renders the full roster for a phone screen.
//
// Discord's mobile column is roughly forty characters, so the layout spends
// none of it on indentation and gives detail only where detail is wanted: a
// healthy automation is one line, and everything below the fold is reserved
// for the ones that need him. Reports arrive worst-first from Run.
func Digest(reports []Report) string {
	var bad, good []Report
	for _, r := range reports {
		if r.State == OK {
			good = append(good, r)
		} else {
			bad = append(bad, r)
		}
	}

	var b strings.Builder
	if len(bad) == 0 {
		fmt.Fprintf(&b, "✅ **Automations** · all %d delivering\n", len(reports))
	} else {
		fmt.Fprintf(&b, "⚠️ **Automations** · %d of %d need attention\n", len(bad), len(reports))
	}

	for _, r := range bad {
		fmt.Fprintf(&b, "\n%s **%s** · %s\n%s\n", r.State.Icon(), r.Name, r.State, tidy(r.Detail))
		if !r.Last.IsZero() {
			fmt.Fprintf(&b, "· last delivery %s\n", r.Last.Local().Format("Mon 15:04"))
		}
		for _, n := range r.Notes {
			fmt.Fprintf(&b, "· %s\n", tidy(n))
		}
		if r.Err != "" {
			fmt.Fprintf(&b, "· %s\n", tidy(r.Err))
		}
	}

	if len(good) > 0 {
		if len(bad) > 0 {
			b.WriteString("\n**Healthy**\n")
		} else {
			b.WriteString("\n")
		}
		for _, r := range good {
			fmt.Fprintf(&b, "🟢 %s · %s\n", r.Name, tidy(r.Detail))
		}
	}

	fmt.Fprintf(&b, "\n_checked %s_", time.Now().Local().Format("Mon 2 Jan 15:04"))
	return b.String()
}

// bareURL matches an unwrapped http(s) URL.
var bareURL = regexp.MustCompile(`https?://[^\s<>]+`)

// tidy wraps URLs in angle brackets, which is what stops Discord expanding a
// link into a preview card. On a phone one card pushes the rest of the roster
// off the screen, and these links are for tapping, not for reading.
func tidy(s string) string {
	return bareURL.ReplaceAllString(strings.TrimSpace(s), "<$0>")
}

// Alert renders only what changed, so a healthy day is silent. A monitor that
// messages every cycle gets muted, and a muted monitor is worse than none.
//
// "Changed" means the state differs, not merely that it is bad. A pipeline
// moving from Late to Failing has told him something new: it stopped being
// quiet and started erroring, and comparing against OK alone would swallow
// that.
func Alert(reports []Report, prev map[string]State) (string, bool) {
	var broke, fixed []Report
	for _, r := range reports {
		was, seen := prev[r.Name]
		switch {
		case r.State != OK && (!seen || was != r.State):
			broke = append(broke, r)
		case r.State == OK && seen && was != OK:
			fixed = append(fixed, r)
		}
	}
	if len(broke) == 0 && len(fixed) == 0 {
		return "", false
	}
	var b strings.Builder
	for _, r := range broke {
		fmt.Fprintf(&b, "%s **%s** · %s\n%s\n", r.State.Icon(), r.Name, r.State, tidy(r.Detail))
		for _, n := range r.Notes {
			fmt.Fprintf(&b, "· %s\n", tidy(n))
		}
	}
	for _, r := range fixed {
		fmt.Fprintf(&b, "🟢 **%s recovered** · %s\n", r.Name, tidy(r.Detail))
	}
	return strings.TrimRight(b.String(), "\n"), true
}
