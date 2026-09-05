package eval

// The harness is the thing that turns "I built a router" into a number, so a
// bug here does not produce a broken program, it produces a confident wrong
// claim. These tests exist to make the arithmetic and the gating auditable
// without a network, a key, or a bill.

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/lgoyal6/amac/internal/model"
	"github.com/lgoyal6/amac/internal/router"
)

// fake answers from a table keyed by prompt, so one tier can be right about
// some tasks and wrong about others. An arm that is uniformly right or
// uniformly wrong cannot exercise the accounting this file exists to pin.
type fake struct {
	tier    model.Tier
	answers map[string]string
	cost    float64
	calls   *[]model.Tier
}

func (f *fake) Name() string     { return "fake-" + f.tier.String() }
func (f *fake) Model() string    { return "fake-" + f.tier.String() }
func (f *fake) Tier() model.Tier { return f.tier }

func (f *fake) Complete(_ context.Context, req model.Request) (model.Response, error) {
	if f.calls != nil {
		*f.calls = append(*f.calls, f.tier)
	}
	return model.Response{
		Text: f.answers[req.Prompt], Model: f.Model(), Tier: f.tier,
		Latency: time.Millisecond,
		Usage:   model.Usage{CostUSD: f.cost},
	}, nil
}

const (
	cheapCost  = 0.0001
	strongCost = 0.01
)

// rig builds a two-tier registry and a runner over it. Mid is deliberately
// absent: the router degrades upward through it, which is the path a real
// escalation takes here.
func rig(t *testing.T, cheap, strong map[string]string) (*Runner, *[]model.Tier) {
	t.Helper()
	calls := &[]model.Tier{}
	reg := model.NewRegistry()
	reg.Set(&fake{tier: model.TierCheap, answers: cheap, cost: cheapCost, calls: calls})
	reg.Set(&fake{tier: model.TierStrong, answers: strong, cost: strongCost, calls: calls})
	return &Runner{Reg: reg, Router: router.New(reg, nil)}, calls
}

func armByName(t *testing.T, rep Report, name string) ArmSummary {
	t.Helper()
	for _, a := range rep.Arms {
		if a.Arm == name {
			return a
		}
	}
	t.Fatalf("no %q arm in report (%d arms)", name, len(rep.Arms))
	return ArmSummary{}
}

// -------------------------------------------------------------------- gate --

func TestGateNeverCarriesTheAnswerKey(t *testing.T) {
	// A plausible-looking wrong answer. Every gate that is not the answer key
	// must accept it; every grader must reject it.
	const wrong = "The capital of France is Paris."

	cases := []struct {
		name       string
		task       Task
		gateAccept bool
		realGate   bool
	}{
		{
			name:       "contains cannot be gated, only graded",
			task:       Task{Check: CheckContains, Values: []string{"replay"}},
			gateAccept: true, realGate: false,
		},
		{
			name:       "regex cannot be gated, only graded",
			task:       Task{Check: CheckRegex, Values: []string{"(?i)write[- ]ahead log"}},
			gateAccept: true, realGate: false,
		},
		{
			// The label set is what production knows: which words are legal,
			// not which one is right.
			name: "one_of with a label set is a gate production can also run",
			task: Task{Check: CheckOneOf, Values: []string{"bug"},
				Labels: []string{"bug", "feature", "question"}},
			gateAccept: false, realGate: true,
		},
		{
			// The leak. Without labels the only set available is the answer,
			// so gating on it tells the cascade to keep escalating until a
			// model says the right word. It has to degrade to non-empty, and
			// it must stop claiming to be the gate production would run.
			name:       "one_of without a label set cannot be gated",
			task:       Task{Check: CheckOneOf, Values: []string{"bug"}},
			gateAccept: true, realGate: false,
		},
		{
			name:       "json_keys is a gate production can also run",
			task:       Task{Check: CheckJSONKeys, Values: []string{"company", "role"}},
			gateAccept: false, realGate: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gateErr := c.task.Gate()("", wrong)
			if c.gateAccept && gateErr != nil {
				t.Errorf("gate rejected %q (%v); a gate that rejects it is consulting the answer key", wrong, gateErr)
			}
			if !c.gateAccept && gateErr == nil {
				t.Errorf("gate accepted %q, but production could have rejected it", wrong)
			}
			if err := c.task.Verify(wrong); err == nil {
				t.Errorf("grader accepted a wrong answer")
			}
			if got := c.task.RealGate(); got != c.realGate {
				t.Errorf("RealGate() = %v, want %v", got, c.realGate)
			}
		})
	}
}

