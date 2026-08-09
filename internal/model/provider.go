// Package model is amac's gateway to raw LLM calls.
//
// This is separate from the agent layer on purpose. Agents (Claude Code,
// Codex) are driven over ACP and do their own model calls; this package is for
// the small internal calls amac makes on its own behalf: grading a prompt,
// judging an eval, classifying an email. Those are exactly the calls that
// should run on the cheapest model that can do them, which is why the router
// lives on top of this interface rather than inside any one vendor's SDK.
package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// Tier orders models by capability and price. The router moves along this
// axis, so the ordering is load-bearing.
type Tier int

const (
	TierCheap  Tier = iota // small open models: classification, extraction
	TierMid                // mid-size: summarising, routine transformation
	TierStrong             // frontier: judgement, anything a mistake is expensive on
)

func (t Tier) String() string {
	switch t {
	case TierCheap:
		return "cheap"
	case TierMid:
		return "mid"
	default:
		return "strong"
	}
}

func ParseTier(s string) (Tier, error) {
	switch strings.ToLower(s) {
	case "cheap":
		return TierCheap, nil
	case "mid":
		return TierMid, nil
	case "strong":
		return TierStrong, nil
	}
	return 0, fmt.Errorf("unknown tier %q (cheap|mid|strong)", s)
}

type Request struct {
	System      string
	Prompt      string
	MaxTokens   int
	Temperature float64
}

type Usage struct {
	InputTokens  int
	OutputTokens int
	// CostUSD is computed from the model's published rates. It is an estimate
	// and labelled as one: only the agent adapters report authoritative cost.
	CostUSD float64
}

type Response struct {
	Text    string
	Model   string
	Tier    Tier
	Usage   Usage
	Latency time.Duration
}

type Provider interface {
	Name() string
	Model() string
	Tier() Tier
	Complete(ctx context.Context, req Request) (Response, error)
}

// ---------------------------------------------------------------- registry --

// Registry holds one provider per tier. A tier with no provider configured is
// simply absent, and the router degrades to what exists rather than failing:
// running everything on the strong model is a cost problem, not a correctness
// one, so it is the safe fallback.
type Registry struct {
	byTier map[Tier]Provider
}

func NewRegistry() *Registry { return &Registry{byTier: map[Tier]Provider{}} }

func (r *Registry) Set(p Provider) { r.byTier[p.Tier()] = p }

func (r *Registry) Get(t Tier) (Provider, bool) {
	p, ok := r.byTier[t]
	return p, ok
}

// Best returns the provider for t, or the closest stronger one available.
// Degrading upward is deliberate: if the cheap tier is unconfigured, doing the
// work correctly and expensively beats not doing it.
func (r *Registry) Best(t Tier) (Provider, bool) {
	for tt := t; tt <= TierStrong; tt++ {
		if p, ok := r.byTier[tt]; ok {
			return p, true
		}
	}
	for tt := t - 1; tt >= TierCheap; tt-- {
		if p, ok := r.byTier[tt]; ok {
			return p, true
		}
	}
	return nil, false
}

