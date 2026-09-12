package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The hackathon pipeline, on the one page that is already on the tailnet.
//
// hackqueue had a board of its own: a kanban rendered by its Cloudflare Worker,
// reachable only through a loopback proxy on the laptop that injects the bearer
// token. That means it opens on exactly one machine, which is the machine you
// are least likely to be holding when a deadline is six hours out.
//
// This is the same split Jobs and Money already use. The Worker stays the write
// side of record, because Discord interactions need a public HTTPS endpoint and
// this daemon refuses to start off the tailnet, so nothing here owns state.
// amac is the reader and the driver.
//
// Two panes, because they answer different questions. The board is the whole
// market, ninety-odd open online hackathons ranked by deadline, prize and
// crowd, and it is read-mostly. The queue is the handful actually offered, and
// it is where a decision happens. Nothing moves from the first to the second
// except the daily sweep or a person.

const (
	hackqueueEnvDefault    = ".config/hackathon-bot.env"
	hackqueueRankedDefault = "hackqueue/data/ranked.json"
	hackqueueTimeout       = 12 * time.Second
)

type hackqueueBoardRow struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	URL       string `json:"url"`
	Organizer string `json:"organizer"`
	Prize     int64  `json:"prize"`
	Going     int64  `json:"going"`
	Closes    string `json:"closes"`
	DaysLeft  *int   `json:"daysLeft"`
	Fit       int    `json:"fit"`
	// Whether hackqueue has already offered this one, so the board can say so
	// rather than inviting a decision that was already made.
	Offered bool   `json:"offered"`
	Status  string `json:"status,omitempty"`
}

type hackqueueEntry struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	URL    string `json:"url"`
	Status string `json:"status"`
	Closes string `json:"closes"`
	Error  string `json:"error,omitempty"`
}

type hackqueueResponse struct {
	Board    []hackqueueBoardRow `json:"board"`
	Queue    []hackqueueEntry    `json:"queue"`
	Waiting  int                 `json:"waiting"`
	Cap      int                 `json:"cap"`
	RankedAt string              `json:"rankedAt,omitempty"`
	Warning  string              `json:"warning,omitempty"`
}

func homePath(rest string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return rest
	}
	return filepath.Join(home, rest)
}

// The secrets stay in the one 0600 file hackqueue already keeps them in. Copying
// them into amac's config would make a second place to rotate and a second place
// to leak, for no gain: both processes are this user on this machine.
func hackqueueEnv() (base, secret string, err error) {
	path := os.Getenv("HACKQUEUE_ENV_FILE")
	if path == "" {
		path = homePath(hackqueueEnvDefault)
	}
	file, err := os.Open(path)
	if err != nil {
		return "", "", fmt.Errorf("hackqueue env file: %w", err)
	}
	defer file.Close()

	scan := bufio.NewScanner(file)
	for scan.Scan() {
		line := strings.TrimSpace(scan.Text())
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		switch strings.TrimSpace(key) {
		case "HACKQUEUE_URL":
			base = strings.TrimSpace(value)
		case "QUEUE_SECRET":
			secret = strings.TrimSpace(value)
		}
	}
	if base == "" || secret == "" {
		return "", "", fmt.Errorf("hackqueue env file has no HACKQUEUE_URL or QUEUE_SECRET")
	}
	return base, secret, nil
}

