package daemon

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lgoyal6/amac/internal/procs"
)

// A session amac only sees still has something to show.
//
// The board learned to list desktop and terminal claude processes, and then
// clicking one did nothing, because the pane was tmux's screen and these have
// none. They do have a record: a desktop session writes a transcript amac
// already reads for its state, and a hackqueue build writes the run's log line
// by line. Either is a read-only pane. Nothing here can send a key, and the
// handler that would is already refused for these ids.

// hackqueueBuildsDir is where dispatch.mjs builds; a claude whose directory is
// under it is a build, and the interesting record is the run log, not the
// transcript it deliberately does not keep (--no-session-persistence).
func hackqueueBuildsDir() string { return homePath("hackqueue-builds") }
func hackqueueRunsDir() string   { return homePath("Documents/agent-materials/hackqueue_materials/runs") }

// hackqueueSlug is the build directory's basename when the process is a
// hackqueue build, else "".
func hackqueueSlug(dir string) string {
	base := hackqueueBuildsDir()
	if dir == "" || !strings.HasPrefix(dir, base+string(filepath.Separator)) {
		return ""
	}
	rest := strings.TrimPrefix(dir, base+string(filepath.Separator))
	return strings.SplitN(rest, string(filepath.Separator), 2)[0]
}

// buildLog is the newest build-v*/claude.log for a slug, or "".
func buildLog(slug string) string {
	matches, _ := filepath.Glob(filepath.Join(hackqueueRunsDir(), slug, "build-v*", "claude.log"))
	if len(matches) == 0 {
		return ""
	}
	sort.Slice(matches, func(i, j int) bool {
		a, _ := os.Stat(matches[i])
		b, _ := os.Stat(matches[j])
		if a == nil || b == nil {
			return a != nil
		}
		return a.ModTime().After(b.ModTime())
	})
	return matches[0]
}

// procPane finds the record behind a pid-<n> id and renders its tail as text.
func (s *Server) procPane(id string, n int) (paneView, bool) {
	pid, err := strconv.Atoi(strings.TrimPrefix(id, procIDPrefix))
	if err != nil {
		return paneView{}, false
	}
	list, err := procs.List()
	if err != nil {
		return paneView{}, false
	}
	for _, p := range list {
		if p.PID != pid {
			continue
		}
		var text string
		switch {
		case hackqueueSlug(p.Dir) != "" && buildLog(hackqueueSlug(p.Dir)) != "":
			text = renderStream(buildLog(hackqueueSlug(p.Dir)), n)
		case p.Transcript != "":
			text = renderStream(p.Transcript, n)
		default:
			text = "no transcript or log to show for this session; it is running in " + p.Dir
		}
		return paneView{Session: id, Text: text, At: time.Now()}, true
	}
	return paneView{}, false
}

// renderStream turns the last lines of a Claude Code JSONL stream, transcript
// or stream-json run log alike, into the text a person would read: who spoke,
// and what they said, tool calls named rather than dumped. Only the tail of
// the file is read; transcripts run to hundreds of megabytes.
func renderStream(path string, n int) string {
	f, err := os.Open(path)
	if err != nil {
		return "could not read " + path
	}
	defer f.Close()
	const window = 512 << 10
	if st, err := f.Stat(); err == nil && st.Size() > window {
		_, _ = f.Seek(st.Size()-window, io.SeekStart)
		r := bufio.NewReader(f)
		_, _ = r.ReadString('\n') // discard the partial first line
		return renderLines(r, n)
	}
	return renderLines(bufio.NewReader(f), n)
}

func renderLines(r *bufio.Reader, n int) string {
	var out []string
	for {
		line, err := r.ReadString('\n')
		if line != "" {
			out = append(out, speak(line)...)
		}
		if err != nil {
			break
		}
	}
	// Drop blanks, keep the last n non-empty lines.
	var kept []string
	for _, l := range out {
		if strings.TrimSpace(l) != "" {
			kept = append(kept, l)
		}
	}
	if len(kept) > n {
		kept = kept[len(kept)-n:]
	}
	if len(kept) == 0 {
		return "(nothing written yet)"
	}
	return strings.Join(kept, "\n")
}

// speak renders one JSONL event as zero or more lines.
func speak(line string) []string {
	var ev struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
		Message struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"message"`
		Result string `json:"result"`
	}
	if json.Unmarshal([]byte(line), &ev) != nil {
		return nil
	}
	switch ev.Type {
	case "user", "assistant":
		who := "you"
		if ev.Type == "assistant" {
			who = "claude"
		}
		return contentLines(who, ev.Message.Content)
	case "result":
		if ev.Result != "" {
			return []string{"result: " + firstLine(ev.Result, 200)}
		}
	case "system":
		if ev.Subtype == "init" {
			return []string{"-- session started --"}
		}
	}
	return nil
}

func contentLines(who string, raw json.RawMessage) []string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []string{who + ": " + firstLine(text, 240)}
	}
	var blocks []struct {
		Type  string `json:"type"`
		Text  string `json:"text"`
		Name  string `json:"name"`
		Input any    `json:"input"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return nil
	}
	var out []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if strings.TrimSpace(b.Text) != "" {
				out = append(out, who+": "+firstLine(b.Text, 240))
			}
		case "tool_use":
			out = append(out, "  > "+b.Name+" "+toolHint(b.Input))
		case "tool_result":
			out = append(out, "  < result")
		}
	}
	return out
}

func toolHint(input any) string {
	m, ok := input.(map[string]any)
	if !ok {
		return ""
	}
	for _, k := range []string{"command", "description", "file_path", "pattern", "query", "url", "prompt"} {
		if v, ok := m[k].(string); ok && v != "" {
			return firstLine(v, 120)
		}
	}
	return ""
}

func firstLine(s string, max int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " ..."
	}
	if len(s) > max {
		return s[:max-3] + "..."
	}
	return s
}
