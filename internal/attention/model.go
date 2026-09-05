package attention

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Serving the recommender, when there is one.
//
// Training happens in Python, where the tooling for it lives. Serving happens
// here, because the daemon is one binary that launchd starts and nothing about
// deciding whether to send a notification should depend on a virtualenv being
// intact at three in the morning. The artifact between them is a handful of
// logistic-regression weights, which is small enough to read and argue with:
// you can look at the file and see what the model believes.
//
// Everything here is designed around the model not existing, because today it
// does not. analysis/recommender.py refuses to export one until it can show it
// beats the shipped rules on data it never saw, and on the current log that is
// three labelled notifications against 1,249. No file means the rules decide,
// which is exactly what happens now.
//
// The model can only ever suppress. It runs after the rules have already said
// send, so it narrows and never widens, and a model that decides everything is
// worth sending changes nothing. That asymmetry is deliberate: a bad model
// should cost you some notifications you wanted, not deliver ones the rules had
// already ruled out.

// Recommender is the exported artifact. Features and Coef are parallel, and
// the order is the contract with the Python side.
type Recommender struct {
	Features  []string  `json:"features"`
	Coef      []float64 `json:"coef"`
	Intercept float64   `json:"intercept"`
	Threshold float64   `json:"threshold"`
}

var (
	modelOnce sync.Once
	model     *Recommender
)

// ModelPath is where the trainer writes, and is a variable so a test can point
// it somewhere else without reaching into the environment of a live machine.
var ModelPath = func() string {
	if p := os.Getenv("AMAC_RECOMMENDER"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".amac", "recommender.json")
}

// loadModel reads the artifact once per process, the same cadence as the rest
// of amac's configuration: a fresh process picks up a retrained model, and
// nothing watches a file.
//
// Every failure is the same answer, no model, because a notification system
// that stops working when a model file is malformed is worse than one that
// never had a model. It is loud in the log and silent in its behaviour.
func loadModel() *Recommender {
	modelOnce.Do(func() {
		path := ModelPath()
		if path == "" {
			return
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return // absent is the normal case, not an error
		}
		var m Recommender
		if err := json.Unmarshal(b, &m); err != nil {
			fmt.Fprintf(os.Stderr, "amac: ignoring unreadable recommender at %s: %v\n", path, err)
			return
		}
		if len(m.Features) == 0 || len(m.Features) != len(m.Coef) {
			fmt.Fprintf(os.Stderr,
				"amac: ignoring recommender at %s: %d features against %d coefficients\n",
				path, len(m.Features), len(m.Coef))
			return
		}
		model = &m
	})
	return model
}

// Score is the logistic score for one notification, given the features named
// by the artifact. A feature the artifact wants and the caller cannot supply is
// an error rather than a zero: zero is a real value for most of these, and
// scoring against a silently missing one would be a model quietly answering a
// different question.
func (m *Recommender) Score(f map[string]float64) (float64, error) {
	z := m.Intercept
	for i, name := range m.Features {
		v, ok := f[name]
		if !ok {
			return 0, fmt.Errorf("recommender wants %q and it was not supplied", name)
		}
		z += m.Coef[i] * v
	}
	return 1 / (1 + math.Exp(-z)), nil
}

// featuresFor builds what the model was trained on, from what is knowable at
// the moment of the decision. It must agree with analysis/recommender.py's
// featurise, and the artifact naming its features is what makes a disagreement
// an error here rather than a wrong answer.
func featuresFor(n Notice, turn time.Duration, since time.Duration, priorHour, globalHour int, now time.Time) map[string]float64 {
	wants := 0.0
	if n.Reason != TurnComplete {
		wants = 1
	}
	h := float64(now.Hour()) + float64(now.Minute())/60
	weekend := 0.0
	if d := now.Weekday(); d == time.Saturday || d == time.Sunday {
		weekend = 1
	}
	return map[string]float64{
		"wants_attention": wants,
		"log_turn_s":      math.Log1p(math.Max(turn.Seconds(), 0)),
		"log_since_s":     math.Log1p(math.Max(since.Seconds(), 0)),
		"prior_hour":      float64(priorHour),
		"global_hour":     float64(globalHour),
		"hour_sin":        math.Sin(2 * math.Pi * h / 24),
		"hour_cos":        math.Cos(2 * math.Pi * h / 24),
		"is_weekend":      weekend,
		"log_msg_len":     math.Log1p(float64(len(n.Message))),
	}
}