// The ranked board, off the laptop's disk rather than through the Worker.
//
// rank.mjs is pure local computation over a file the sweep refreshes daily, and
// this daemon runs on the same Mac, so there is nothing to upload to KV and
// nothing to pay for. It also means the board pane still renders when the
// Worker is unreachable, which is the half of this tab that does not need it.
func readRankedBoard() ([]hackqueueBoardRow, string, error) {
	path := os.Getenv("HACKQUEUE_RANKED")
	if path == "" {
		path = homePath(hackqueueRankedDefault)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	var file struct {
		RankedAt string              `json:"rankedAt"`
		Ranked   []hackqueueBoardRow `json:"ranked"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, "", err
	}
	return file.Ranked, file.RankedAt, nil
}

func (s *Server) hackqueueGet(w http.ResponseWriter, r *http.Request) {
	resp := hackqueueResponse{Board: []hackqueueBoardRow{}, Queue: []hackqueueEntry{}, Cap: 10}

	board, rankedAt, err := readRankedBoard()
	if err != nil {
		resp.Warning = "the ranked board has not been written yet; run the sweep on the Mac"
	} else {
		resp.Board = board
		resp.RankedAt = rankedAt
	}

	base, secret, err := hackqueueEnv()
	if err != nil {
		resp.Warning = strings.TrimSpace(resp.Warning + " " + err.Error())
		writeJSON(w, 200, resp)
		return
	}

	queue, err := fetchHackqueue(r, base, secret)
	if err != nil {
		// The board still renders. A tab that shows nothing because one of two
		// sources is down is a tab that tells you less than it knows.
		resp.Warning = strings.TrimSpace(resp.Warning + " the queue is unreachable: " + err.Error())
		writeJSON(w, 200, resp)
		return
	}

	decided := map[string]string{}
	for status, entries := range queue {
		if status == "seen" {
			continue
		}
		for _, entry := range entries {
			entry.Status = status
			resp.Queue = append(resp.Queue, entry)
			decided[entry.ID] = status
		}
		if status == "pending" {
			resp.Waiting += len(entries)
		}
	}
	for i := range resp.Board {
		if status, ok := decided[resp.Board[i].ID]; ok {
			resp.Board[i].Offered = true
			resp.Board[i].Status = status
		}
	}
	// Soonest first in both panes. The board arrives ranked by fit, which is
	// the right order for choosing what to offer next and the wrong one for
	// looking at what is about to close.
	sort.SliceStable(resp.Queue, func(a, b int) bool { return resp.Queue[a].Closes < resp.Queue[b].Closes })
	writeJSON(w, 200, resp)
}

func fetchHackqueue(r *http.Request, base, secret string) (map[string][]hackqueueEntry, error) {
	ctx, cancel := context.WithTimeout(r.Context(), hackqueueTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/queue", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("authorization", "Bearer "+secret)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", res.StatusCode)
	}
	// Decoded a key at a time on purpose. The queue answers with one key per
	// status plus `seen`, which is a list of ids rather than a list of entries,
	// so decoding the whole body into map[string][]hackqueueEntry fails on that
	// one key and loses every status with it.
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(res.Body).Decode(&raw); err != nil {
		return nil, err
	}
	queue := map[string][]hackqueueEntry{}
	for status, body := range raw {
		if status == "seen" {
			continue
		}
		var entries []hackqueueEntry
		if err := json.Unmarshal(body, &entries); err != nil {
			// A status this build does not understand is skipped rather than
			// failing the request: a new one added to the Worker should cost a
			// missing column here, not an empty tab.
			continue
		}
		// An id is identity, and Go will happily decode any JSON object into
		// this struct by ignoring every field it does not recognise. Without
		// this, a key holding something that is not an entry at all decodes
		// into a blank one and renders as an empty row with an empty deadline,
		// which then sorts to the top of a list ordered by what closes soonest.
		kept := entries[:0]
		for _, entry := range entries {
			if entry.ID != "" {
				kept = append(kept, entry)
			}
		}
		queue[status] = kept
	}
	return queue, nil
}

// Only pass and retry.
//
// Discord keeps its buttons, so this is deliberately not a second copy of every
// decision. Choosing a direction or approving a build is a judgement made
// against three researched embeds, and Discord already renders those well. Pass
// and retry are the two that are worth having on a phone: one closes something
// the clock already settled, the other restarts a run that died, and neither
// needs anything on screen to decide.
//
// The Worker re-checks both. A pass is refused unless the deadline has passed
// and a retry is refused unless the entry is failed, so this cannot invent a
// transition even if the button is wrong about what it is looking at.
func (s *Server) hackqueueAct(w http.ResponseWriter, r *http.Request) {
	action := r.PathValue("action")
	if action != "pass" && action != "retry" {
		writeJSON(w, 400, map[string]string{"error": "only pass and retry are driven from here"})
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID == "" {
		writeJSON(w, 400, map[string]string{"error": "id required"})
		return
	}
	base, secret, err := hackqueueEnv()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), hackqueueTimeout)
	defer cancel()
	form := url.Values{"id": {body.ID}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/board/"+action, strings.NewReader(form.Encode()))
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	req.Header.Set("authorization", "Bearer "+secret)
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	// The board's handlers answer 303 back to /board on both success and
	// refusal, so following the redirect would fetch a whole HTML page to learn
	// nothing. The next GET is what says whether anything moved.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	defer res.Body.Close()
	if res.StatusCode >= 400 {
		writeJSON(w, 502, map[string]string{"error": fmt.Sprintf("hackqueue answered HTTP %d", res.StatusCode)})
		return
	}
	writeJSON(w, 200, map[string]string{"ok": action})
}
