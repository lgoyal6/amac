package attention

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lgoyal6/amac/internal/event"
)

func writeModel(t *testing.T, m Recommender) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "recommender.json")
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// useModel points the loader at one artifact for one test. The loader caches
// per process like the rest of amac's configuration, so the cache is reset
// around it rather than worked around.
func useModel(t *testing.T, path string) {
	t.Helper()
	oldPath, oldModel := ModelPath, model
	ModelPath = func() string { return path }
	// Reset in the test rather than behind a helper in the package: a
	// production function that exists only for a test is dead code, and the
	// dead-code gate says so.
	model, modelOnce = nil, sync.Once{}
	t.Cleanup(func() {
		ModelPath, model = oldPath, oldModel
		modelOnce = sync.Once{}
	})
}

// The whole design rests on this: no model is the normal case, and it means the
// rules stand. Today the trainer refuses to export one at all.
func TestWithNoModelTheRulesDecide(t *testing.T) {
	l := quietLog(t)
	useModel(t, filepath.Join(t.TempDir(), "absent.json"))
	addAt(t, l, event.KindSessionState, "am-claude-1", 90*time.Minute)

	out := decide(context.Background(), l, Notice{
		Session: "am-claude-1", Agent: "claude", Reason: TurnComplete})
	if !out.Sent {
		t.Errorf("an absent model suppressed a notification: %s", out.Why)
	}
}

// The model narrows and never widens. It runs after the rules have said send,
// so a model that likes everything changes nothing, and a model that likes
// nothing cannot resurrect what the rules already refused.
func TestTheModelCanOnlyWithdrawWhatTheRulesAllowed(t *testing.T) {
	l := quietLog(t)
	// Refuses everything: a large negative intercept and a threshold above any
	// possible score.
	useModel(t, writeModel(t, Recommender{
		Features: []string{"wants_attention"}, Coef: []float64{0}, Intercept: -20, Threshold: 0.9,
	}))

	// A short turn was already refused by the rules, and the reason must stay
	// the rules' reason rather than being attributed to the model.
	addAt(t, l, event.KindSessionState, "am-claude-1", 20*time.Second)
	out := decide(context.Background(), l, Notice{
		Session: "am-claude-1", Agent: "claude", Reason: TurnComplete})
	if out.Sent {
		t.Fatal("a short turn was sent")
	}
	if want := "worth interrupting for"; !strings.Contains(out.Why, want) {
		t.Errorf("why = %q, want the rule's reason, not the model's", out.Why)
	}

	// A long turn the rules allowed is the one the model may withdraw.
	addAt(t, l, event.KindSessionState, "am-claude-2", 90*time.Minute)
	out = decide(context.Background(), l, Notice{
		Session: "am-claude-2", Agent: "claude", Reason: TurnComplete})
	if out.Sent {
		t.Error("the model refused everything and a notification still went out")
	}
	if !strings.Contains(out.Why, "recommender scored") {
		t.Errorf("why = %q, which does not say the model decided", out.Why)
	}
}

// A model that cannot be scored must not be able to silence a notification by
// being broken. That is the one way a serving path turns a training mistake
// into missed alerts.
func TestABrokenModelIsNoOpinionRatherThanSilence(t *testing.T) {
	l := quietLog(t)
	addAt(t, l, event.KindSessionState, "am-claude-1", 90*time.Minute)
	n := Notice{Session: "am-claude-1", Agent: "claude", Reason: TurnComplete}

	for _, tc := range []struct {
		name string
		path string
	}{
		{"not json", func() string {
			p := filepath.Join(t.TempDir(), "m.json")
			os.WriteFile(p, []byte("{not json"), 0o600)
			return p
		}()},
		{"features and coefficients disagree", writeModel(t, Recommender{
			Features: []string{"wants_attention", "log_turn_s"},
			Coef:     []float64{1}, Threshold: 0.9,
		})},
		{"wants a feature nothing supplies", writeModel(t, Recommender{
			Features: []string{"phase_of_moon"}, Coef: []float64{1},
			Intercept: -20, Threshold: 0.9,
		})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useModel(t, tc.path)
			if out := decide(context.Background(), l, n); !out.Sent {
				t.Errorf("a broken model silenced a notification: %s", out.Why)
			}
		})
	}
}

