package orchestrator

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lgoyal6/amac/internal/event"
	"github.com/lgoyal6/amac/internal/model"
	"github.com/lgoyal6/amac/internal/router"
	"github.com/lgoyal6/amac/internal/supervisor"
)

// Triage decides how many agents a task is worth, and it is the CFO doing its
// own job on itself: the decision about how much to spend is the last place
// that should be expensive. It had no test, including the fallback, which is
// the path this machine has always actually taken because no model key is
// configured here.

func orch(t *testing.T, r *router.Router) *Orchestrator {
	t.Helper()
	log, err := event.Open(filepath.Join(t.TempDir(), "events.db"), event.Relaxed)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	return New(supervisor.New(log), r, log)
}

// With no model reachable, triage must still answer. A triage step that can
// fail is a triage step that blocks all work.
func TestTriageFallsBackToHeuristicsWithoutAModel(t *testing.T) {
	o := orch(t, nil)
	size, why := o.Triage(context.Background(), "rename a variable")
	if size == "" {
		t.Fatal("triage returned no size")
	}
	// The reason has to say the grade was not graded, or a heuristic answer
	// reads as a model's judgement.
	if !strings.Contains(why, "heuristic") {
		t.Errorf("why = %q, should admit no model was used", why)
	}
}

// The heuristic is a fallback and still has to be defensible: the words that
// move a task up are the ones where a mistake is expensive.
func TestHeuristicSizesByWhatAMistakeCosts(t *testing.T) {
	for task, want := range map[string]Size{
		"rename a variable":                    SizeSolo,
		"fix a typo in the readme":             SizeSolo,
		"bump the dependency":                  SizeSolo,
		"refactor the queue":                   SizeTeam,
		"migrate the database":                 SizeTeam,
		"fix a security hole in the auth path": SizeTeam,
		"change the concurrency model":         SizeTeam,
	} {
		if got := heuristicSize(task); got != want {
			t.Errorf("heuristicSize(%q) = %q, want %q", task, got, want)
		}
	}
}

// An unrecognised task must land somewhere sensible rather than defaulting to
// the most expensive option. Sizing everything as team is how a cost control
// becomes a cost.
func TestAnUnknownTaskIsNotSizedAsTeam(t *testing.T) {
	if got := heuristicSize("do the thing with the stuff"); got == SizeTeam {
		t.Errorf("an unremarkable task was sized %q", got)
	}
}

// A model that answers is used, and the reason names it so a reader can tell a
// graded decision from a guessed one.
func TestAGradedAnswerNamesTheModelThatGaveIt(t *testing.T) {
	reg := model.NewRegistry()
	reg.Set(fixedModel{tier: model.TierCheap, name: "test-grader", text: "team"})
	o := orch(t, router.New(reg, nil))

	size, why := o.Triage(context.Background(), "rename a variable")
	if size != SizeTeam {
		t.Errorf("size = %q, want the model's answer to win over the heuristic", size)
	}
	if !strings.Contains(why, "test-grader") {
		t.Errorf("why = %q, should name the model", why)
	}
}

// A model that answers with something that is not a size must not be believed.
// The verifier exists so an unusable reply falls back rather than propagating.
func TestAnUnusableAnswerFallsBack(t *testing.T) {
	reg := model.NewRegistry()
	reg.Set(fixedModel{tier: model.TierCheap, name: "confused", text: "it depends, really"})
	o := orch(t, router.New(reg, nil))

	_, why := o.Triage(context.Background(), "rename a variable")
	if !strings.Contains(why, "heuristic") {
		t.Errorf("why = %q, an unusable grade should fall back", why)
	}
}

// fixedModel answers the same thing every time, which is all triage needs from
// a provider.
type fixedModel struct {
	tier       model.Tier
	name, text string
}

func (f fixedModel) Name() string     { return f.name }
func (f fixedModel) Model() string    { return f.name }
func (f fixedModel) Tier() model.Tier { return f.tier }
func (f fixedModel) Complete(ctx context.Context, req model.Request) (model.Response, error) {
	return model.Response{Text: f.text, Model: f.name, Tier: f.tier}, nil
}
