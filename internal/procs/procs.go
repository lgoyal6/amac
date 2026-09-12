// Package procs enumerates the agent sessions that exist only in the process
// table.
//
// The board had two sources: the ACP sessions amac started, and tmux. On a
// machine with no tmux server that second source is empty, and the board said
// "0 sessions" while seven claude processes were running: four started by the
// Claude desktop app, three in plain terminals. Nothing amac read could see
// them. The process table can, so it is the third source.
//
// Like the tmux package this is enumeration, not state detection. What ps
// knows is a fact: this binary, these flags, this long. Where the process is
// working is a fact lsof knows. What the session has been doing comes from one
// more fact, the timestamp of the last line Claude wrote to its transcript;
// "working" here means "wrote something in the last two minutes" and nothing
// more. It never says "blocked", because no file can prove that.
package procs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Session struct {
	PID   int
	Agent string // "claude" or "codex"
	// Kind is "desktop" when the binary lives inside an app bundle, which is
	// how the Claude desktop app and the Codex app run their agents, and
	// "terminal" otherwise.
	Kind     string
	Model    string        // --model, when passed
	ResumeID string        // --resume, when passed: names the transcript
	Command  string        // the binary's basename followed by its arguments
	Elapsed  time.Duration // how long the process has been running
	// InTmux is set when a tmux server is an ancestor. Those sessions are the
	// tmux package's to list, and listing them here too would put every one
	// of them on the board twice.
	InTmux bool

	// Filled by List, not by Parse: they come from lsof and the transcript
	// rather than from ps. Empty when the lookup failed or was ambiguous.
	Dir          string
	Transcript   string
	LastActivity time.Time
}

// Working is how recent the transcript's last write has to be for the session
// to read as working rather than idle. Two minutes is long enough to cover a
// slow model turn and short enough that "working" still means something.
const Working = 2 * time.Minute

// State is the only claim this package makes about what a session is doing.
func (s Session) State(now time.Time) string {
	switch {
	case s.LastActivity.IsZero():
		return "unknown"
	case now.Sub(s.LastActivity) < Working:
		return "working"
	default:
		return "idle"
	}
}

// List returns every claude and codex process on this machine that is not
// inside tmux, with its directory and, where one can be found, its transcript.
func List() ([]Session, error) {
	out, err := exec.Command("ps", "-axwwo", "pid,ppid,etime,command").Output()
	if err != nil {
		return nil, fmt.Errorf("ps: %w", err)
	}
	list := Parse(string(out))
	if len(list) == 0 {
		return nil, nil
	}
	dirs := cwds(list)
	now := time.Now()
	for i := range list {
		list[i].Dir = dirs[list[i].PID]
	}
	attachTranscripts(list, now)
	return list, nil
}

// Parse reads `ps -axo pid,ppid,etime,command` output and keeps the agent
// processes. It is separate from List so it can be handed a fixture.
func Parse(ps string) []Session {
	type row struct {
		pid, ppid int
		exe       string
	}
	rows := map[int]row{}
	var out []Session
	for _, line := range strings.Split(ps, "\n") {
		pidS, rest := field(line)
		ppidS, rest := field(rest)
		etime, command := field(rest)
		pid, err1 := strconv.Atoi(pidS)
		ppid, err2 := strconv.Atoi(ppidS)
		if err1 != nil || err2 != nil || command == "" {
			continue // the header, or a line ps mangled
		}
		exe, args := executable(command)
		rows[pid] = row{pid: pid, ppid: ppid, exe: path.Base(exe)}
		agent := path.Base(exe)
		if agent != "claude" && agent != "codex" {
			continue
		}
		s := Session{
			PID: pid, Agent: agent, Kind: "terminal", Elapsed: elapsed(etime),
			Command: strings.TrimSpace(agent + " " + strings.Join(args, " ")),
		}
		if strings.Contains(exe, ".app/Contents/") {
			s.Kind = "desktop"
		}
		s.Model = argValue(args, "--model")
		s.ResumeID = argValue(args, "--resume", "-r")
		out = append(out, s)
	}
	// The ancestry walk needs the whole table, so it runs once every row is in.
	for i := range out {
		for pid, hops := out[i].PID, 0; hops < 64; hops++ {
			r, ok := rows[pid]
			if !ok || r.ppid == pid || r.ppid <= 1 {
				break
			}
			if rows[r.ppid].exe == "tmux" {
				out[i].InTmux = true
				break
			}
			pid = r.ppid
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out
}

// field splits one whitespace-separated column off the front of a line.
func field(line string) (string, string) {
	line = strings.TrimLeft(line, " \t")
	i := strings.IndexAny(line, " \t")
	if i < 0 {
		return line, ""
	}
	return line[:i], line[i:]
}

// executable finds where the binary ends and its arguments begin.
//
// The command column is argv joined with spaces, and the Claude desktop app
// runs its agent from "~/Library/Application Support/...", so the first
// space is not the end of the path. A path is extended a token at a time until
// it names one of the agents; an argument, which starts with a dash, ends the
// search. A bare name is taken as it is.
func executable(command string) (exe string, args []string) {
	tokens := strings.Fields(command)
	if len(tokens) == 0 {
		return "", nil
	}
	if !strings.HasPrefix(tokens[0], "/") {
		return tokens[0], tokens[1:]
	}
	for i, tok := range tokens {
		if i > 0 && strings.HasPrefix(tok, "-") {
			break
		}
		exe = strings.Join(tokens[:i+1], " ")
		if b := path.Base(exe); b == "claude" || b == "codex" {
			return exe, tokens[i+1:]
		}
	}
	return tokens[0], tokens[1:]
}

// argValue returns the value of the first of names present in args, accepting
// both `--name value` and `--name=value`.
func argValue(args []string, names ...string) string {
	for i, a := range args {
		for _, n := range names {
			if strings.HasPrefix(a, n+"=") {
				return a[len(n)+1:]
			}
			if a == n && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				return args[i+1]
			}
		}
	}
	return ""
}

// elapsed parses ps's etime column: [[dd-]hh:]mm:ss.
func elapsed(s string) time.Duration {
	var days int64
	if d, rest, ok := strings.Cut(s, "-"); ok {
		days, _ = strconv.ParseInt(d, 10, 64)
		s = rest
	}
	var total int64
	for _, part := range strings.Split(s, ":") {
		n, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			return 0
		}
		total = total*60 + n
	}
	return time.Duration(days)*24*time.Hour + time.Duration(total)*time.Second
}

