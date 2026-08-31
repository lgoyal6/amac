// Package discord is the one place that knows how to reach a phone.
//
// Configured rather than compiled: a token from the environment or the login
// keychain, a channel id from a file. It looks in amac's own places first and
// then in agentmon's, because this machine's bot was registered by the
// predecessor and rotating a working credential to satisfy a rename is not an
// improvement. Anyone else sets AMAC_DISCORD_TOKEN and writes a channel id, and
// the fallbacks never fire.
//
// Delivery is optional throughout. A missing token is reported by whoever tried
// to send, never swallowed, but it does not stop amac from running: the board,
// the queue and the sweep all work without a phone attached, and refusing to
// start without one would make a notifier a dependency of a control plane.
package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"
)

const api = "https://discord.com/api/v10"

var client = &http.Client{Timeout: 20 * time.Second}

// tokenNames are tried in order. amac's own first, the predecessor's second, so
// an existing setup keeps working without being asked to re-register a bot.
var tokenNames = []string{"AMAC_DISCORD_TOKEN", "AGENTMON_DISCORD_TOKEN"}

func token() string {
	for _, name := range tokenNames {
		if t := os.Getenv(name); t != "" {
			return t
		}
	}
	for _, name := range tokenNames {
		out, err := exec.Command("security", "find-generic-password",
			"-w", "-s", name, "-a", os.Getenv("USER")).Output()
		if err == nil {
			if t := strings.TrimSpace(string(out)); t != "" {
				return t
			}
		}
	}
	return ""
}

func channel() string {
	if c := os.Getenv("AMAC_DISCORD_CHANNEL"); c != "" {
		return c
	}
	home := os.Getenv("HOME")
	for _, path := range []string{
		home + "/.amac/discord_channel",
		home + "/.agentmon/state/.discord_dm",
	} {
		if b, err := os.ReadFile(path); err == nil {
			if c := strings.TrimSpace(string(b)); c != "" {
				return c
			}
		}
	}
	return ""
}

// BoardURL returns a notification-safe link to one session. It deliberately
// does not put the board token in Discord: the phone's installed board already
// has it, while a chat transcript is a much broader place to leave a shell-
// equivalent credential. AMAC_BOARD_URL can override discovery for unusual
// ports or hostnames.
func BoardURL(session string) string {
	base := strings.TrimSpace(os.Getenv("AMAC_BOARD_URL"))
	if base == "" {
		out, err := exec.Command("tailscale", "ip", "-4").Output()
		if err != nil {
			return ""
		}
		ip := strings.Fields(string(out))
		if len(ip) == 0 {
			return ""
		}
		port := strings.TrimSpace(os.Getenv("AMAC_PORT"))
		if port == "" {
			port = "7788"
		}
		base = "http://" + ip[0] + ":" + port + "/"
	}
	u, err := url.Parse(base)
	if err != nil {
		return ""
	}
	q := u.Query()
	if session != "" {
		q.Set("session", session)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// Send posts one message to the DM channel.
func Send(ctx context.Context, content string) error {
	tok, ch := token(), channel()
	if tok == "" {
		return fmt.Errorf("no Discord token: set AMAC_DISCORD_TOKEN, or add it to the login keychain under that name")
	}
	if ch == "" {
		return fmt.Errorf("no Discord channel: set AMAC_DISCORD_CHANNEL, or put the id in ~/.amac/discord_channel")
	}
	// Discord rejects the whole request past 2000 characters, so a long
	// message has to be trimmed rather than dropped.
	const limit = 1990
	if len(content) > limit {
		content = content[:limit] + "\n…"
	}
	body, _ := json.Marshal(map[string]string{"content": content})
	req, err := http.NewRequestWithContext(ctx, "POST", api+"/channels/"+ch+"/messages", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bot "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("discord: http %d", resp.StatusCode)
	}
	return nil
}
