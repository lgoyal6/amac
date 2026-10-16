// Package demo turns bounded browser samples into health reports using the
// evaluator used by AMAC's native probes.
package demo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/lgoyal6/amac/internal/health/core"
)

const maxInputBytes = 32 << 10
const maxAutomations = 20
const maxNameBytes = 120
const maxMinutes = 525600

type Request struct {
	Automations []Automation `json:"automations"`
}

type Automation struct {
	Name            string `json:"name"`
	CadenceMinutes  *int   `json:"cadence_minutes"`
	GraceMinutes    *int   `json:"grace_minutes"`
	LastDeliveryAge *int   `json:"last_delivery_age_minutes"`
	Outcome         string `json:"outcome"`
}

// Evaluate validates user-entered sample data and runs the shared evaluator.
func Evaluate(raw []byte, now time.Time) ([]core.Report, error) {
	if len(raw) == 0 || len(raw) > maxInputBytes {
		return nil, fmt.Errorf("input must be between 1 and %d bytes", maxInputBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var request Request
	if err := decoder.Decode(&request); err != nil {
		return nil, fmt.Errorf("invalid JSON input: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, errors.New("input must contain one JSON value")
	}
	if len(request.Automations) == 0 || len(request.Automations) > maxAutomations {
		return nil, fmt.Errorf("enter between 1 and %d automations", maxAutomations)
	}

	items := make([]core.Automation, 0, len(request.Automations))
	for i, input := range request.Automations {
		name := strings.TrimSpace(input.Name)
		if name == "" || len(name) > maxNameBytes {
			return nil, fmt.Errorf("automation %d needs a name of 1 to %d bytes", i+1, maxNameBytes)
		}
		if input.CadenceMinutes == nil || *input.CadenceMinutes < 1 || *input.CadenceMinutes > maxMinutes {
			return nil, fmt.Errorf("%s cadence must be 1 to %d minutes", name, maxMinutes)
		}
		if input.GraceMinutes == nil || *input.GraceMinutes < 0 || *input.GraceMinutes > maxMinutes {
			return nil, fmt.Errorf("%s grace must be 0 to %d minutes", name, maxMinutes)
		}
		if input.LastDeliveryAge != nil && (*input.LastDeliveryAge < -1440 || *input.LastDeliveryAge > maxMinutes) {
			return nil, fmt.Errorf("%s delivery age must be -1440 to %d minutes, or null", name, maxMinutes)
		}
		if input.Outcome != "healthy" && input.Outcome != "no-op" && input.Outcome != "failed" && input.Outcome != "down" && input.Outcome != "probe-error" {
			return nil, fmt.Errorf("%s outcome must be healthy, no-op, failed, down, or probe-error", name)
		}

		report, probeErr := sampleReport(input, name, now)
		items = append(items, core.Automation{
			Name:  name,
			Every: time.Duration(*input.CadenceMinutes) * time.Minute,
			Grace: time.Duration(*input.GraceMinutes) * time.Minute,
			Check: func(context.Context) (core.Report, error) { return report, probeErr },
		})
	}
	return core.SweepAt(context.Background(), items, now), nil
}

func sampleReport(input Automation, name string, now time.Time) (core.Report, error) {
	last := time.Time{}
	if input.LastDeliveryAge != nil {
		last = now.Add(-time.Duration(*input.LastDeliveryAge) * time.Minute)
	}
	if input.Outcome == "probe-error" {
		return core.Report{Detail: "Probe could not read the entered status"}, errors.New("sample probe error")
	}
	if (input.Outcome == "healthy" || input.Outcome == "no-op") && last.IsZero() {
		report := core.Report{State: core.Unknown, Detail: "No delivery time entered, so health cannot be confirmed"}
		if input.Outcome == "no-op" {
			report.Notes = []string{"This no-op did not record a delivery."}
		}
		return report, nil
	}
	state, detail := core.OK, "Latest probe reported healthy"
	var notes []string
	switch input.Outcome {
	case "no-op":
		detail = "Latest run was a no-op; no new delivery was recorded"
		notes = []string{"The last run was a no-op; its prior delivery time was retained for this check."}
	case "failed":
		state, detail = core.Failing, "Latest run failed"
	case "down":
		state, detail = core.Down, "Host is unreachable"
	}
	return core.Report{Name: name, State: state, Last: last, Detail: detail, Notes: notes}, nil
}
