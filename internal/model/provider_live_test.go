package model

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// The provider code was the last thing in amac that talked to a model and had
// never been executed by a test: Complete on both providers and postJSON under
// it were at zero, so every claim about routing rested on code nobody had run.
//
// These use a real HTTP server speaking the real wire format rather than a mock
// of our own client. That proves the request we build, the response we parse
// and the cost we compute, all of which are things a stub would have let us get
// wrong in agreement with itself. What it deliberately does not prove is that a
// given vendor accepts the request, which is what the live test at the bottom
// is for.

func serve(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s.URL
}

// The OpenAI-compatible shape is what GMI, OpenRouter, LiteLLM, vLLM and Ollama
// all implement, so one implementation carries the whole cheap tier and getting
// its request wrong would be wrong everywhere at once.
func TestOpenAICompatibleSendsAndParsesTheRealShape(t *testing.T) {
	var got map[string]any
	var auth string
	base := serve(t, func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{
			"choices":[{"message":{"content":"42"}}],
			"usage":{"prompt_tokens":11,"completion_tokens":3}}`))
	})

	p := &openAICompatible{base: base, key: "sk-test", model: "test-model",
		tier: TierCheap, rateIn: 1.0, rateOut: 2.0}
	res, err := p.Complete(context.Background(), Request{
		Prompt: "what is 6 times 7", System: "answer with digits only", MaxTokens: 64,
	})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}

	if auth != "Bearer sk-test" {
		t.Errorf("authorization = %q, want a bearer token", auth)
	}
	if got["model"] != "test-model" {
		t.Errorf("model not sent: %v", got["model"])
	}
	// A system prompt has to become its own message, not be pasted onto the
	// user turn: the two are treated differently by every model behind this.
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("sent %d messages, want a system and a user turn", len(msgs))
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "answer with digits only" {
		t.Errorf("system turn wrong: %v", first)
	}

	if res.Text != "42" {
		t.Errorf("text = %q", res.Text)
	}
	if res.Usage.InputTokens != 11 || res.Usage.OutputTokens != 3 {
		t.Errorf("usage not parsed: %+v", res.Usage)
	}
	// 11 in at $1/M and 3 out at $2/M. Cost is the number the router spends
	// against, so an arithmetic slip here becomes a false saving later.
	want := 11.0/1e6*1.0 + 3.0/1e6*2.0
	if res.Usage.CostUSD < want*0.999 || res.Usage.CostUSD > want*1.001 {
		t.Errorf("cost = %v, want %v", res.Usage.CostUSD, want)
	}
	if res.Tier != TierCheap || res.Model != "test-model" {
		t.Errorf("response should carry which tier answered: %+v", res)
	}
	if res.Latency <= 0 {
		t.Error("latency must be measured; the router compares on it")
	}
}

// Anthropic's shape is different enough that sharing the parser would be wrong:
// content is a list of blocks, the key rides in x-api-key rather than a bearer,
// and the version header is mandatory.
func TestAnthropicSpeaksItsOwnShape(t *testing.T) {
	var hdr http.Header
	var body map[string]any
	base := serve(t, func(w http.ResponseWriter, r *http.Request) {
		hdr = r.Header.Clone()
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"content":[{"text":"hello "},{"text":"world"}],
			"usage":{"input_tokens":5,"output_tokens":2}}`))
	})
	p := &anthropic{key: "k", model: "claude-test", rateIn: 3, rateOut: 15, base: base}
	res, err := p.Complete(context.Background(), Request{Prompt: "hi", System: "be brief"})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if hdr.Get("x-api-key") != "k" {
		t.Errorf("key must ride in x-api-key, got %q", hdr.Get("x-api-key"))
	}
	if hdr.Get("anthropic-version") == "" {
		t.Error("the version header is mandatory and was not sent")
	}
	if body["system"] != "be brief" {
		t.Errorf("system belongs at the top level, not in messages: %v", body)
	}
	// Multiple content blocks concatenate. Taking only the first silently
	// truncates every answer long enough to be split.
	if res.Text != "hello world" {
		t.Errorf("text = %q, want both blocks joined", res.Text)
	}
}

