package health

// The declared roster, as data.
//
// Every automation here used to be a Go constant: this person's repos, this
// person's launchd labels, this person's log paths, compiled in. That is fine
// for one machine and it is the whole reason nobody else can run the health
// subsystem, which is most of what amac is for.
//
// So the roster moves into a file and the probes become shapes. The shapes are
// real rather than forced: four of the ten are a launchd job with a completion
// marker in a log, and they already shared an implementation before this change
// made it visible. Two read a JSON artifact for a timestamp. One reads the
// newest file in a directory. One is a liveness check on a service.
//
// What deliberately did not generalise is how a specific pipeline reports
// itself. morning-brief commits a marker with a date and no time; hacklist
// encodes timestamps in filenames; n8n has its own API. Those interpretations
// stay in code and take their repo or workflow as a parameter, because an
// abstraction that flattened them would be a worse description of the world
// than three functions are.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/lgoyal6/amac/internal/event"
)

// ConfigPath is where the roster lives.
func ConfigPath() string {
	if p := os.Getenv("AMAC_HEALTH_CONFIG"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".amac", "health.json")
}

// Declaration is one automation as written down.
//
// Every and Grace are strings here and durations everywhere else, because
// "24h" in a config file is worth the parse: the alternative is a number whose
// unit lives in a comment.
type Declaration struct {
	Name     string         `json:"name"`
	What     string         `json:"what"`
	Every    string         `json:"every"`
	Grace    string         `json:"grace"`
	Schedule string         `json:"schedule,omitempty"`
	Host     string         `json:"host,omitempty"`
	Category string         `json:"category,omitempty"`
	Home     string         `json:"home,omitempty"`
	Probe    string         `json:"probe"`
	With     map[string]any `json:"with,omitempty"`
}

type Config struct {
	Automations []Declaration `json:"automations"`
}

// Declarations reads the intended roster without constructing its probes.
// The board uses this to explain what should happen even before the first
// sweep has run. It is deliberately uncached so editing health.json is visible
// on the next refresh without restarting the daemon.
func Declarations(path string) ([]Declaration, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, ErrNoConfig{Path: path}
	}
	if err != nil {
		return nil, err
	}
	var cfg Config
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(cfg.Automations) == 0 {
		return nil, fmt.Errorf("%s declares no automations", path)
	}
	return cfg.Automations, nil
}

// ErrNoConfig is returned when the roster has never been written. It is a
// distinct error so the CLI can say "run amac init" rather than printing a
// bare file-not-found at someone who has just cloned the repo.
type ErrNoConfig struct{ Path string }

func (e ErrNoConfig) Error() string {
	return "no health roster at " + e.Path + " (run `amac init`)"
}

// probeKinds maps a name in the config to the shape that reads it.
//
// Adding a kind is adding a line here. An unknown kind is a hard error naming
// the valid ones, never a skipped automation: an automation silently dropped
// from the roster is an automation nobody is watching, which is the one outcome
// this package exists to prevent.
// kinds are built per load rather than held in a package variable, because one
// of them needs the event log and reaching for a global to avoid threading it
// would be trading a parameter for a thing nobody can see.
func kinds(log *event.Log) map[string]probeMaker {
	return map[string]probeMaker{
		"launchd_marker":       newLaunchdMarker,
		"systemd_unit":         newSystemdUnit,
		"service":              newService,
		"marker_fields":        newMarkerFields,
		"spend_snapshot":       newSpendSnapshot,
		"github_delivery_file": newGitHubDeliveryFile,
		"github_newest_file":   newGitHubNewestFile,
		"n8n":                  newN8N,
		"heartbeat":            newHeartbeat(log),
	}
}

// probeMaker turns one declaration into the check that reads it. It returns an
// error rather than a check when the declaration is unusable, so the roster
// fails to load instead of failing at sweep time.
type probeMaker func(Declaration) (func(context.Context) (Report, error), error)