func (r *Registry) Tiers() []Tier {
	out := make([]Tier, 0, len(r.byTier))
	for t := range r.byTier {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// GMIBaseURL is GMI Cloud's OpenAI-compatible endpoint. One key there covers
// every tier, which is the cleanest way to satisfy "model agnostic": no tier
// is bound to a specific vendor's SDK, and swapping the whole stack is three
// environment variables.
const GMIBaseURL = "https://api.gmi-serving.com/v1"

// gmiDefaults are the models used when only a GMI key is supplied. Chosen for
// the shape of the work each tier does, not for benchmark scores: extraction
// wants speed, judgement wants headroom.
//
// Rates are USD per million tokens and are estimates for reporting only. Cost
// from an agent adapter is authoritative; this is not.
var gmiDefaults = map[Tier]struct {
	model   string
	in, out float64
}{
	TierCheap:  {"deepseek-ai/DeepSeek-V4-Flash", 0.14, 0.28},
	TierMid:    {"Qwen/Qwen3-235B-A22B-Instruct-2507-FP8", 0.30, 0.60},
	TierStrong: {"MoonshotAI/Kimi-K3", 0.60, 2.50},
}

// FromEnv wires providers from the environment. Missing credentials are not an
// error: amac must run with whatever is configured, and report what is not.
//
// Precedence, least specific first, so a single key gets you running and any
// tier can still be overridden individually:
//
//	GMI_API_KEY                          -> all three tiers on GMI Cloud
//	ANTHROPIC_API_KEY                    -> strong tier (overrides GMI strong)
//	AMAC_<TIER>_BASE_URL + _API_KEY      -> that tier, any OpenAI-compatible host
//	AMAC_<TIER>_MODEL                    -> that tier's model
func FromEnv() (*Registry, []string) {
	r := NewRegistry()
	var missing []string

	// 1. One GMI key fills every tier.
	if k := os.Getenv("GMI_API_KEY"); k != "" {
		for tier, d := range gmiDefaults {
			r.Set(&openAICompatible{
				base:  GMIBaseURL,
				key:   k,
				model: envOr(tierEnv(tier, "MODEL"), d.model),
				tier:  tier, rateIn: d.in, rateOut: d.out,
			})
		}
	} else {
		missing = append(missing, "GMI_API_KEY (fills all three tiers)")
	}

	// 2. Anthropic, when present, takes the strong tier. Frontier judgement is
	// the one place a closed model still earns its price.
	if k := os.Getenv("ANTHROPIC_API_KEY"); k != "" {
		r.Set(&anthropic{
			key:    k,
			model:  envOr("AMAC_STRONG_MODEL", "claude-sonnet-5"),
			rateIn: 3.0, rateOut: 15.0,
		})
	}

	// 3. Explicit per-tier configuration wins over everything.
	for _, tier := range []Tier{TierCheap, TierMid, TierStrong} {
		base, key := os.Getenv(tierEnv(tier, "BASE_URL")), os.Getenv(tierEnv(tier, "API_KEY"))
		if base == "" || key == "" {
			continue
		}
		d := gmiDefaults[tier]
		r.Set(&openAICompatible{
			base: strings.TrimRight(base, "/"), key: key,
			model: envOr(tierEnv(tier, "MODEL"), d.model),
			tier:  tier, rateIn: d.in, rateOut: d.out,
		})
	}

	for _, tier := range []Tier{TierCheap, TierMid, TierStrong} {
		if _, ok := r.Get(tier); !ok {
			missing = append(missing, fmt.Sprintf("%s tier (set GMI_API_KEY, or %s + %s)",
				tier, tierEnv(tier, "BASE_URL"), tierEnv(tier, "API_KEY")))
		}
	}
	return r, missing
}

func tierEnv(t Tier, suffix string) string {
	return "AMAC_" + strings.ToUpper(t.String()) + "_" + suffix
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// ---------------------------------------------------------------- anthropic -

type anthropic struct {
	key, model      string
	rateIn, rateOut float64 // USD per million tokens
}

func (a *anthropic) Name() string  { return "anthropic" }
func (a *anthropic) Model() string { return a.model }
func (a *anthropic) Tier() Tier    { return TierStrong }

func (a *anthropic) Complete(ctx context.Context, req Request) (Response, error) {
	body := map[string]any{
		"model":      a.model,
		"max_tokens": orDefault(req.MaxTokens, 1024),
		"messages":   []map[string]string{{"role": "user", "content": req.Prompt}},
	}
	if req.System != "" {
		body["system"] = req.System
	}
	if req.Temperature > 0 {
		body["temperature"] = req.Temperature
	}

	start := time.Now()
	var out struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	err := postJSON(ctx, "https://api.anthropic.com/v1/messages", map[string]string{
		"x-api-key":         a.key,
		"anthropic-version": "2023-06-01",
	}, body, &out)
	if err != nil {
		return Response{}, err
	}

	var sb strings.Builder
	for _, c := range out.Content {
		sb.WriteString(c.Text)
	}
	return Response{
		Text: sb.String(), Model: a.model, Tier: TierStrong, Latency: time.Since(start),
		Usage: Usage{
			InputTokens: out.Usage.InputTokens, OutputTokens: out.Usage.OutputTokens,
			CostUSD: cost(out.Usage.InputTokens, out.Usage.OutputTokens, a.rateIn, a.rateOut),
		},
	}, nil
}

// ------------------------------------------------------- openai-compatible --

// openAICompatible speaks the /chat/completions shape, which GMI Cloud,
// OpenRouter, LiteLLM, vLLM, Ollama and most open-model hosts all implement.
// One implementation covers the entire cheap tier.
type openAICompatible struct {
	base, key, model string
	tier             Tier
	rateIn, rateOut  float64
}

func (o *openAICompatible) Name() string  { return "openai-compatible:" + o.base }
func (o *openAICompatible) Model() string { return o.model }
func (o *openAICompatible) Tier() Tier    { return o.tier }

func (o *openAICompatible) Complete(ctx context.Context, req Request) (Response, error) {
	msgs := []map[string]string{}
	if req.System != "" {
		msgs = append(msgs, map[string]string{"role": "system", "content": req.System})
	}
	msgs = append(msgs, map[string]string{"role": "user", "content": req.Prompt})

	body := map[string]any{
		"model": o.model, "messages": msgs,
		"max_tokens": orDefault(req.MaxTokens, 1024),
	}
	if req.Temperature > 0 {
		body["temperature"] = req.Temperature
	}

	start := time.Now()
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	err := postJSON(ctx, o.base+"/chat/completions",
		map[string]string{"Authorization": "Bearer " + o.key}, body, &out)
	if err != nil {
		return Response{}, err
	}
	if len(out.Choices) == 0 {
		return Response{}, fmt.Errorf("%s returned no choices", o.Name())
	}
	return Response{
		Text: out.Choices[0].Message.Content, Model: o.model, Tier: o.tier, Latency: time.Since(start),
		Usage: Usage{
			InputTokens: out.Usage.PromptTokens, OutputTokens: out.Usage.CompletionTokens,
			CostUSD: cost(out.Usage.PromptTokens, out.Usage.CompletionTokens, o.rateIn, o.rateOut),
		},
	}, nil
}

// ---------------------------------------------------------------- helpers ---

func cost(in, out int, rateIn, rateOut float64) float64 {
	return (float64(in)*rateIn + float64(out)*rateOut) / 1_000_000
}

func orDefault(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

var httpClient = &http.Client{Timeout: 120 * time.Second}

func postJSON(ctx context.Context, url string, headers map[string]string, body, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	r.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		r.Header.Set(k, v)
	}

	resp, err := httpClient.Do(r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 300 {
		// Include the body: provider errors (bad model name, quota, auth) are
		// only diagnosable from it, and a bare status code sends you guessing.
		return fmt.Errorf("%s: %s: %.400s", url, resp.Status, raw)
	}
	return json.Unmarshal(raw, out)
}
