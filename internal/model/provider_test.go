package model

import (
	"context"
	"math"
	"testing"
)

type stub struct {
	name  string
	tier  Tier
	model string
}

func (s stub) Name() string  { return s.name }
func (s stub) Model() string { return s.model }
func (s stub) Tier() Tier    { return s.tier }
func (s stub) Complete(context.Context, Request) (Response, error) {
	return Response{Text: s.name}, nil
}

func TestParseTier(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Tier
	}{{"cheap", TierCheap}, {"mid", TierMid}, {"strong", TierStrong}} {
		got, err := ParseTier(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("ParseTier(%q) = %v, %v", tc.in, got, err)
		}
		// Round trip, so a tier written to the log parses back to itself.
		if got.String() != tc.in {
			t.Errorf("%v.String() = %q, want %q", got, got.String(), tc.in)
		}
	}
	if _, err := ParseTier("enormous"); err == nil {
		t.Error("an unknown tier must be refused, not defaulted")
	}
}

// Degrading upward is deliberate: if the cheap tier is unconfigured, doing the
// work correctly and expensively beats not doing it. Degrading downward
// silently would answer a hard question with a weak model, which is the failure
// nobody notices.
func TestBestPrefersStrongerWhenATierIsMissing(t *testing.T) {
	r := NewRegistry()
	r.Set(stub{name: "strong", tier: TierStrong})

	for _, want := range []Tier{TierCheap, TierMid, TierStrong} {
		p, ok := r.Best(want)
		if !ok || p.Name() != "strong" {
			t.Errorf("Best(%v) = %v, %v", want, p, ok)
		}
	}

	// With the cheap tier present, a cheap request takes it rather than
	// reaching upward for no reason.
	r.Set(stub{name: "cheap", tier: TierCheap})
	if p, _ := r.Best(TierCheap); p.Name() != "cheap" {
		t.Errorf("Best(cheap) = %s, want cheap", p.Name())
	}
	// And a strong request does not fall down to it.
	if p, _ := r.Best(TierStrong); p.Name() != "strong" {
		t.Errorf("Best(strong) = %s, want strong", p.Name())
	}
}

// Only when nothing stronger exists does it fall back downward, because a weak
// answer still beats no answer at all.
func TestBestFallsDownwardOnlyAsALastResort(t *testing.T) {
	r := NewRegistry()
	r.Set(stub{name: "cheap", tier: TierCheap})

	p, ok := r.Best(TierStrong)
	if !ok || p.Name() != "cheap" {
		t.Fatalf("with only a cheap tier, Best(strong) = %v, %v", p, ok)
	}
}

func TestBestOnAnEmptyRegistry(t *testing.T) {
	if _, ok := NewRegistry().Best(TierMid); ok {
		t.Fatal("an empty registry must not claim to have a provider")
	}
}

func TestTiersAreSorted(t *testing.T) {
	r := NewRegistry()
	r.Set(stub{tier: TierStrong})
	r.Set(stub{tier: TierCheap})
	r.Set(stub{tier: TierMid})

	got := r.Tiers()
	for i := 1; i < len(got); i++ {
		if got[i-1] >= got[i] {
			t.Fatalf("Tiers() is not ascending: %v", got)
		}
	}
}

// Setting a tier twice replaces it. An override in the environment has to win
// over the default rather than being ignored because a default got there first.
func TestSetReplaces(t *testing.T) {
	r := NewRegistry()
	r.Set(stub{name: "first", tier: TierMid})
	r.Set(stub{name: "second", tier: TierMid})
	if p, _ := r.Get(TierMid); p.Name() != "second" {
		t.Fatalf("got %s, want the later one", p.Name())
	}
}