// The regression this whole distinction exists for. Passing the router
// task.Verify made the cascade omniscient: it would escalate a wrong cheap
// answer it could not actually have detected, and the routed arm scored a
// quality it could never reproduce in production.
func TestRoutedArmIsNotFlatteredByTheAnswerKey(t *testing.T) {
	const prompt = "Summarise an append-only log in one sentence."
	task := Task{ID: "s1", Prompt: prompt, Check: CheckContains, Values: []string{"replay"}}

	runner, calls := rig(t,
		map[string]string{prompt: "Logs are useful for storing things."}, // wrong, but present
		map[string]string{prompt: "State is rebuilt by replay of the log."},
	)

	rep, err := runner.Run(context.Background(), []Task{task})
	if err != nil {
		t.Fatal(err)
	}

	routed := armByName(t, rep, "routed")
	if routed.Passed != 0 {
		t.Errorf("routed quality %.0f%%, want 0%%: a non-empty gate cannot detect a wrong summary", routed.Quality()*100)
	}
	if routed.Escalated != 0 {
		t.Errorf("escalated %d times; production had no signal to escalate on", routed.Escalated)
	}
	if got := *calls; len(got) < 1 || got[len(got)-1] != model.TierCheap {
		t.Errorf("routed arm called %v, want the cheap tier only", got)
	}
	if routed.CostUSD != cheapCost {
		t.Errorf("routed cost %v, want %v (one cheap call)", routed.CostUSD, cheapCost)
	}
	// And the arm that can actually answer it still scores.
	if s := armByName(t, rep, "strong"); s.Passed != 1 {
		t.Errorf("strong arm passed %d/%d, want 1", s.Passed, s.Total)
	}
}

func TestRoutedArmEscalatesOnAGateProductionCanRun(t *testing.T) {
	const prompt = "Classify as bug or feature: the retry loop fires after cancel."
	task := Task{ID: "c1", Prompt: prompt, Check: CheckOneOf, Values: []string{"bug"},
		Labels: []string{"bug", "feature"}}

	runner, calls := rig(t,
		map[string]string{prompt: "banana"}, // not in the label set: detectable without the key
		map[string]string{prompt: "bug"},
	)

	rep, err := runner.Run(context.Background(), []Task{task})
	if err != nil {
		t.Fatal(err)
	}

	routed := armByName(t, rep, "routed")
	if routed.Passed != 1 {
		t.Errorf("routed passed %d/%d, want 1: it should have escalated and recovered", routed.Passed, routed.Total)
	}
	if routed.Escalated != 1 {
		t.Errorf("escalated %d, want 1", routed.Escalated)
	}
	// The wasted cheap call is charged. Hiding it would overstate the saving,
	// which is the one number this harness exists to get right.
	if want := cheapCost + strongCost; routed.CostUSD != want {
		t.Errorf("routed cost %v, want %v (rejected cheap attempt plus the strong one)", routed.CostUSD, want)
	}
	seen := map[model.Tier]bool{}
	for _, c := range *calls {
		seen[c] = true
	}
	if !seen[model.TierCheap] || !seen[model.TierStrong] {
		t.Errorf("calls %v, want both tiers", *calls)
	}
}

// ---------------------------------------------------------------- accounting -

