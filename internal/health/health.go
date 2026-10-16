// Package health probes native AMAC integrations and shares its evaluator
// with the browser lab through internal/health/core.
package health

import (
	"context"
	"time"

	"github.com/lgoyal6/amac/internal/health/core"
)

type State = core.State

const (
	OK      = core.OK
	Failing = core.Failing
	Late    = core.Late
	Down    = core.Down
	Unknown = core.Unknown
)

type Report = core.Report
type Automation = core.Automation

// Sweep evaluates the native roster using the current time.
func Sweep(ctx context.Context, list []Automation) []Report {
	return core.Sweep(ctx, list)
}

func short(d time.Duration) string { return core.Short(d) }
func itoa(n int) string            { return core.Itoa(n) }
