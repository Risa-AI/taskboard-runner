package apitest

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Known is what a suite may refer to without capturing it: the target's seeds
// and variables, the fixtures stored for it, and its roles.
type Known struct {
	Seeds    []string
	Target   []string
	Fixtures []string
	Roles    []RoleInfo
}

// Options tunes Validate.
type Options struct {
	// Strict holds a case to what an AI-written case must meet: an
	// expect_status. Without one a case passes on any 2xx, which hides a
	// regression where the wrong success path was taken.
	Strict bool
	// CheckFixtures refuses a form "@path" that names no stored fixture.
	// Off where fixtures are not known (a bare `apitest validate`).
	CheckFixtures bool
}

// Problem is one thing wrong with a suite: where, and what to change.
type Problem struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Message string `json:"message"`
}

func (p Problem) String() string {
	if p.Line > 0 {
		return fmt.Sprintf("%s:%d: %s", p.File, p.Line, p.Message)
	}
	return p.File + ": " + p.Message
}

var httpMethods = map[string]bool{"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true, "HEAD": true, "OPTIONS": true}

// maxDelayMS caps delay_ms. The original suite's longest is 6 s (a 5 s cache); a
// minute is already a run spent waiting.
const maxDelayMS = 60_000

// Validate checks a suite before it runs: what would otherwise be found on a
// live server, one case at a time. Files are checked in run order, because
// that is what decides whether a variable exists yet: every ${name} must be a
// seed, a target variable, or captured by an earlier case -- the original suite's rule
// "any captured vars must come from earlier cases", checked instead of
// discovered as a request carrying a literal "${token}".
//
// A case's own captures count for its expect_body, which runs after them, and
// not for its request, which runs before.
func Validate(files []File, known Known, opts Options) []Problem {
	ordered := append([]File(nil), files...)
	Order(ordered)

	defined := map[string]bool{}
	for _, n := range known.Seeds {
		defined[n] = true
	}
	for _, n := range known.Target {
		defined[n] = true
	}
	fixtures := map[string]bool{}
	for _, f := range known.Fixtures {
		fixtures[f] = true
	}
	// Where each variable is first captured, to tell a case that uses one too
	// early where it comes from.
	capturedAt := map[string]string{}
	for _, f := range ordered {
		for _, c := range f.Cases {
			for _, name := range sortedKeys(c.Capture) {
				if _, ok := capturedAt[name]; !ok {
					capturedAt[name] = fmt.Sprintf("%s:%d", f.Name, c.Line)
				}
			}
		}
	}

	var problems []Problem
	seen := map[string]bool{}
	for _, f := range ordered {
		if seen[f.Name] {
			problems = append(problems, Problem{File: f.Name, Message: "two files have this name"})
		}
		seen[f.Name] = true
		for _, c := range f.Cases {
			add := func(format string, args ...interface{}) {
				problems = append(problems, Problem{File: f.Name, Line: c.Line, Message: fmt.Sprintf(format, args...)})
			}
			checkCase(c, opts, add)
			if c.Role != "" {
				checkRole(c, known.Roles, add)
			} else if c.Fallback != nil {
				add("fallback is set but role is not: a fallback is what the case expects when its role has no account on the target")
			}

			undefined := func(where, name string) {
				msg := fmt.Sprintf("%s uses ${%s}, which is not defined here: it is not a seed or a target variable, and no earlier case captures it", where, name)
				if at, ok := capturedAt[name]; ok {
					msg += fmt.Sprintf(" (%s captures it; this case must run after that one)", at)
				}
				add("%s", msg)
			}
			// The request side: defined before this case.
			for _, ref := range requestRefs(c) {
				if !defined[ref.name] {
					undefined(ref.where, ref.name)
				}
			}
			if SendsToken(c.Auth) && !defined["token"] {
				undefined("auth: "+c.Auth+" (it sends Bearer ${token})", "token")
			}
			for _, name := range c.RequiresVars {
				if !ValidVarName(name) {
					add("requires_vars names %q, which is not a variable name", name)
				} else if !defined[name] {
					add("requires_vars names %s, which nothing defines before this case, so the case would always skip", name)
				}
			}
			if opts.CheckFixtures {
				for _, k := range sortedKeys(c.Form) {
					if v := c.Form[k]; strings.HasPrefix(v, "@") && !fixtures[strings.TrimPrefix(v, "@")] {
						add("form %s names the fixture %s, which is not stored for this project", k, strings.TrimPrefix(v, "@"))
					}
				}
			}

			// This case's captures exist from here on, and for its own
			// expect_body.
			for _, name := range sortedKeys(c.Capture) {
				if !ValidVarName(name) {
					add("capture names %q, which is not a variable name", name)
					continue
				}
				if strings.TrimSpace(c.Capture[name]) == "" {
					add("capture %s has no path", name)
				}
				defined[name] = true
			}
			for _, name := range refsIn(c.ExpectBody) {
				if !defined[name] {
					undefined("expect_body", name)
				}
			}
			if c.Fallback != nil {
				for _, name := range refsIn(c.Fallback.ExpectBody) {
					if !defined[name] {
						undefined("fallback expect_body", name)
					}
				}
			}
		}
	}
	return problems
}

// checkCase is everything about one case that does not depend on the others.
func checkCase(c Case, opts Options, add func(string, ...interface{})) {
	if strings.TrimSpace(c.Name) == "" {
		add("a case needs a name: it is what the result shows")
	}
	if !httpMethods[c.Method] {
		add("method %q is not an HTTP method (GET, POST, PUT, PATCH, DELETE, HEAD, OPTIONS, in capitals)", c.Method)
	}
	if !strings.HasPrefix(c.Path, "/") {
		add("path %q must start with /; the target's URL is put in front of it", c.Path)
	}
	switch c.Auth {
	case "", "none", "bearer", "owner", "barber":
	default:
		add("auth is %q; it is bearer (sends Bearer ${token}) or none. To send another token, set headers: {Authorization: \"Bearer ${name}\"}", c.Auth)
	}
	if c.Request != nil && c.Form != nil {
		add("request and form are both set; a body is one or the other")
	}
	if c.Sign != nil {
		checkSign(c, add)
	}
	if c.ExpectStatus != 0 && (c.ExpectStatus < 100 || c.ExpectStatus > 599) {
		add("expect_status %d is not an HTTP status", c.ExpectStatus)
	}
	if opts.Strict && c.ExpectStatus == 0 {
		add("expect_status is missing: without it the case passes on any status")
	}
	if c.DelayMS < 0 || c.DelayMS > maxDelayMS {
		add("delay_ms %d is outside 0 to %d", c.DelayMS, maxDelayMS)
	}
	for _, path := range sortedKeys(c.ExpectBody) {
		checkExpectation(path, c.ExpectBody[path], add)
	}
}

// checkSign refuses a signing block the runner would fail on at send time,
// where the failure would read as the API refusing the case.
func checkSign(c Case, add func(string, ...interface{})) {
	s := c.Sign
	switch s.SNAP {
	case "token":
		if strings.TrimSpace(s.ClientKey) == "" || strings.TrimSpace(s.PrivateKey) == "" {
			add("sign: snap token needs client_key and private_key (the variables holding them)")
		}
	case "request":
		if strings.TrimSpace(s.Secret) == "" || strings.TrimSpace(s.Token) == "" {
			add("sign: snap request needs secret (the HMAC key) and token (the access token, e.g. ${snap_token})")
		}
		if c.Auth != "" && c.Auth != "none" {
			add("sign: snap request sends its own Bearer token; leave auth out")
		}
	default:
		add("sign: snap is %q; it is token (the access-token request, RSA) or request (every call after it, HMAC)", s.SNAP)
	}
	if s.EmptyBody != "" && s.EmptyBody != "hash" && s.EmptyBody != "blank" {
		add("sign: empty_body is %q; it is hash (the SHA-256 of nothing) or blank (nothing)", s.EmptyBody)
	}
	if s.Zone != "" {
		if _, err := time.LoadLocation(s.Zone); err != nil {
			add("sign: zone %q is not a time zone (use an IANA name such as Asia/Jakarta)", s.Zone)
		}
	}
	if c.Form != nil {
		add("sign: a multipart form cannot be signed -- its boundary is new every time; send a JSON request")
	}
}

// checkExpectation walks one expect_body value: operators known and well
// formed, and a map that mixes operators with fields refused -- the runner
// would treat every field as an unknown operator.
func checkExpectation(path string, v interface{}, add func(string, ...interface{})) {
	m, ok := v.(map[string]interface{})
	if !ok {
		return
	}
	if !hasOperatorKeys(m) {
		for _, k := range sortedKeys(m) {
			checkExpectation(path+"."+k, m[k], add)
		}
		return
	}
	for _, op := range sortedKeys(m) {
		arg := m[op]
		if !strings.HasPrefix(op, "$") {
			add("expect_body %s mixes operators and fields (%s); put fields under their own path", path, op)
			continue
		}
		if !operators[op] {
			add("expect_body %s: unknown operator %s; the operators are $exists, $type, $len, $contains, $regex", path, op)
			continue
		}
		switch op {
		case "$exists":
			if _, ok := arg.(bool); !ok {
				add("expect_body %s: $exists takes true or false", path)
			}
		case "$type":
			if s, ok := arg.(string); !ok || !jsonTypes[s] {
				add("expect_body %s: $type is one of string, number, bool, array, object, null", path)
			}
		case "$len":
			switch n := arg.(type) {
			case int:
				if n < 0 {
					add("expect_body %s: $len cannot be negative", path)
				}
			default:
				add("expect_body %s: $len takes a whole number", path)
			}
		case "$regex":
			s, ok := arg.(string)
			if !ok {
				add("expect_body %s: $regex takes a pattern string", path)
				break
			}
			// A ${name} in a pattern is only known at run time; check the
			// rest with a placeholder in its place.
			if _, err := regexp.Compile(varRE.ReplaceAllString(s, "x")); err != nil {
				add("expect_body %s: $regex %q does not compile: %v", path, s, err)
			}
		}
	}
}

type varRef struct{ where, name string }

// requestRefs is every ${name} the request side of a case uses: path, headers,
// request body and form values.
func requestRefs(c Case) []varRef {
	var refs []varRef
	for _, n := range refsInString(c.Path) {
		refs = append(refs, varRef{"path", n})
	}
	for _, k := range sortedKeys(c.Headers) {
		for _, n := range refsInString(c.Headers[k]) {
			refs = append(refs, varRef{"header " + k, n})
		}
	}
	for _, n := range refsIn(c.Request) {
		refs = append(refs, varRef{"request", n})
	}
	for _, k := range sortedKeys(c.Form) {
		for _, n := range refsInString(c.Form[k]) {
			refs = append(refs, varRef{"form " + k, n})
		}
	}
	if s := c.Sign; s != nil {
		for _, f := range []struct{ name, value string }{
			{"client_key", s.ClientKey}, {"private_key", s.PrivateKey}, {"secret", s.Secret}, {"token", s.Token},
		} {
			for _, n := range refsInString(f.value) {
				refs = append(refs, varRef{"sign " + f.name, n})
			}
		}
	}
	return refs
}

// refsIn is every ${name} in the string leaves of a decoded value, each once,
// sorted.
func refsIn(v interface{}) []string {
	set := map[string]bool{}
	var walk func(interface{})
	walk = func(v interface{}) {
		switch t := v.(type) {
		case string:
			for _, n := range refsInString(t) {
				set[n] = true
			}
		case map[string]interface{}:
			for _, val := range t {
				walk(val)
			}
		case []interface{}:
			for _, val := range t {
				walk(val)
			}
		}
	}
	walk(v)
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func refsInString(s string) []string {
	var out []string
	for _, m := range varRE.FindAllStringSubmatch(s, -1) {
		out = append(out, m[1])
	}
	return out
}

// SendsToken reports whether a case's auth sends Bearer ${token}: "bearer",
// or the original suite's "owner" and "barber", which mean the same.
func SendsToken(auth string) bool {
	return auth == "bearer" || auth == "owner" || auth == "barber"
}