func TestQualityAndCostAreCountedPerArm(t *testing.T) {
	const p1 = "Classify as bug or feature: the retry loop fires after cancel."
	const p2 = "Classify as bug or feature: add a --json flag to status."
	tasks := []Task{
		{ID: "t1", Prompt: p1, Check: CheckOneOf, Values: []string{"bug"},
			Labels: []string{"bug", "feature"}},
		{ID: "t2", Prompt: p2, Check: CheckOneOf, Values: []string{"feature"},
			Labels: []string{"bug", "feature"}},
	}

	runner, _ := rig(t,
		map[string]string{p1: "bug", p2: "bug"}, // right once, wrong once
		map[string]string{p1: "bug", p2: "feature"},
	)

	rep, err := runner.Run(context.Background(), tasks)
	if err != nil {
		t.Fatal(err)
	}

	if got := armByName(t, rep, "cheap"); got.Passed != 1 || got.Total != 2 {
		t.Errorf("cheap arm %d/%d, want 1/2", got.Passed, got.Total)
	}
	if got := armByName(t, rep, "strong"); got.Passed != 2 || got.Total != 2 {
		t.Errorf("strong arm %d/%d, want 2/2", got.Passed, got.Total)
	}
	if got := armByName(t, rep, "cheap").CostUSD; got != 2*cheapCost {
		t.Errorf("cheap arm cost %v, want %v", got, 2*cheapCost)
	}
	// Every task is recorded on every arm, so a per-task regression is
	// findable afterwards rather than only visible as a moved average.
	if want := len(rep.Arms) * len(tasks); len(rep.Results) != want {
		t.Errorf("%d results, want %d (%d arms x %d tasks)", len(rep.Results), want, len(rep.Arms), len(tasks))
	}
	if rep.RealGates != 2 || rep.WeakGates != 0 {
		t.Errorf("gate census %d real / %d weak, want 2/0", rep.RealGates, rep.WeakGates)
	}
}

func TestQualityIsZeroForAnEmptyArm(t *testing.T) {
	if q := (ArmSummary{}).Quality(); q != 0 {
		t.Errorf("Quality() = %v on an empty arm, want 0 rather than a divide by zero", q)
	}
}

// ---------------------------------------------------------------- reporting -

func TestTableStatesSavingsAgainstTheStrongBaseline(t *testing.T) {
	rep := Report{
		Arms: []ArmSummary{
			{Arm: "cheap", Passed: 6, Total: 10, CostUSD: 0.001},
			{Arm: "strong", Passed: 9, Total: 10, CostUSD: 0.01},
			{Arm: "routed", Passed: 9, Total: 10, CostUSD: 0.004, Escalated: 3},
		},
		RealGates: 6, WeakGates: 4,
	}
	out := rep.Table()

	for _, want := range []string{
		"-90% cost, -30.0 pts quality",       // cheap: much cheaper, much worse
		"-60% cost, +0.0 pts quality (3 esc", // routed: cheaper at the same quality
		"routed gating: 6 of 10 tasks gated as production would",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q:\n%s", want, out)
		}
	}
}

// Escalation can make routing cost MORE than going straight to the strong
// model. A report that could only render a saving would hide the case the
// router most needs to be caught in.
func TestTableReportsRoutingThatCostsMore(t *testing.T) {
	rep := Report{Arms: []ArmSummary{
		{Arm: "strong", Passed: 10, Total: 10, CostUSD: 0.010},
		{Arm: "routed", Passed: 10, Total: 10, CostUSD: 0.012, Escalated: 9},
	}}
	if out := rep.Table(); !strings.Contains(out, "+20% cost") {
		t.Errorf("table did not report routing as more expensive:\n%s", out)
	}
}

func TestTableOmitsTheGateNoteWhenEveryGateIsReal(t *testing.T) {
	rep := Report{
		Arms:      []ArmSummary{{Arm: "strong", Passed: 1, Total: 1, CostUSD: 0.01}},
		RealGates: 8,
	}
	if out := rep.Table(); strings.Contains(out, "routed gating") {
		t.Errorf("table warned about gating with nothing to warn about:\n%s", out)
	}
}

