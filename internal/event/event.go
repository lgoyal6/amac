// Package event defines amac's append-only event log.
//
// Every subsystem publishes here and reads from here: the supervisor records
// session lifecycle, the router records its decisions and their cost, sensors
// record what they observed, actuators record what they did. Nothing queries
// another subsystem directly.
//
// That constraint is what makes the rest tractable. The dashboard is a view
// over this log, automations are subscribers to it, the workflow miner is a
// query against it, and "why did it do that" is answerable by replay instead
// of by guessing.
package event

import (
	"encoding/json"
	"time"
)

type Kind string

const (
	// Session lifecycle, sourced from ACP rather than from screen scraping.
	KindSessionStarted Kind = "session.started"
	KindSessionEnded   Kind = "session.ended"
	KindSessionUpdate  Kind = "session.update"

	// Agent asked for something and is now waiting on a human.
	KindPermissionRequested Kind = "permission.requested"
	KindPermissionAnswered  Kind = "permission.answered"

	// Router decisions, and what they cost. Paired so savings are measurable
	// rather than asserted.
	KindRouteDecided Kind = "route.decided"
	KindModelCall    Kind = "model.call"

	// Anything that changed the world outside amac. Every one is recorded so
	// the audit log and kill switch have something to work with.
	KindActuation Kind = "actuation"

	// What you were doing. Metadata only, and only for allowlisted apps.
	KindObservation Kind = "observation"

	// A job application, from the browser extension or a confirmation email.
	KindApplication Kind = "application"

	// One health sweep over the declared automations. Carries every report,
	// not just the failures, so "it was fine an hour ago" is answerable by
	// reading the log rather than by trusting that no alert means no problem.
	KindAutomationCheck Kind = "automation.check"

	// An agent session wants the human. Recorded whether or not it was
	// delivered, with the reason it was held back, so a silent stretch can be
	// told apart from a stretch where nothing happened.
	KindAttention Kind = "attention"

	// The board was opened by somebody with a token.
	//
	// Recorded because amac knows what it sent and has never known what
	// happened next, and that gap is not academic: the obvious way to score a
	// notification, "did the session it named change state soon after", turned
	// out to measure how much telemetry an adapter emits rather than anything
	// the human did. It correlated 0.64 with session chattiness, so a model
	// trained on it learns which CLI reports state. See analysis/README.md.
	//
	// This is the raw fact and nothing more. Whether an open counts as a
	// response to a particular notification is a question with a window in it,
	// and a window baked into the log is a decision nobody can revisit; the
	// join belongs in the analysis, where it can be argued with.
	KindBoardOpened Kind = "board.opened"

	// A deliberate act by a person, as opposed to a page load or an agent.
	//
	// board.opened alone turned out to be a label that barely accumulates.
	// Over the four weeks before this was written amac delivered a median of
	// 111 notifications a day and could observe roughly two human actions a
	// day in total, so two hundred labelled examples were about a hundred days
	// away. Not because a person does little, but because amac recorded almost
	// none of what they do.
	//
	// The test for what belongs here is whether an agent could have caused it.
	// Reads are excluded, because the board polls: a GET is the page doing its
	// job, not a decision. Every write through the authenticated API is a
	// decision, and so is following a signed link out of a notification, which
	// is the strongest one available because it names the notification it
	// answers.
	//
	// As with board.opened this is the raw fact. Whether an action counts as a
	// response to a particular notification has a window in it, and a window
	// baked into the log is a decision nobody can revisit.
	KindHumanActed Kind = "human.acted"

	// What a session amac does not own is doing, as reported by the agent's
	// own hooks. Separate from session.update, which carries ACP traffic for
	// sessions amac started: this is the only state available for the twenty
	// tmux sessions it merely watches, and it is written only when the state
	// changes so a busy session does not bury the log in its own heartbeat.
	KindSessionState Kind = "session.state"

	// One individual execution of an automation, reported exactly once
	// whatever happened after it. Separate from automation.check, which is a
	// verdict on the newest state and deliberately says nothing about a
	// failure that a later run recovered from.
	KindAutomationRun Kind = "automation.run"

	// One evaluation run: the cost/quality curve, the arms it was measured
	// over, and the models underneath. Recorded because the curve is the claim
	// the router rests on, and a claim that only exists in terminal scrollback
	// cannot be compared against the next one when the models change.
	KindEvalCompleted Kind = "eval.completed"

	// A job somewhere else saying it ran. The only inbound signal here: every
	// other probe reads an artifact, which is stronger and only possible for
	// things amac can reach. This is for the ones it cannot.
	KindHeartbeat Kind = "heartbeat"

	// Diagnostics from amac itself.
	KindDaemon Kind = "daemon"

	// A crew run hidden from the operational board. The original run events
	// remain append-only and auditable; archiving only changes the current view.
	KindCrewArchived Kind = "crew.archived"
)

// Event is the single record type. Seq is assigned by the store and is the
// total order; nothing else in the system may invent one.
//
// Payload stays opaque JSON on purpose. A typed column per subsystem would
// mean a migration every time a subsystem learns a new fact, and the log has
// to outlive every schema above it.
type Event struct {
	Seq     int64           `json:"seq"`
	At      time.Time       `json:"at"`
	Kind    Kind            `json:"kind"`
	Source  string          `json:"source"`            // subsystem: supervisor, router, observer, ...
	Session string          `json:"session,omitempty"` // amac session id, when scoped to one
	Payload json.RawMessage `json:"payload,omitempty"`
}

func New(kind Kind, source, session string, payload any) (Event, error) {
	var raw json.RawMessage
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return Event{}, err
		}
		raw = b
	}
	return Event{At: time.Now().UTC(), Kind: kind, Source: source, Session: session, Payload: raw}, nil
}
