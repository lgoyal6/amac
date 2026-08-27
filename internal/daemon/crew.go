package daemon

// The org, driven from the board.
//
// `amac crew` already lays a task out as a chain of roles and opens each one
// as a tmux session a human can take over. What it could not do was be watched
// from anywhere but the terminal it was typed into, which for a run whose
// whole point is that a human steps in partway through is the wrong place for
// it to live.
//
// Nothing about the chain changes here. The handoff is still a file, the roles
// are still real sessions, and the board still opens one role at a time rather
// than all four: a role whose input does not exist yet has nothing to read,
// and opening it early burns a context window waiting for a file. The board is
// a second front end to the same mechanism, not a second mechanism.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/lgoyal6/amac/internal/crew"
	"github.com/lgoyal6/amac/internal/event"
	"github.com/lgoyal6/amac/internal/orchestrator"
)

type roleView struct {
	Role    string `json:"role"`
	Agent   string `json:"agent"`
	Session string `json:"session"`
	State   string `json:"state"` // running | done | waiting | ready
	Output  string `json:"output"`
	Input   string `json:"input,omitempty"`
	Bytes   int64  `json:"bytes,omitempty"`
}

type planView struct {
	Task   string     `json:"task"`
	Slug   string     `json:"slug"`
	Dir    string     `json:"dir"`
	Size   string     `json:"size"`
	Reason string     `json:"reason"`
	RunDir string     `json:"runDir"`
	Roles  []roleView `json:"roles"`
	// Next is the role a click would open, or "" when the chain is finished or
	// blocked on one that is still running. The board asks rather than
	// deciding for itself, so the button can say what it is about to do.
	Next string `json:"next,omitempty"`
}

func (s *Server) plan(task, dir, size string) planView {
	sessions := s.orch.Attachable(task, dir, orchestrator.Size(size))
	slug := crew.Slug(task)
	v := planView{Task: task, Slug: slug, Dir: dir, Size: size, RunDir: crew.RunDir(slug)}

	for _, sess := range sessions {
		r := roleView{
			Role: sess.Role, Agent: sess.Agent, Session: sess.Name,
			State: crew.Status(sess), Output: sess.Output, Input: sess.Input,
		}
		if st, err := os.Stat(sess.Output); err == nil {
			r.Bytes = st.Size()
		}
		if v.Next == "" && r.State == "ready" {
			v.Next = sess.Role
		}
		v.Roles = append(v.Roles, r)
	}
	return v
}

