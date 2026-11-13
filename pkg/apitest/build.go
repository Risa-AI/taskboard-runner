package apitest

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// BuildState is what the build check found.
type BuildState string

const (
	// BuildPassed: the server reported the expected build.
	BuildPassed BuildState = "passed"
	// BuildNotChecked: there was nothing to compare -- no expected build, or
	// a server that reports none. Never shown in the words a pass uses.
	BuildNotChecked BuildState = "not_checked"
	// BuildRefused: the server was built from other code; no case runs.
	BuildRefused BuildState = "refused"
)

// Build is the check's outcome, with what was compared and why.
type Build struct {
	State    BuildState `json:"state"`
	Expected string     `json:"expected,omitempty"`
	Got      string     `json:"got,omitempty"`
	Note     string     `json:"note,omitempty"`
}

// CheckBuild compares expected with the `build` field of the target's
// /health: the original suite's assertServerIsCurrent, made part of every run that
// names a build. A stale server is a correct server of the wrong vintage, and
// nothing in the scenarios can tell -- the server has to say which source it
// came from, and the run has to check.
//
// A server reporting no stamp (empty, or "dev") is not a mismatch: it is
// unknown, and the result says so rather than passing quietly.
func CheckBuild(client *http.Client, baseURL, expected string) Build {
	if expected == "" {
		return Build{State: BuildNotChecked, Note: "no expected build was given"}
	}
	b := Build{Expected: expected}
	resp, err := client.Get(baseURL + "/health")
	if err != nil {
		b.State, b.Note = BuildNotChecked, "GET /health failed: "+err.Error()
		return b
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b.State, b.Note = BuildNotChecked, fmt.Sprintf("GET /health answered %d, so the server's build is unknown", resp.StatusCode)
		return b
	}
	var body struct {
		Build string `json:"build"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		b.State, b.Note = BuildNotChecked, "GET /health is not JSON with a build field"
		return b
	}
	b.Got = body.Build
	switch {
	case body.Build == "" || body.Build == "dev":
		b.State, b.Note = BuildNotChecked, fmt.Sprintf("the server reports build %q, which carries no stamp", body.Build)
	case body.Build != expected:
		b.State = BuildRefused
		b.Note = fmt.Sprintf("the server was built from %s, and this run expects %s: every case would test other code. Rebuild or restart the server at the expected source first", body.Build, expected)
	default:
		b.State = BuildPassed
	}
	return b
}
