package apitest

// Outcome is what happened to one case.
type Outcome string

const (
	OutcomeOK   Outcome = "ok"
	OutcomeSkip Outcome = "skip"
	OutcomeFail Outcome = "fail"
)

// Result is one case's outcome, in the shape the runner prints and the page
// shows. Request and Response are kept for every case that sent a request,
// passes too since 30 Oct (the page reads what a passing case checked), masked
// and cut by the Redactor before they are printed -- except a sign-in step
// that passed, whose answer is the role's token.
type Result struct {
	File       string  `json:"file"`
	Line       int     `json:"line"`
	Index      int     `json:"index"`
	Name       string  `json:"name"`
	Method     string  `json:"method"`
	Path       string  `json:"path"`
	Status     int     `json:"status,omitempty"`
	DurationMS int64   `json:"duration_ms"`
	Outcome    Outcome `json:"outcome"`
	// Reason says why a case was skipped or failed, in words a person acts on.
	Reason   string `json:"reason,omitempty"`
	Request  string `json:"request,omitempty"`
	Response string `json:"response,omitempty"`
	// Role is the case's role, or for a sign-in step the role it signs in.
	Role string `json:"role,omitempty"`
	// RanAs is the default role a case was sent as, because its own role has
	// no account on the target; it was checked against its fallback.
	RanAs string `json:"ran_as,omitempty"`
	// SignIn marks a role's login step. It is not one of the suite's cases,
	// and is counted on top of them.
	SignIn bool `json:"sign_in,omitempty"`
}

// Verdict is a run's one-word outcome.
type Verdict string

const (
	VerdictPassed Verdict = "passed"
	// VerdictPassedWithSkips is never shortened to "passed": a skipped case
	// asserted nothing, and a run of skips reported green is the failure
	// the original suite's nightly lived with for months.
	VerdictPassedWithSkips Verdict = "passed_with_skips"
	VerdictFailed          Verdict = "failed"
	// VerdictRefused: no case ran -- the target was unreachable, its build was
	// not the one under test, or the suite was invalid.
	VerdictRefused Verdict = "refused"
)

// Summary is a whole run.
type Summary struct {
	Verdict Verdict `json:"verdict"`
	Total   int     `json:"total"`
	OK      int     `json:"ok"`
	Skipped int     `json:"skipped"`
	Failed  int     `json:"failed"`
	// NotRun is the cases a run never reached: its time ran out, or it was
	// stopped. A case held back by a failure it depended on is a skip, with
	// its reason.
	NotRun     int    `json:"not_run"`
	DurationMS int64  `json:"duration_ms"`
	Build      Build  `json:"build"`
	Refusal    string `json:"refusal,omitempty"`
}

// Summarize counts a run's results. total is how many cases the suite had,
// so the cases a run never reached are counted as not run. A role's sign-in
// steps are counted on top: a failed sign-in fails the run like a case.
func Summarize(total int, results []Result, build Build) Summary {
	s := Summary{Total: total, Build: build}
	for _, r := range results {
		if r.SignIn {
			s.Total++
		}
		s.DurationMS += r.DurationMS
		switch r.Outcome {
		case OutcomeOK:
			s.OK++
		case OutcomeSkip:
			s.Skipped++
		case OutcomeFail:
			s.Failed++
		}
	}
	s.NotRun = s.Total - len(results)
	switch {
	case s.Failed > 0:
		s.Verdict = VerdictFailed
	case s.Skipped > 0:
		s.Verdict = VerdictPassedWithSkips
	default:
		s.Verdict = VerdictPassed
	}
	return s
}

// Refused is the summary of a run in which no case ran.
func Refused(total int, build Build, why string) Summary {
	return Summary{Verdict: VerdictRefused, Total: total, NotRun: total, Build: build, Refusal: why}
}