// A model that could not be reached is not a model that answered badly, and a
// table that cannot tell them apart is how the strong tier spent its whole
// existence 404ing without anyone noticing. It scored 0% at $0 in 81ms, which
// is indistinguishable from a fast, free, useless model.
func TestAnArmThatNeverAnsweredHasNoQualityScore(t *testing.T) {
	rep := Report{Arms: []ArmSummary{
		{Arm: "cheap", Passed: 7, Total: 8, CostUSD: 0.0004},
		{Arm: "strong", Passed: 0, Total: 8, Errored: 8,
			ErrDetail: "404 Not Found: No matching target server found for model MoonshotAI/Kimi-K3"},
	}}
	out := rep.Table()

	if strings.Contains(out, "0.0%") {
		t.Errorf("an arm that never answered was scored as 0%% quality:\n%s", out)
	}
	for _, want := range []string{
		"strong: 8 of 8 calls returned no answer",
		"so it has no quality score",
		"No matching target server", // the actionable half
	} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q:\n%s", want, out)
		}
	}
}

// The savings line is quoted against strong. A strong arm that never ran spent
// nothing, so every other arm would show as a 100% saving against it: the
// worse the baseline is broken, the better the router looks.
func TestNoSavingsAreQuotedAgainstABrokenBaseline(t *testing.T) {
	rep := Report{Arms: []ArmSummary{
		{Arm: "cheap", Passed: 7, Total: 8, CostUSD: 0.0004},
		{Arm: "strong", Passed: 0, Total: 8, Errored: 8, ErrDetail: "404"},
		{Arm: "routed", Passed: 6, Total: 8, CostUSD: 0.0002, Escalated: 2},
	}}
	out := rep.Table()

	if strings.Contains(out, "% cost,") {
		t.Errorf("savings were quoted against a baseline that never answered:\n%s", out)
	}
	if !strings.Contains(out, "the strong baseline never answered") {
		t.Errorf("table did not say why the comparison is absent:\n%s", out)
	}
}

// Some errors are normal. An arm that mostly answered still has a quality
// worth reading, and the failures are disclosed beside it rather than hidden
// or allowed to suppress the score.
func TestPartialErrorsAreDisclosedButStillScored(t *testing.T) {
	rep := Report{Arms: []ArmSummary{
		{Arm: "strong", Passed: 8, Total: 10, CostUSD: 0.01},
		{Arm: "cheap", Passed: 5, Total: 10, Errored: 2, ErrDetail: "429 rate limited", CostUSD: 0.001},
	}}
	out := rep.Table()

	if !strings.Contains(out, "50.0%") {
		t.Errorf("an arm that mostly answered lost its score:\n%s", out)
	}
	if !strings.Contains(out, "cheap: 2 of 10 calls returned no answer") {
		t.Errorf("partial failures were not disclosed:\n%s", out)
	}
	if strings.Contains(out, "no quality score") {
		t.Errorf("an arm with a score was said to have none:\n%s", out)
	}
	// It is still comparable, because it answered.
	if !strings.Contains(out, "-90% cost") {
		t.Errorf("a partially failing arm was dropped from the comparison:\n%s", out)
	}
}

// The leak, stated as the thing that would actually have happened. A gate built
// from a single-value answer key rejects every wrong answer, so the router
// escalates until some tier produces the right word and the routed arm records
// a quality it could never reach in production, where nothing knows the answer.
func TestASingleLabelGateWouldMakeTheCascadeOmniscient(t *testing.T) {
	leaky := Task{Check: CheckOneOf, Values: []string{"bug"}}
	honest := Task{Check: CheckOneOf, Values: []string{"bug"},
		Labels: []string{"bug", "feature", "question"}}

	// "feature" is wrong, and it is exactly the mistake a cheap model makes.
	// Production cannot tell: it is a legal label, so the gate must accept it
	// and let the grader be the one to mark it wrong.
	if err := honest.Gate()("", "feature"); err != nil {
		t.Errorf("an honest gate rejected a legal label: %v", err)
	}
	if err := honest.Verify("feature"); err == nil {
		t.Error("the grader accepted a wrong label")
	}
	// Whereas the leaky one rejects it, which is the router being told it
	// guessed wrong by something production does not have.
	if err := leaky.Gate()("", "feature"); err != nil {
		t.Errorf("the fallback gate rejected a legal label, so the leak survives: %v", err)
	}

	// And it must not be advertised as the trustworthy kind, which is how the
	// report came to count precisely the leaking tasks as production-gated.
	if leaky.RealGate() {
		t.Error("a task whose gate is its answer key was reported as production-gated")
	}
}