func kindNames(probes map[string]probeMaker) string {
	names := make([]string, 0, len(probes))
	for k := range probes {
		names = append(names, k)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// Load reads and validates the roster.
//
// Validation is strict and complete: it reports every problem it finds rather
// than the first, because someone editing this file by hand should not have to
// run the command five times to learn about five typos.
func Load(path string, log *event.Log) ([]Automation, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, ErrNoConfig{Path: path}
	}
	if err != nil {
		return nil, err
	}

	var cfg Config
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(cfg.Automations) == 0 {
		return nil, fmt.Errorf("%s declares no automations", path)
	}

	probes := kinds(log)
	var problems []string
	seen := map[string]bool{}
	out := make([]Automation, 0, len(cfg.Automations))

	for i, d := range cfg.Automations {
		where := fmt.Sprintf("automations[%d]", i)
		if d.Name == "" {
			problems = append(problems, where+": no name")
			continue
		}
		where = d.Name
		if seen[d.Name] {
			problems = append(problems, where+": declared twice")
			continue
		}
		seen[d.Name] = true

		category := d.Category
		if category == "" {
			category = "automation"
		}
		if category != "automation" && category != "machine" {
			problems = append(problems, where+": category must be automation or machine")
		}
		a := Automation{Name: d.Name, What: d.What, Home: expand(d.Home), Category: category}

		// Cadence may be empty for a service and for nothing else.
		//
		// A service is either up or it is not, and declaring a fake cadence for
		// one would be declaring a fake delivery. Everything else must say how
		// often it is expected to deliver, because that declaration is the only
		// thing that makes silence detectable: nothing pushes an event when a
		// cron fails to fire, so an automation with no cadence can go dark
		// forever and every sweep will keep reporting it fine.
		//
		// This was documented as required and never enforced, so a roster could
		// quietly contain an automation nothing would ever call late.
		if d.Every == "" {
			if d.Probe != "service" && category != "machine" {
				problems = append(problems, where+
					": every is required, or its silence can never be detected"+
					" (only a continuous service may omit it)")
			}
		} else {
			if a.Every, err = time.ParseDuration(d.Every); err != nil {
				problems = append(problems, where+": every "+err.Error())
			}
		}
		if d.Grace != "" {
			if a.Grace, err = time.ParseDuration(d.Grace); err != nil {
				problems = append(problems, where+": grace "+err.Error())
			}
		}

		mk, ok := probes[d.Probe]
		if !ok {
			problems = append(problems, fmt.Sprintf("%s: unknown probe %q (have: %s)", where, d.Probe, kindNames(probes)))
			continue
		}
		check, err := mk(d)
		if err != nil {
			problems = append(problems, where+": "+err.Error())
			continue
		}
		a.Check = check
		out = append(out, a)
	}

	if len(problems) > 0 {
		return nil, fmt.Errorf("%s:\n  %s", path, strings.Join(problems, "\n  "))
	}
	return out, nil
}

// expand resolves a leading ~ so a roster can be written portably and still
// name a home directory.
func expand(p string) string {
	if p == "" {
		return ""
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// ---------------------------------------------------------------- params ----

// params reads a declaration's `with` block with the errors collected rather
// than panicked, so a bad roster produces a list of problems and not a stack
// trace.
type params struct {
	name string
	with map[string]any
	errs []string
}

func (p *params) str(key string, required bool) string {
	v, ok := p.with[key]
	if !ok {
		if required {
			p.errs = append(p.errs, "missing "+key)
		}
		return ""
	}
	s, ok := v.(string)
	if !ok {
		p.errs = append(p.errs, key+" must be a string")
		return ""
	}
	return s
}

func (p *params) path(key string, required bool) string { return expand(p.str(key, required)) }

func (p *params) num(key string, def float64) float64 {
	v, ok := p.with[key]
	if !ok {
		return def
	}
	f, ok := v.(float64) // encoding/json gives every number as float64
	if !ok {
		p.errs = append(p.errs, key+" must be a number")
		return def
	}
	return f
}

func (p *params) err() error {
	if len(p.errs) == 0 {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(p.errs, "; "))
}

func paramsOf(d Declaration) *params {
	w := d.With
	if w == nil {
		w = map[string]any{}
	}
	return &params{name: d.Name, with: w}
}

// withOf reads one parameter of one declared automation.
//
// Per-run reporting needs the same repo and workflow the state probes use, and
// threading them through six call sites would be threading the roster itself.
// An empty answer means the automation is not declared, which the caller
// already treats as "nothing to report from it".
func withOf(name, key string) string {
	b, err := os.ReadFile(ConfigPath())
	if err != nil {
		return ""
	}
	var cfg Config
	if json.Unmarshal(b, &cfg) != nil {
		return ""
	}
	for _, d := range cfg.Automations {
		if d.Name != name {
			continue
		}
		if s, ok := d.With[key].(string); ok {
			return s
		}
	}
	return ""
}
