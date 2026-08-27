package daemon

// Mirroring a pane, and typing into one.
//
// Everywhere else this codebase refuses to read a rendered terminal. That rule
// exists because the predecessor inferred agent state from one with regexes
// and was confidently wrong often enough to need two competing detectors, and
// ACP was adopted precisely to stop guessing at a rendering.
//
// The rule is about inference, not about pixels. Nothing here parses a pane or
// forms any belief about what is on it. The bytes are forwarded to a human,
// who reads the options with their own eyes exactly as they would three
// seconds after `tmux attach`, and presses the key they want sent. A mirror
// cannot be confidently wrong, because it makes no claim.
//
// That distinction is what lets the board answer a permission prompt in a
// session amac does not own. The alternative on offer was a row of Allow/Deny
// buttons whose mapping to the agent's actual options was a positional guess,
// which is the same bug in a nicer coat: an approval sent to the wrong option
// is worse than no button at all.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lgoyal6/amac/internal/event"
	"github.com/lgoyal6/amac/internal/tmux"
)

// paneLines is how much of the screen the phone gets by default. The whole
// visible pane is usually 50 lines of which the last dozen carry the prompt;
// more than this scrolls off a phone anyway.
const paneLines = 40

// maxTyped bounds one keystroke payload. Typing into a pane is arbitrary shell
// input by design, but a multi-megabyte paste is never an intended action and
// blocks the tmux server while it is delivered.
const maxTyped = 4 << 10

type paneView struct {
	Session string    `json:"session"`
	Text    string    `json:"text"`
	At      time.Time `json:"at"`
}

// target is the tmux address for a session, validated against the live server.
//
// Validation is not paranoia about the shell: exec.Command takes an argv, so
// nothing here reaches a shell to be injected into. It is about the target
// itself. An unvalidated name would let a stale card type into whatever
// session happens to hold that name now, and typing into the wrong agent is
// the one failure this feature must not have.
//
// The trailing colon is load-bearing. Session commands accept the "=name"
// exact form; pane commands do not, and drop it with "can't find pane".
func (s *Server) target(name string) (string, bool) {
	list, err := tmux.List()
	if err != nil {
		return "", false
	}
	for _, t := range list {
		if t.Name == name {
			return "=" + name + ":", true
		}
	}
	return "", false
}

func (s *Server) pane(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("id")
	target, ok := s.target(name)
	if !ok {
		writeJSON(w, 404, map[string]string{"error": "no such tmux session"})
		return
	}
	n, _ := strconv.Atoi(r.URL.Query().Get("lines"))
	if n <= 0 || n > 200 {
		n = paneLines
	}

	out, err := exec.CommandContext(r.Context(), "tmux", "capture-pane", "-p", "-t", target).Output()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "capture: " + err.Error()})
		return
	}
	writeJSON(w, 200, paneView{Session: name, Text: tail(string(out), n), At: time.Now()})
}

// tail keeps the last n lines and drops the blank ones a pane pads itself with,
// so a phone shows the prompt rather than an empty screen with a prompt above
// the fold.
func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// namedKeys is what may be sent as a key rather than as text.
//
// The list is short because it is the list of things a human presses to answer
// an agent: pick an option, confirm, back out, interrupt. Anything outside it
// is text, and text is sent literally with -l so it can never be reinterpreted
// as a key name or a flag.
var namedKeys = map[string]bool{
	"Enter": true, "Escape": true, "Space": true, "Tab": true, "BSpace": true,
	"Up": true, "Down": true, "Left": true, "Right": true,
	"C-c": true, "C-d": true, "C-r": true,
}

