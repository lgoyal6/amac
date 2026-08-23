// Package discord is the one place that knows how to reach Laksh's phone.
//
// It reuses the bot agentmon already registered: token in the login keychain,
// DM channel cached on disk. A second bot would mean a second token to rotate
// and a second thread to check, for a channel already proven to arrive.
package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

const api = "https://discord.com/api/v10"

var client = &http.Client{Timeout: 20 * time.Second}

func token() string {
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

func channel() string {
	b, err := os.ReadFile(os.Getenv("HOME") + "/.agentmon/state/.discord_dm")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// Send posts one message to the DM channel.
func Send(ctx context.Context, content string) error {
	tok, ch := token(), channel()
	if tok == "" {
		return fmt.Errorf("no Discord token (keychain AGENTMON_DISCORD_TOKEN)")
	}
	if ch == "" {
		return fmt.Errorf("no cached DM channel (~/.agentmon/state/.discord_dm)")
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
