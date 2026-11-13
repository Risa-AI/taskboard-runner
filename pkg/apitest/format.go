// Package apitest reads, checks and runs API test scenarios.
//
// It is copied whole from Risa's src/apitest at commit 57380f3f (the last
// change to that package, on Risa's main at d2f49305), so a suite runs the
// same in both products. Only comments and test names were changed: they no
// longer name the projects the rules were learned on.
//
// The format is the one an end-to-end suite of several hundred cases was
// written in, field for field, because each of its rules was learned from a
// bug. What is new is only what a runner serving many projects needs: the
// variables a project seeds are data (seeds.go) rather than Go code, every
// suite is checked before it runs (validate.go), and what leaves the runner
// is redacted (result.go).
package apitest

// Case is one step of a scenario. The fields are the original suite's RouteCase; the
// comments say what each one does there.
type Case struct {
	// Name is shown in the result: what the case checks.
	Name string `yaml:"name"`
	// Method and Path are the request; ${var} is substituted in the path.
	Method string `yaml:"method"`
	Path   string `yaml:"path"`
	// Auth is "none" (the default), or "bearer", which sends
	// "Authorization: Bearer ${token}". "owner" and "barber" are the original suite's
	// names for the same thing, accepted so its suites import unchanged; they
	// are not roles the runner knows. A case juggling identities names its
	// token in Headers instead.
	Auth string `yaml:"auth"`
	// Sign signs the request at send time, for an API that verifies a
	// signature a file cannot hold: it covers the moment it is sent.
	Sign *Sign `yaml:"sign"`
	// Request is a JSON body. ${var} is substituted in every string leaf, and a
	// leaf that is exactly one number variable stays a number.
	Request interface{} `yaml:"request"`
	// Form is a multipart body; a value starting with "@" is a fixture file.
	// Never with Request.
	Form map[string]string `yaml:"form"`
	// ExpectStatus is the expected status code; 0 checks nothing.
	ExpectStatus int `yaml:"expect_status"`
	// Capture stores response values into variables for later cases:
	// name -> dotted path, where a number segment indexes an array.
	Capture map[string]string `yaml:"capture"`
	// Skip turns the case off; it is reported as skipped.
	Skip bool `yaml:"skip"`
	// Headers are extra request headers, substituted.
	Headers map[string]string `yaml:"headers"`
	// ExpectBody asserts on the response: dotted path -> exact value, nested
	// object, or operators ($exists, $type, $len, $contains, $regex).
	ExpectBody map[string]interface{} `yaml:"expect_body"`
	// LocalOnly skips the case unless the target is marked local: a route a
	// deployed gateway does not route is unreachable there however healthy the
	// API is.
	LocalOnly bool `yaml:"local_only"`
	// RequiresVars skips the case, reported, when any of these variables is
	// empty -- for a credential the suite cannot mint itself.
	RequiresVars []string `yaml:"requires_vars"`
	// DelayMS waits before the request. Only where the wait is what is being
	// tested (an eventually consistent read), never to hide flakiness.
	DelayMS int `yaml:"delay_ms"`
	// Role names the target's role the case is sent as: its token goes in
	// the Authorization header. The runner signs a role in the first time a
	// case needs it. Never with auth: bearer or an Authorization header --
	// two ways of saying who sent a request test the wrong one.
	Role string `yaml:"role"`
	// Fallback is what the case expects when its role has no account on the
	// target: the same request is sent as the target's default role and
	// checked against these instead. Without one, such a case is skipped.
	Fallback *Fallback `yaml:"fallback"`

	// File and Line say where the case was written. Not part of the format:
	// they are what a problem or a result points at.
	File string `yaml:"-"`
	Line int    `yaml:"-"`
}

// File is one scenario file: a list of cases, run in order. A suite is its
// files in name order (Order), run as one sequence with one set of variables.
type File struct {
	Name  string
	Cases []Case
}

// caseFields is every key a case may have. A key outside it is refused rather
// than ignored: a misspelt `expect_statsu` decoded silently would be a case
// that checks nothing and passes.
var caseFields = map[string]bool{
	"name": true, "method": true, "path": true, "auth": true, "request": true,
	"form": true, "expect_status": true, "capture": true, "skip": true,
	"headers": true, "expect_body": true, "local_only": true,
	"requires_vars": true, "delay_ms": true, "sign": true, "role": true,
	"fallback": true,
}

// Fallback is a case's expectations when it is sent as the target's default
// role. Only expectations, never the request: what a request gets depends on
// who sends it, so the default role is checked against what it should get,
// and a fallback that could change the request would be another test under
// the same name.
type Fallback struct {
	ExpectStatus int                    `yaml:"expect_status"`
	ExpectBody   map[string]interface{} `yaml:"expect_body"`
}

// fallbackFields is every key a fallback block may have, refused otherwise.
var fallbackFields = map[string]bool{"expect_status": true, "expect_body": true}

// signFields is every key a sign block may have, refused otherwise for the
// same reason: a misspelt key is a signature made without it.
var signFields = map[string]bool{
	"snap": true, "client_key": true, "private_key": true, "secret": true,
	"token": true, "empty_body": true, "zone": true,
}