func (s *Server) sendKeys(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("id")
	target, ok := s.target(name)
	if !ok {
		writeJSON(w, 404, map[string]string{"error": "no such tmux session"})
		return
	}

	var body struct {
		Text  string `json:"text"`  // typed literally
		Key   string `json:"key"`   // one named key
		Enter bool   `json:"enter"` // press Enter after the text
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if len(body.Text) > maxTyped {
		writeJSON(w, 413, map[string]string{"error": fmt.Sprintf("text over %d bytes", maxTyped)})
		return
	}
	if body.Key != "" && !namedKeys[body.Key] {
		writeJSON(w, 400, map[string]string{"error": "unknown key " + body.Key})
		return
	}
	if body.Text == "" && body.Key == "" && !body.Enter {
		writeJSON(w, 400, map[string]string{"error": "nothing to send"})
		return
	}

	// -l sends the text as literal input. Without it tmux reads "Enter" or
	// "C-c" inside a message as key names, so asking an agent about a keybind
	// would press it instead.
	if body.Text != "" {
		if err := exec.CommandContext(r.Context(), "tmux",
			"send-keys", "-t", target, "-l", "--", body.Text).Run(); err != nil {
			writeJSON(w, 500, map[string]string{"error": "send: " + err.Error()})
			return
		}
	}
	for _, key := range keysToSend(body.Key, body.Enter) {
		if err := exec.CommandContext(r.Context(), "tmux",
			"send-keys", "-t", target, "--", key).Run(); err != nil {
			writeJSON(w, 500, map[string]string{"error": "send " + key + ": " + err.Error()})
			return
		}
	}

	// Recorded as an actuation, like every other thing amac does to the world
	// outside itself. This one is worth the row more than most: it is
	// indistinguishable at the terminal from Laksh having typed it, so the log
	// is the only place that can say the phone did.
	s.record(r.Context(), name, body.Text, body.Key, body.Enter)

	// The pane a beat later, so the caller sees what its keystroke did without
	// a second round trip. Agents redraw fast; this is not a guarantee that
	// the redraw happened, just the best available answer.
	time.Sleep(150 * time.Millisecond)
	out, _ := exec.Command("tmux", "capture-pane", "-p", "-t", target).Output()
	writeJSON(w, 200, paneView{Session: name, Text: tail(string(out), paneLines), At: time.Now()})
}

func keysToSend(key string, enter bool) []string {
	var out []string
	if key != "" {
		out = append(out, key)
	}
	if enter && key != "Enter" {
		out = append(out, "Enter")
	}
	return out
}

func (s *Server) record(ctx context.Context, session, text, key string, enter bool) {
	sent := text
	if key != "" {
		sent = strings.TrimSpace(sent + " <" + key + ">")
	}
	if enter && key != "Enter" {
		sent = strings.TrimSpace(sent + " <Enter>")
	}
	ev, err := event.New(event.KindActuation, "dashboard", session, map[string]any{
		"op": "send-keys", "target": session, "sent": sent,
	})
	if err != nil {
		return
	}
	_, _ = s.log.Append(ctx, ev)
}

// panes captures every session at once, for the wall.
//
// One request rather than one per card. The board polls, and a grid of twelve
// sessions asking separately is twelve HTTP round trips and twelve renders a
// tick, which on a phone is the difference between a wall and a slideshow.
//
// Captures run concurrently because each is a fork and they do not contend:
// twelve sequential tmux calls at a few milliseconds each is most of a frame
// budget spent waiting on processes that could have run together.
func (s *Server) panes(w http.ResponseWriter, r *http.Request) {
	list, err := tmux.List()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	n, _ := strconv.Atoi(r.URL.Query().Get("lines"))
	if n <= 0 || n > 60 {
		// Fewer than the expanded card shows. A wall trades depth per session
		// for seeing all of them, and a tile deep enough to read properly is a
		// tile you can only fit two of.
		n = 14
	}

	out := make([]paneView, len(list))
	var wg sync.WaitGroup
	for i, t := range list {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			out[i] = paneView{Session: name, At: time.Now()}
			raw, err := exec.CommandContext(r.Context(), "tmux",
				"capture-pane", "-p", "-t", "="+name+":").Output()
			if err != nil {
				// A session that ended between the list and the capture is not
				// an error worth failing the whole wall over.
				out[i].Text = ""
				return
			}
			out[i].Text = tail(string(raw), n)
		}(i, t.Name)
	}
	wg.Wait()
	writeJSON(w, 200, out)
}