// crewPlan grades a task and returns the chain without opening anything.
//
// Deciding what to open and opening it stay apart here for the same reason
// they do in the CLI: a run that will stall on its third role should say so
// before the first one has burned any tokens.
func (s *Server) crewPlan(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Task string `json:"task"`
		Dir  string `json:"dir"`
		Size string `json:"size"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	body.Task = strings.TrimSpace(body.Task)
	if body.Task == "" {
		writeJSON(w, 400, map[string]string{"error": "task required"})
		return
	}
	if body.Dir == "" {
		body.Dir, _ = os.UserHomeDir()
	}

	reason := "forced by caller"
	if body.Size == "" {
		// Triage asks the cheap tier one question and falls back to
		// heuristics when no model answers, so this cannot block the board.
		size, why := s.orch.Triage(r.Context(), body.Task)
		body.Size, reason = string(size), why
	}

	v := s.plan(body.Task, body.Dir, body.Size)
	v.Reason = reason
	writeJSON(w, 200, v)
}

// crewOpen opens one role: the named one, or the next whose input exists.
//
// One at a time, deliberately. The CLI's -all exists for a chain whose
// artifacts are already on disk; from a phone the useful action is "the plan
// looks right, start the executor", and opening four sessions at once when
// three of them have nothing to read is not that.
func (s *Server) crewOpen(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Task string `json:"task"`
		Dir  string `json:"dir"`
		Size string `json:"size"`
		Role string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	body.Task = strings.TrimSpace(body.Task)
	if body.Task == "" {
		writeJSON(w, 400, map[string]string{"error": "task required"})
		return
	}
	if body.Dir == "" {
		body.Dir, _ = os.UserHomeDir()
	}
	if body.Size == "" {
		size, _ := s.orch.Triage(r.Context(), body.Task)
		body.Size = string(size)
	}

	sessions := s.orch.Attachable(body.Task, body.Dir, orchestrator.Size(body.Size))
	var chosen *crewTarget
	for _, sess := range sessions {
		state := crew.Status(sess)
		if body.Role != "" && sess.Role != body.Role {
			continue
		}
		if state != "ready" {
			if body.Role != "" {
				writeJSON(w, 409, map[string]string{
					"error": fmt.Sprintf("%s is %s, not ready to open", sess.Role, state)})
				return
			}
			continue
		}
		chosen = &crewTarget{sess: sess}
		break
	}
	if chosen == nil {
		writeJSON(w, 409, map[string]string{
			"error": "nothing to open: every role is running, finished, or waiting on the one before it"})
		return
	}

	if err := crew.Open(chosen.sess, orchestrator.BriefFor(chosen.sess, body.Task)); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	// Size is recorded with the run because the board has to rebuild this
	// exact chain later from the log alone, and a chain rebuilt at the wrong
	// size points the next role at a file the previous one never wrote.
	if ev, err := event.New(event.KindSessionStarted, "crew", chosen.sess.Name, map[string]any{
		"role": chosen.sess.Role, "agent": chosen.sess.Agent, "task": body.Task,
		"dir": chosen.sess.Dir, "input": chosen.sess.Input, "output": chosen.sess.Output,
		"size": body.Size, "slug": crew.Slug(body.Task),
	}); err == nil {
		_, _ = s.log.Append(r.Context(), ev)
	}

	writeJSON(w, 200, s.plan(body.Task, body.Dir, body.Size))
}

type crewTarget struct{ sess crew.Session }

type runView struct {
	planView
	Started time.Time `json:"started"`
}

// crewRuns rebuilds every run the log knows about.
//
// The runs are read out of the event log rather than off the run directory,
// because the directory holds artifacts and the log holds what the run was:
// its task, its working directory, and the size it was graded at. A directory
// full of planner.md files cannot tell you which prompt produced them.
func (s *Server) crewRuns(w http.ResponseWriter, r *http.Request) {
	rows, err := s.log.DB().QueryContext(r.Context(), `
		SELECT at, payload FROM events
		 WHERE kind = ? AND source = 'crew'
		 ORDER BY seq DESC LIMIT 200`, string(event.KindSessionStarted))
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()

	type seed struct {
		task, dir, size string
		at              time.Time
		roles           int
	}
	order := []string{}
	seeds := map[string]*seed{}

	for rows.Next() {
		// TEXT in the store; the driver hands back a string and scanning
		// straight into a time.Time fails silently into an empty result.
		var at string
		var payload []byte
		if rows.Scan(&at, &payload) != nil {
			continue
		}
		started, _ := time.Parse(time.RFC3339Nano, at)
		var body struct {
			Task string `json:"task"`
			Dir  string `json:"dir"`
			Size string `json:"size"`
		}
		if json.Unmarshal(payload, &body) != nil || body.Task == "" {
			continue
		}
		slug := crew.Slug(body.Task)
		if s, ok := seeds[slug]; ok {
			s.roles++
			if s.size == "" {
				s.size = body.Size
			}
			continue
		}
		seeds[slug] = &seed{task: body.Task, dir: body.Dir, size: body.Size, at: started, roles: 1}
		order = append(order, slug)
	}

	out := []runView{}
	for _, slug := range order {
		sd := seeds[slug]
		// Runs opened before size was recorded still have to rebuild. The
		// number of roles that were opened is a fact about the run rather than
		// a guess about it, and it names the org exactly.
		if sd.size == "" {
			switch {
			case sd.roles <= 1:
				sd.size = string(orchestrator.SizeSolo)
			case sd.roles == 2:
				sd.size = string(orchestrator.SizePair)
			default:
				sd.size = string(orchestrator.SizeTeam)
			}
		}
		out = append(out, runView{planView: s.plan(sd.task, sd.dir, sd.size), Started: sd.at})
		if len(out) >= 20 {
			break
		}
	}
	writeJSON(w, 200, out)
}

var safeRole = regexp.MustCompile(`^[a-z]+$`)

// crewArtifact serves one handoff file.
//
// The path is rebuilt from a slug and a role rather than accepted from the
// caller. Slug() is idempotent, so a slug that does not survive it is not one
// this system produced, and a role is a bare word. Between them there is no
// input that can escape the run directory, which matters because this endpoint
// is reachable from a phone with a token and reads files off disk.
func (s *Server) crewArtifact(w http.ResponseWriter, r *http.Request) {
	slug := r.URL.Query().Get("slug")
	role := r.URL.Query().Get("role")
	if slug == "" || slug != crew.Slug(slug) || !safeRole.MatchString(role) {
		writeJSON(w, 400, map[string]string{"error": "bad slug or role"})
		return
	}
	path := filepath.Join(crew.RunDir(slug), role+".md")
	b, err := os.ReadFile(path)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "no artifact yet"})
		return
	}
	writeJSON(w, 200, map[string]any{"slug": slug, "role": role, "path": path, "text": string(b)})
}
