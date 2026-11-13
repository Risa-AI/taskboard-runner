// Package protocol is what the runner and taskboard exchange over
// /v1/runner/*: one definition, imported by both, so the two sides cannot
// drift apart. docs/protocol.md describes every call and its answers.
//
// Every request carries the fleet's token in TokenHeader and the protocol
// version in VersionHeader. Every answer with a body is wrapped in an
// Envelope, taskboard's usual response shape.
package protocol

import "github.com/Risa-AI/taskboard-runner/pkg/apitest"

// ProtocolVersion is the version of this protocol the runner speaks. It
// changes only when a change would break the other side; taskboard answers
// 426 to a version it no longer speaks.
const ProtocolVersion = 1

const (
	// BasePath is where taskboard serves the protocol.
	BasePath = "/v1/runner"
	// TokenHeader carries the fleet token, shown once at registration.
	TokenHeader = "X-Fleet-Token"
	// VersionHeader carries ProtocolVersion on every call.
	VersionHeader = "X-Runner-Protocol"
)

// Error codes in an Envelope's error, beside taskboard's own
// (UNAUTHORIZED, NOT_FOUND, CONFLICT, VALIDATION_ERROR).
const (
	// CodeProtocolTooOld answers 426: the runner must be updated. The
	// message names the oldest runner version taskboard accepts.
	CodeProtocolTooOld = "PROTOCOL_TOO_OLD"
)

// Envelope is the body of every answer that has one.
type Envelope[T any] struct {
	Success bool   `json:"success"`
	Data    T      `json:"data,omitempty"`
	Error   *Error `json:"error,omitempty"`
}

// Error says why a call was refused, in words a person acts on.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// EnvironmentsReport is PUT /environments: which secrets this fleet holds
// for each environment. Names only, never a value.
type EnvironmentsReport []Environment

// Environment is one environment of the runner's environments file.
type Environment struct {
	Name    string   `json:"name"`
	Secrets []string `json:"secrets"`
}

// Heartbeat is POST /heartbeat, sent every 30 seconds.
type Heartbeat struct {
	// Version is the runner's release, e.g. "v0.3.0", so the fleet's page can
	// say when it is behind.
	Version   string `json:"version"`
	SlotsFree int    `json:"slots_free"`
}

// HeartbeatAnswer is the answer to a Heartbeat: the runs this fleet holds
// that were cancelled, which it stops.
type HeartbeatAnswer struct {
	RunsToCancel []string `json:"runs_to_cancel"`
}

// Job is the answer to POST /claim: one run to execute. It holds no
// secret; the runner adds the environment's secrets from its own file.
type Job struct {
	RunID       string         `json:"run_id"`
	Suite       Suite          `json:"suite"`
	Environment JobEnvironment `json:"environment"`
	// TimeoutS is how long the whole run may take, in seconds.
	TimeoutS int `json:"timeout_s"`
	// LeaseS is how long the run may go without a call about it before
	// taskboard fails it as silent, in seconds. Posting cases extends it.
	LeaseS int `json:"lease_s"`
}

// Suite is the pinned suite version a run executes.
type Suite struct {
	Files    []File        `json:"files"`
	Seeds    apitest.Seeds `json:"seeds"`
	Fixtures []Fixture     `json:"fixtures"`
}

// File is one scenario file, as its YAML text. The runner parses it with
// apitest.Parse.
type File struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

// Fixture is a file a case uploads ("@fixtures/proof.png"). URL is a
// short-lived download link; the runner fetches it into the run's own
// directory and deletes it when the run ends.
type Fixture struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// JobEnvironment is the environment a run is for, as taskboard holds it:
// its base URL and plain variables. Secrets are the runner's.
type JobEnvironment struct {
	Name      string            `json:"name"`
	BaseURL   string            `json:"base_url"`
	Variables map[string]string `json:"variables"`
}

// CaseResult is one case's outcome, posted to POST /runs/:id/cases as it
// finishes. Request and Response are already masked and cut to 2 KB.
type CaseResult struct {
	Index      int    `json:"index"`
	File       string `json:"file"`
	Line       int    `json:"line"`
	Name       string `json:"name"`
	Method     string `json:"method"`
	Path       string `json:"path"`
	Status     int    `json:"status"`
	Outcome    string `json:"outcome"`
	DurationMS int64  `json:"duration_ms"`
	Reason     string `json:"reason"`
	Request    string `json:"request"`
	Response   string `json:"response"`
}

// Summary is a whole run's count. Verdict is one of apitest's verdicts:
// passed, passed_with_skips, failed or refused.
type Summary struct {
	Verdict string `json:"verdict"`
	Total   int    `json:"total"`
	OK      int    `json:"ok"`
	Skipped int    `json:"skipped"`
	Failed  int    `json:"failed"`
	NotRun  int    `json:"not_run"`
}

// Complete is POST /runs/:id/complete, the run's last call. Error says why
// a run could not run at all (a missing secret, an unreachable target),
// masked like everything else.
type Complete struct {
	Summary Summary `json:"summary"`
	Error   string  `json:"error,omitempty"`
}
