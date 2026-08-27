package health

// The roster, loaded rather than compiled.
//
// This file used to return ten Go literals: this person's repos, this person's
// launchd labels, this person's log paths. It is the single reason nobody else
// could run the part of amac that watches automations, which is most of what
// amac does when nobody is looking at it.
//
// There is deliberately no built-in fallback roster. A default list of someone
// else's automations would probe paths that do not exist and report a machine
// that is fine as broken, and an empty fallback would sweep nothing while
// reporting success. Both are worse than refusing to start, so a missing roster
// is an error that names the file and the command that writes one.

import (
	"sync"

	"github.com/lgoyal6/amac/internal/event"
)

var (
	rosterOnce sync.Once
	roster     []Automation
	rosterErr  error
)

// Roster returns the declared automations.
//
// Loaded once per process. A sweep runs every fifteen minutes from launchd, so
// each run is a fresh process and picks up an edited roster without anything
// having to watch the file.
func Roster(log *event.Log) ([]Automation, error) {
	rosterOnce.Do(func() {
		roster, rosterErr = Load(ConfigPath(), log)
	})
	return roster, rosterErr
}

// All returns the roster, or nothing if it could not be read.
//
// Only for callers that already have somewhere better to report the error, or
// for which an empty answer is harmless: Find, and the board joining Home onto
// a report it already has. Anything that sweeps must use Roster and surface the
// failure, because a sweep over an empty roster reports every automation as
// fine by never looking at one.
func All(log *event.Log) []Automation {
	list, _ := Roster(log)
	return list
}

// Find returns one declared automation by name.
func Find(log *event.Log, name string) (Automation, bool) {
	for _, a := range All(log) {
		if a.Name == name {
			return a, true
		}
	}
	return Automation{}, false
}
