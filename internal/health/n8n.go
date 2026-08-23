package health

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// job-discovery runs as an n8n workflow on Railway. Its Postgres holds the
// authoritative source_runs and email_batches tables, but it is bound to
// postgres.railway.internal and the project runbook says not to expose it
// through a TCP proxy for routine administration. So this probe reads n8n's
// own REST API over the public host instead, which reports the same executions
// without opening the database to the internet.
const (
	n8nHost       = "n8n-production-a322.up.railway.app"
	n8nWorkflowID = "LakshJobDiscovery2h"
)

var httpc = &http.Client{Timeout: 20 * time.Second}

// n8nKey reads the API key from the login keychain, matching where the Discord
// bot token already lives. Returns empty when unset, which downgrades the probe
// to liveness rather than failing it.
func n8nKey() string {
	if k := os.Getenv("AMAC_N8N_API_KEY"); k != "" {
		return k
	}
	out, err := exec.Command("security", "find-generic-password",
		"-w", "-s", "AMAC_N8N_API_KEY", "-a", os.Getenv("USER")).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// JobDiscovery reports the last completed run of the two-hourly digest.
//
// Without an API key it can still prove n8n is alive, but not that the
// workflow is firing, so it reports Unknown rather than OK. A monitor that
// says "green" when it only checked that the web server answers is worse than
// no monitor, because it converts an unknown into a false assurance.
func JobDiscovery(ctx context.Context) (Report, error) {
	r := Report{State: OK}

	key := n8nKey()
	if key == "" {
		alive, err := n8nAlive(ctx)
		if err != nil || !alive {
			r.State = Down
			r.Detail = "n8n /healthz unreachable"
			if err != nil {
				r.Err = err.Error()
			}
			return r, nil
		}
		r.State = Unknown
		r.Detail = "n8n is up, but execution history needs an API key"
		r.Notes = append(r.Notes, "set one: n8n Settings > API, then "+
			`security add-generic-password -s AMAC_N8N_API_KEY -a "$USER" -w '<key>'`)
		return r, nil
	}

	req, err := http.NewRequestWithContext(ctx, "GET",
		fmt.Sprintf("https://%s/api/v1/executions?workflowId=%s&limit=5", n8nHost, n8nWorkflowID), nil)
	if err != nil {
		return r, err
	}
	req.Header.Set("X-N8N-API-KEY", key)
	resp, err := httpc.Do(req)
	if err != nil {
		r.State = Down
		r.Detail = "n8n unreachable"
		r.Err = err.Error()
		return r, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return r, fmt.Errorf("n8n API key rejected (401), rotate AMAC_N8N_API_KEY")
	}
	if resp.StatusCode != http.StatusOK {
		return r, fmt.Errorf("n8n api: http %d", resp.StatusCode)
	}

	var body struct {
		Data []struct {
			Status    string     `json:"status"`
			StartedAt time.Time  `json:"startedAt"`
			StoppedAt *time.Time `json:"stoppedAt"`
			Finished  bool       `json:"finished"`
			ID        string     `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return r, fmt.Errorf("n8n api decode: %w", err)
	}
	if len(body.Data) == 0 {
		r.State = Unknown
		r.Detail = "no executions recorded for " + n8nWorkflowID
		return r, nil
	}

	// Newest first. Skip anything still running: an in-flight execution is not
	// yet evidence either way, and treating it as the last outcome would flap.
	for _, e := range body.Data {
		if !e.Finished && e.StoppedAt == nil {
			continue
		}
		when := e.StartedAt
		if e.StoppedAt != nil {
			when = *e.StoppedAt
		}
		r.Last = when
		if e.Status == "success" {
			r.Detail = "last digest " + short(time.Since(when)) + " ago"
		} else {
			r.State = Failing
			r.Detail = fmt.Sprintf("last execution %s (%s ago), https://%s/execution/%s",
				e.Status, short(time.Since(when)), n8nHost, e.ID)
		}
		return r, nil
	}
	r.State = Unknown
	r.Detail = "all recent executions still running"
	return r, nil
}

func n8nAlive(ctx context.Context) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", "https://"+n8nHost+"/healthz", nil)
	if err != nil {
		return false, err
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK, nil
}