// Rates are USD per million tokens. Getting the scale wrong here makes every
// cost figure in the system wrong by a factor of a million, in a direction
// nobody would question because the number would simply look small.
func TestCostIsPerMillionTokens(t *testing.T) {
	// A million in, a million out, at $1 and $2: three dollars.
	if got := cost(1_000_000, 1_000_000, 1, 2); !close(got, 3) {
		t.Errorf("cost = %v, want 3", got)
	}
	// A thousand input tokens at $3/Mtok is three tenths of a cent.
	if got := cost(1000, 0, 3, 15); !close(got, 0.003) {
		t.Errorf("cost = %v, want 0.003", got)
	}
	if got := cost(0, 0, 3, 15); got != 0 {
		t.Errorf("no tokens must cost nothing, got %v", got)
	}
	// Output is priced separately and usually higher; a function that summed
	// them at one rate would understate every real call.
	in := cost(1000, 0, 3, 15)
	out := cost(0, 1000, 3, 15)
	if out <= in {
		t.Errorf("output (%v) should cost more than input (%v) at these rates", out, in)
	}
}

func TestOrDefault(t *testing.T) {
	if got := orDefault(0, 42); got != 42 {
		t.Errorf("zero should take the default, got %d", got)
	}
	if got := orDefault(-1, 42); got != 42 {
		t.Errorf("a negative should take the default, got %d", got)
	}
	if got := orDefault(7, 42); got != 7 {
		t.Errorf("a real value should survive, got %d", got)
	}
}

// One GMI key fills every tier, which is what "model agnostic" means here: no
// tier is bound to a vendor's SDK.
func TestOneKeyFillsEveryTier(t *testing.T) {
	t.Setenv("GMI_API_KEY", "k")
	t.Setenv("ANTHROPIC_API_KEY", "")
	r, missing := FromEnv()

	if got := len(r.Tiers()); got != 3 {
		t.Fatalf("one key configured %d tiers, want 3", got)
	}
	for _, tier := range r.Tiers() {
		p, _ := r.Get(tier)
		if p.Model() == "" {
			t.Errorf("%v has no model set", tier)
		}
	}
	if len(missing) != 0 {
		t.Errorf("nothing should be missing, got %v", missing)
	}
}

// Anthropic takes the strong tier when present. Frontier judgement is the one
// place a closed model still earns its price.
func TestAnthropicTakesTheStrongTier(t *testing.T) {
	t.Setenv("GMI_API_KEY", "k")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-x")
	r, _ := FromEnv()

	p, ok := r.Get(TierStrong)
	if !ok || p.Name() != "anthropic" {
		t.Fatalf("strong tier is %v, want anthropic", p)
	}
	// And it does not take the others.
	if p, _ := r.Get(TierCheap); p.Name() == "anthropic" {
		t.Error("anthropic should not have taken the cheap tier")
	}
}

// With nothing configured, the caller is told exactly what to set rather than
// getting an empty registry and a confusing failure three calls later.
func TestNothingConfiguredSaysWhatIsMissing(t *testing.T) {
	noKeychain(t)
	for _, k := range []string{"GMI_API_KEY", "ANTHROPIC_API_KEY",
		"AMAC_CHEAP_API_KEY", "AMAC_MID_API_KEY", "AMAC_STRONG_API_KEY"} {
		t.Setenv(k, "")
	}
	r, missing := FromEnv()
	if len(r.Tiers()) != 0 {
		t.Fatalf("nothing is configured but %d tiers exist", len(r.Tiers()))
	}
	if len(missing) == 0 {
		t.Fatal("an unconfigured registry must say what is missing")
	}
	var namesGMI bool
	for _, m := range missing {
		if contains(m, "GMI_API_KEY") {
			namesGMI = true
		}
	}
	if !namesGMI {
		t.Errorf("the advice should name the one key that fills everything: %v", missing)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

func close(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// noKeychain makes the login keychain answer nothing for one test.
//
// Clearing the environment used to be the whole of "nothing is configured".
// Once keyFor started reading the keychain, it stopped being: on the machine
// where the key is actually stored, these assertions describe a state that
// machine is not in. Without a seam the choice is a test that fails there or
// one that skips there, and skipping is worse, because the machine that skips
// is the only one where the configuration is real.
func noKeychain(t *testing.T) {
	t.Helper()
	prev := keychain
	keychain = func(string) string { return "" }
	t.Cleanup(func() { keychain = prev })
}