// A label set that does not contain the answer would reject every correct
// reply, sending the cascade to the strong tier on tasks the cheap tier got
// right. That is a broken suite, not a hard one, so it is refused at load.
func TestLoadTasksRejectsALabelSetMissingItsAnswer(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tasks.json")
	if err := writeFile(path, `[{"id":"x","prompt":"p","check":"one_of",
		"values":["bug"],"labels":["feature","question"]}]`); err != nil {
		t.Fatal(err)
	}
	_, err := LoadTasks(path)
	if err == nil {
		t.Fatal("a label set that cannot contain the answer was accepted")
	}
	if !strings.Contains(err.Error(), "not among its labels") {
		t.Errorf("error %q does not say what is wrong", err)
	}
}

// ---------------------------------------------------------------- checks ----

func TestVerifyPerCheckKind(t *testing.T) {
	cases := []struct {
		name   string
		task   Task
		answer string
		ok     bool
	}{
		{"contains ignores case", Task{Check: CheckContains, Values: []string{"Replay"}}, "rebuilt by replay", true},
		{"contains needs every value", Task{Check: CheckContains, Values: []string{"replay", "append"}}, "rebuilt by replay", false},
		{"one_of tolerates trailing punctuation", Task{Check: CheckOneOf, Values: []string{"bug"}}, "Bug.", true},
		{"one_of rejects a sentence", Task{Check: CheckOneOf, Values: []string{"bug"}}, "This is a bug in the retry loop", false},
		{"regex matches", Task{Check: CheckRegex, Values: []string{"(?i)write[- ]ahead log"}}, "Write-Ahead Log", true},
		{"regex with no pattern is an error", Task{Check: CheckRegex}, "anything", false},
		{"json_keys survives a code fence", Task{Check: CheckJSONKeys, Values: []string{"host", "port"}},
			"```json\n{\"host\":\"a\",\"port\":1}\n```", true},
		{"json_keys needs the key", Task{Check: CheckJSONKeys, Values: []string{"host", "port"}}, `{"host":"a"}`, false},
		{"an unknown check never silently passes", Task{Check: "vibes"}, "anything at all", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.task.Verify(c.answer)
			if c.ok && err != nil {
				t.Errorf("Verify(%q) = %v, want pass", c.answer, err)
			}
			if !c.ok && err == nil {
				t.Errorf("Verify(%q) passed, want failure", c.answer)
			}
		})
	}
}

func TestLoadTasksRejectsAnIncompleteTask(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	if err := writeFile(path, `[{"id":"a","prompt":"hi","check":"contains","values":["x"]},{"prompt":"no id","check":"contains"}]`); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTasks(path); err == nil {
		t.Fatal("loaded a task set with a task missing an id")
	}
}

// The shipped suite is what any published number is computed over, so a
// malformed entry in it is a broken claim rather than a broken test. Checking
// it here catches that before a run spends anything.
func TestShippedTaskSetIsWellFormed(t *testing.T) {
	tasks, err := LoadTasks(filepath.Join("..", "..", "evals", "tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) == 0 {
		t.Fatal("shipped task set is empty")
	}

	seen := map[string]bool{}
	real := 0
	for _, task := range tasks {
		if seen[task.ID] {
			t.Errorf("duplicate task id %q", task.ID)
		}
		seen[task.ID] = true

		switch task.Check {
		case CheckContains, CheckOneOf, CheckJSONKeys:
			if len(task.Values) == 0 {
				t.Errorf("%s: %s check with no values", task.ID, task.Check)
			}
		case CheckRegex:
			if len(task.Values) == 0 {
				t.Errorf("%s: regex check with no pattern", task.ID)
				continue
			}
			if _, err := regexp.Compile(task.Values[0]); err != nil {
				t.Errorf("%s: pattern does not compile: %v", task.ID, err)
			}
		default:
			t.Errorf("%s: unknown check %q", task.ID, task.Check)
		}
		if task.RealGate() {
			real++
		}
	}
	// Not an assertion, a disclosure: it is the ceiling on how much of the
	// routed number the suite can actually support.
	t.Logf("%d of %d shipped tasks can be gated the way production would gate them", real, len(tasks))
}

func writeFile(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o644)
}