// The artifact is weights, so the score has to be the logistic function of
// them. Checked against a hand-computed value rather than against itself.
func TestScoreIsTheLogisticOfTheWeights(t *testing.T) {
	m := Recommender{
		Features:  []string{"a", "b"},
		Coef:      []float64{0.5, -1.5},
		Intercept: 0.25,
	}
	got, err := m.Score(map[string]float64{"a": 2, "b": 1})
	if err != nil {
		t.Fatal(err)
	}
	z := 0.25 + 0.5*2 + -1.5*1 // -0.25
	want := 1 / (1 + math.Exp(-z))
	if math.Abs(got-want) > 1e-12 {
		t.Errorf("score = %v, want %v", got, want)
	}

	// A missing feature is an error, not a zero. Zero is a real value for most
	// of these, so scoring without one is a model quietly answering a
	// different question.
	if _, err := m.Score(map[string]float64{"a": 2}); err == nil {
		t.Error("a missing feature scored anyway")
	}
}

// The Go side has to build every feature the Python side names, or the artifact
// can only ever fail to score. This is the contract between them, checked here
// because a mismatch is otherwise found in production by a notification that
// silently stops being ranked.
func TestEveryTrainedFeatureCanBeBuiltHere(t *testing.T) {
	trained := []string{
		"wants_attention", "log_turn_s", "log_since_s", "prior_hour",
		"global_hour", "hour_sin", "hour_cos", "is_weekend", "log_msg_len",
	}
	got := featuresFor(Notice{Reason: TurnComplete, Message: "hello"},
		time.Minute, time.Hour, 2, 40, time.Now())
	for _, name := range trained {
		if _, ok := got[name]; !ok {
			t.Errorf("analysis/recommender.py trains on %q and nothing here supplies it", name)
		}
	}
	if len(got) != len(trained) {
		t.Errorf("%d features built against %d trained; the two sides have drifted",
			len(got), len(trained))
	}
}

// The contract between the two halves, as an artifact rather than a promise.
//
// testdata/recommender.json was produced by analysis/recommender.py's own
// export path, from a model it trained and its gate passed. If the Python side
// changes what it writes, this stops loading here, which is the failure that
// otherwise shows up in production as a model silently never being consulted.
func TestAnArtifactPythonWroteLoadsAndScores(t *testing.T) {
	useModel(t, filepath.Join("testdata", "recommender.json"))
	m := loadModel()
	if m == nil {
		t.Fatal("the exported artifact did not load; the two sides have drifted")
	}
	if len(m.Features) != len(m.Coef) {
		t.Fatalf("%d features against %d coefficients", len(m.Features), len(m.Coef))
	}
	if m.Threshold <= 0 || m.Threshold >= 1 {
		t.Errorf("threshold %v is not a probability", m.Threshold)
	}

	// Every feature it names has to be one this package builds, or it can only
	// ever fail to score.
	built := featuresFor(Notice{Reason: TurnComplete, Message: "hello"},
		time.Minute, time.Hour, 1, 10, time.Now())
	score, err := m.Score(built)
	if err != nil {
		t.Fatalf("scoring the exported artifact failed: %v", err)
	}
	if score < 0 || score > 1 {
		t.Errorf("score %v is not a probability", score)
	}

	// And it has to have an opinion rather than being flat. This model was
	// trained to like a finished turn that said something over one that did
	// not, so the two must not score the same.
	terse := featuresFor(Notice{Reason: TurnComplete, Message: "ok"},
		time.Minute, time.Hour, 1, 10, time.Now())
	wordy := featuresFor(Notice{Reason: TurnComplete, Message: string(make([]byte, 4000))},
		time.Minute, time.Hour, 1, 10, time.Now())
	a, _ := m.Score(terse)
	b, _ := m.Score(wordy)
	if b <= a {
		t.Errorf("the model scored a terse turn %v and a wordy one %v; it learned nothing", a, b)
	}
}
