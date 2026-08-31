package daemon

// The queue, from the board.
//
// Filing work from a phone is the case this exists for. Something breaks, or
// you think of something on the way somewhere, and the alternative to writing
// it down in a place agents can reach is writing it down in a place only you
// can, which is how a list of things becomes a list of things you feel guilty
// about.
//
// Claiming from here opens a session for the task and hands the fencing token
// to whoever is holding it. The board never finishes work on a worker's behalf:
// a task is done when the thing doing it says so, and a button that closed a
// task the moment a session opened would be reporting an intention as a result.

import (
	"encoding/json"
	"net/http"
	"os"
	"time"

	"github.com/lgoyal6/amac/internal/crew"
	"github.com/lgoyal6/amac/internal/queue"
)

// boardLease is how long a claim made from the board is held without renewal.
//
// Longer than the CLI's, because nobody is going to renew this one: a task
// claimed from a phone is being worked by an agent in a tmux session, and the
// board has no way to know whether that session is still thinking. Fifteen
// minutes would take the task back off an agent mid-run; an hour is long enough
// that expiry means something really went wrong.
const boardLease = time.Hour

func (s *Server) tasks(w http.ResponseWriter, r *http.Request) {
	list, err := s.queue.List(r.Context(), queue.State(r.URL.Query().Get("state")))
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, list)
}

func (s *Server) fileTask(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title string `json:"title"`
		Dir   string `json:"dir"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Title == "" {
		writeJSON(w, 400, map[string]string{"error": "title required"})
		return
	}
	if body.Dir == "" {
		body.Dir, _ = os.UserHomeDir()
	}
	t, err := s.queue.File(r.Context(), queue.Task{
		ID: crew.Slug(body.Title), Title: body.Title, Dir: body.Dir,
	})
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, t)
}

// claimTask takes the next task and opens a session on it.
//
// The next one rather than a named one, deliberately. A queue whose consumers
// pick which item to take is a list, and the ordering it was filed in stops
// meaning anything.
func (s *Server) claimTask(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Owner string `json:"owner"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Owner == "" {
		body.Owner = "board"
	}

	t, err := s.queue.Claim(r.Context(), body.Owner, boardLease)
	if err == queue.ErrNoWork {
		writeJSON(w, 409, map[string]string{"error": "nothing claimable"})
		return
	}
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}

	sess := crew.Session{
		Name: crew.Name(t.ID, "worker"), Role: "worker", Agent: "claude",
		Dir: t.Dir, Output: crew.RunDir(t.ID) + "/worker.md",
	}
	if crew.Exists(sess.Name) {
		// The session outlived a previous claim. Handing the task to a second
		// agent in the same tmux session would put two of them on one keyboard.
		_ = s.queue.Release(r.Context(), t.ID, t.Token)
		writeJSON(w, 409, map[string]string{
			"error": sess.Name + " is already open; take that over instead"})
		return
	}
	brief := crew.Brief(sess, "Do this task. It came off amac's queue.", t.Title)
	if err := crew.Open(sess, brief); err != nil {
		// The claim is given back rather than left held by a session that does
		// not exist, which would make the task unclaimable until the lease ran
		// out for no reason anyone could see.
		_ = s.queue.Release(r.Context(), t.ID, t.Token)
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, 200, map[string]any{
		"task": t, "session": sess.Name, "attach": sess.Attach(),
	})
}

func (s *Server) finishTask(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token  int64  `json:"token"`
		State  string `json:"state"`
		Result string `json:"result"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	id := r.PathValue("id")

	var err error
	if body.State == "released" {
		err = s.queue.Release(r.Context(), id, body.Token)
	} else {
		state := queue.State(body.State)
		if state == "" {
			state = queue.Done
		}
		err = s.queue.Finish(r.Context(), id, body.Token, state, body.Result)
	}
	if err == queue.ErrNotHeld {
		// Said plainly. A worker that has been fenced needs to stop, and an
		// error that reads like a transient failure invites a retry that will
		// be refused for the same reason.
		writeJSON(w, 409, map[string]string{"error": err.Error()})
		return
	}
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

func (s *Server) cancelTask(w http.ResponseWriter, r *http.Request) {
	if err := s.queue.CancelReady(r.Context(), r.PathValue("id"), "withdrawn from board"); err != nil {
		if err == queue.ErrNotHeld {
			writeJSON(w, 409, map[string]string{"error": "task is not ready; refresh before changing it"})
			return
		}
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]string{"status": "canceled"})
}