// An HTTP error has to surface as an error. A provider that returns an empty
// string on a 429 makes the router record a cheap success and escalate nothing.
func TestProviderErrorsAreNotSilentlyEmpty(t *testing.T) {
	base := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"slow down"}}`))
	})
	p := &openAICompatible{base: base, key: "k", model: "m", tier: TierCheap}
	res, err := p.Complete(context.Background(), Request{Prompt: "x"})
	if err == nil {
		t.Fatalf("a 429 must be an error, got %+v", res)
	}
	if !strings.Contains(err.Error(), "429") && !strings.Contains(err.Error(), "slow down") {
		t.Errorf("the error should say what happened: %v", err)
	}
}

func TestProviderRespectsContextCancellation(t *testing.T) {
	base := serve(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	p := &openAICompatible{base: base, key: "k", model: "m", tier: TierCheap}
	if _, err := p.Complete(ctx, Request{Prompt: "x"}); err == nil {
		t.Error("a cancelled context must abandon the call")
	}
}

// TestLiveProvider talks to the real thing, and only when a key is present.
//
// Everything above proves the shape against a server we wrote, which cannot
// tell us that a vendor accepts it. This is the only test that can, and it is
// skipped rather than failed without a key so that the suite stays green on a
// machine that has none. It is not run in CI: a test that spends money on every
// push is a test somebody eventually deletes.
//
//	GMI_API_KEY=... go test ./internal/model/ -run TestLiveProvider -v
func TestLiveProvider(t *testing.T) {
	key := os.Getenv("GMI_API_KEY")
	if key == "" {
		t.Skip("no GMI_API_KEY; this is the only test that can prove a vendor accepts our request")
	}
	reg, missing := FromEnv()
	if len(missing) > 0 {
		t.Logf("providers not configured: %v", missing)
	}
	p, ok := reg.Best(TierCheap)
	if !ok {
		t.Skip("no cheap-tier provider configured")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := p.Complete(ctx, Request{
		System: "Reply with exactly one word and no punctuation.",
		Prompt: "What is the capital of France?",
		// Enough for a reasoning model to think and then answer. Sixteen buys
		// the thinking and nothing else, which is how this test found that
		// amac's triage budget was too small to ever get a reply.
		MaxTokens: 256,
	})
	if err != nil {
		t.Fatalf("live call to %s failed: %v", p.Name(), err)
	}
	t.Logf("%s/%s answered %q in %v for $%.6f (%d in, %d out)",
		p.Name(), p.Model(), strings.TrimSpace(res.Text), res.Latency.Round(time.Millisecond),
		res.Usage.CostUSD, res.Usage.InputTokens, res.Usage.OutputTokens)

	if strings.TrimSpace(res.Text) == "" {
		t.Error("a live provider returned nothing")
	}
	// Usage is what every cost figure in amac is built on, so a provider that
	// answers without reporting it is a provider whose spend cannot be trusted.
	if res.Usage.InputTokens == 0 && res.Usage.OutputTokens == 0 {
		t.Error("no usage reported; the spend numbers downstream would be zero")
	}
	if !strings.Contains(strings.ToLower(res.Text), "paris") {
		t.Errorf("answer %q does not contain paris; the request may be malformed", res.Text)
	}
}

// A reasoning model that spends its whole budget thinking must produce a
// diagnosable error rather than an empty string.
//
// This is the bug the live test found the first time it ran. GMI's cheap tier
// is DeepSeek-V4-Flash: at max_tokens 16 all sixteen went to reasoning tokens,
// finish_reason came back "length", and content was empty. amac returned that
// as a successful call with no text, so every caller with a tight budget would
// fall back forever while being billed for each attempt. Triage asked for 8.
func TestReasoningWithoutAnAnswerIsAnError(t *testing.T) {
	base := serve(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"choices":[{"finish_reason":"length","message":{
				"role":"assistant","content":"",
				"reasoning_content":"We are asked: the capital of France. The answer is"}}],
			"usage":{"prompt_tokens":17,"completion_tokens":16,
			         "completion_tokens_details":{"reasoning_tokens":16}}}`))
	})
	p := &openAICompatible{base: base, key: "k", model: "reasoner", tier: TierCheap}
	res, err := p.Complete(context.Background(), Request{Prompt: "x", MaxTokens: 16})
	if err == nil {
		t.Fatalf("an empty answer after a full reasoning budget was reported as success: %+v", res)
	}
	// The error has to say what to change, or the next person sees an empty
	// reply and concludes the model is broken.
	for _, want := range []string{"reason", "MaxTokens"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// A model that answers alongside its reasoning is fine, and the reasoning must
// not be mistaken for the answer.
func TestReasoningBesideAnAnswerIsNotAnError(t *testing.T) {
	base := serve(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"choices":[{"finish_reason":"stop","message":{
				"role":"assistant","content":"Paris",
				"reasoning_content":"The capital of France is Paris."}}],
			"usage":{"prompt_tokens":17,"completion_tokens":23,
			         "completion_tokens_details":{"reasoning_tokens":21}}}`))
	})
	p := &openAICompatible{base: base, key: "k", model: "reasoner", tier: TierCheap}
	res, err := p.Complete(context.Background(), Request{Prompt: "x", MaxTokens: 256})
	if err != nil {
		t.Fatalf("a model that answered was refused: %v", err)
	}
	if res.Text != "Paris" {
		t.Errorf("text = %q, want the answer rather than the reasoning", res.Text)
	}
}

// The daemon runs under launchd, which strips the environment, so a key that
// only resolves in a shell is a key the daemon never has. That is why routing
// on this machine reported "no model available" while the same key worked in a
// terminal. keyFor reads the environment first and the login keychain second,
// which is where `amac setup` already tells you to put it.
func TestKeyForPrefersTheEnvironmentThenFallsBack(t *testing.T) {
	t.Setenv("AMAC_TEST_CREDENTIAL", "from-env")
	if got := keyFor("AMAC_TEST_CREDENTIAL"); got != "from-env" {
		t.Errorf("keyFor = %q, want the environment value", got)
	}
	// A name in neither place is empty rather than an error, because a missing
	// provider is a configuration state and not a failure: amac runs without
	// one and says so.
	if got := keyFor("AMAC_NO_SUCH_CREDENTIAL_ANYWHERE"); got != "" {
		t.Errorf("keyFor on an unset name = %q, want empty", got)
	}
}

// The report of what is missing has to name both places it looked, or somebody
// who stored the key in the keychain is told to set an environment variable
// they have deliberately not set.
func TestMissingProvidersNameBothPlaces(t *testing.T) {
	t.Setenv("GMI_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	_, missing := FromEnv()
	// The primary guidance line, not the per-tier override lines, several of
	// which also name GMI_API_KEY.
	var gmi string
	for _, m := range missing {
		if strings.Contains(m, "fills all three tiers") {
			gmi = m
		}
	}
	if gmi == "" {
		t.Skip("a GMI key is configured on this machine, so nothing is missing")
	}
	if !strings.Contains(gmi, "keychain") {
		t.Errorf("%q does not mention the keychain", gmi)
	}
}