// cwds asks lsof for every process's working directory in one call. A failure
// is an empty map, not an error: the session is still real without it.
func cwds(list []Session) map[int]string {
	pids := make([]string, 0, len(list))
	for _, s := range list {
		pids = append(pids, strconv.Itoa(s.PID))
	}
	// lsof exits 1 when any listed process has gone, and still prints the
	// rest, so the output is read regardless of the exit status.
	out, _ := exec.Command("lsof", "-a", "-p", strings.Join(pids, ","), "-d", "cwd", "-Fpn").Output()
	dirs := map[int]string{}
	pid := 0
	for _, line := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(line, "p"):
			pid, _ = strconv.Atoi(line[1:])
		case strings.HasPrefix(line, "n") && pid != 0:
			dirs[pid] = line[1:]
		}
	}
	return dirs
}

// ------------------------------------------------------------ transcripts --

// Slug is the directory name Claude Code keeps a project's transcripts under:
// the working directory with every character outside [A-Za-z0-9] turned into
// a dash. Checked against ~/.claude/projects on this machine: a space and a
// leading dot both become dashes, so "/Users/x/.ao" is "-Users-x--ao".
func Slug(dir string) string {
	return nonAlnum.ReplaceAllString(dir, "-")
}

var nonAlnum = regexp.MustCompile(`[^A-Za-z0-9]`)

// ProjectsDir is where Claude Code writes transcripts.
func ProjectsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "projects")
}

// attachTranscripts finds each Claude session's transcript and reads its last
// timestamp. Codex keeps its sessions in another layout this does not read,
// so those stay "unknown".
func attachTranscripts(list []Session, now time.Time) {
	projects := ProjectsDir()
	if projects == "" {
		return
	}
	// A session that named its transcript is unambiguous. One that did not is
	// matched to the newest transcript in its directory written since it
	// started, and that is only honest when it is the sole such session
	// there: two bare `claude` processes in one directory both write to that
	// slug, and nothing on disk says which file is whose. Those stay unknown
	// rather than both being handed the same file.
	unnamed := map[string]int{}
	for _, s := range list {
		if s.Agent == "claude" && s.ResumeID == "" && s.Dir != "" {
			unnamed[s.Dir]++
		}
	}
	for i := range list {
		s := &list[i]
		if s.Agent != "claude" {
			continue
		}
		switch {
		case s.ResumeID != "":
			s.Transcript = named(projects, s.Dir, s.ResumeID)
		case s.Dir != "" && unnamed[s.Dir] == 1:
			s.Transcript = newest(filepath.Join(projects, Slug(s.Dir)), now.Add(-s.Elapsed))
		}
		if s.Transcript != "" {
			s.LastActivity = lastTimestamp(s.Transcript)
		}
	}
}

// named locates <id>.jsonl, in the session's own slug first and then anywhere
// under projects: a session resumed from a different directory than it was
// started in keeps the transcript it started with.
func named(projects, dir, id string) string {
	if dir != "" {
		p := filepath.Join(projects, Slug(dir), id+".jsonl")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	matches, _ := filepath.Glob(filepath.Join(projects, "*", id+".jsonl"))
	if len(matches) > 0 {
		return matches[0]
	}
	return ""
}

// newest returns the most recently written transcript in dir that has been
// written since the process started, or "" when none has. The minute of
// slack covers the process starting before its first line lands.
func newest(dir string, started time.Time) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var best string
	var bestAt time.Time
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		at := info.ModTime()
		if at.Before(started.Add(-time.Minute)) || !at.After(bestAt) {
			continue
		}
		best, bestAt = filepath.Join(dir, e.Name()), at
	}
	return best
}

// tailBytes bounds how much of a transcript is read. The last lines are what
// matter and a transcript is routinely megabytes; a tool result can push one
// line past 64K, so the read is generous, and the file's own mtime stands in
// when no line in the tail carries a timestamp.
const tailBytes = 256 << 10

// lastTimestamp is when the transcript was last written to, taken from the
// newest line that carries a timestamp. Not every line does: Claude writes
// bookkeeping lines ("last-prompt", "mode") with none, and those are often
// the final line.
func lastTimestamp(p string) time.Time {
	f, err := os.Open(p)
	if err != nil {
		return time.Time{}
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return time.Time{}
	}
	start := info.Size() - tailBytes
	if start < 0 {
		start = 0
	}
	buf, err := io.ReadAll(io.NewSectionReader(f, start, info.Size()-start))
	if err != nil {
		return time.Time{}
	}
	lines := bytes.Split(buf, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		var line struct {
			Timestamp time.Time `json:"timestamp"`
		}
		if json.Unmarshal(lines[i], &line) == nil && !line.Timestamp.IsZero() {
			return line.Timestamp
		}
	}
	return info.ModTime()
}
