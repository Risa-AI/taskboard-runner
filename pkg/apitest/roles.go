package apitest

import (
	"fmt"
	"regexp"
	"strings"
)

// Role is one identity a target signs in as (docs/tech/tester/TECH_PLAN.md,
// slice 7). It is the target's data, never the runner's: the runner knows no
// project's roles, only the ones a target lists.
//
// A role holds no credential. Its login steps name the target's variables
// (${finance_approver_email}), which stay sealed where every variable is.
type Role struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Default is the role a case's fallback is sent as. At most one.
	Default bool `json:"default,omitempty"`
	// Login is the steps that sign the role in, in the suite's own format:
	// a YAML list of cases. Steps, not one request, because signing in can
	// take two calls -- an admin with 2FA on is given an otp_signature first
	// and a token only for the code. They run in the role's own scope: what
	// they capture is not visible to the suite's cases.
	Login string `json:"login"`
	// Token is the variable one of the steps captures: the role's token.
	Token string `json:"token"`
}

// RoleInfo is what a suite's validation needs to know of a role.
type RoleInfo struct {
	Name    string
	Default bool
}

// Infos is what validating a suite needs of these roles.
func Infos(roles []Role) []RoleInfo {
	out := make([]RoleInfo, 0, len(roles))
	for _, r := range roles {
		out = append(out, RoleInfo{Name: r.Name, Default: r.Default})
	}
	return out
}

var roleNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// ValidRoleName reports whether s may name a role: lower case, digits and
// underscores, a letter first, at most 32 characters.
func ValidRoleName(s string) bool { return roleNameRE.MatchString(s) }

// RoleFile is the name a role's login steps go by in a problem and a result.
func RoleFile(name string) string { return "role " + name }

// ParseRoleLogin reads a role's login steps, refusing what Parse refuses.
func ParseRoleLogin(r Role) ([]Case, error) {
	f, err := parseCases(RoleFile(r.Name), []byte(r.Login))
	return f.Cases, err
}

// ValidateRoles checks a target's roles against what its steps may use: the
// target's seeds and variables, and what an earlier step of the same role
// captured. Each step is held to what an AI-written case must meet, because
// a sign-in that passes on any status passes on a refused password.
func ValidateRoles(roles []Role, known Known) []Problem {
	var problems []Problem
	add := func(name string, line int, format string, args ...interface{}) {
		problems = append(problems, Problem{File: RoleFile(name), Line: line, Message: fmt.Sprintf(format, args...)})
	}
	seen := map[string]bool{}
	var defaults []string
	scope := Known{Seeds: known.Seeds, Target: known.Target, Fixtures: known.Fixtures}
	for _, r := range roles {
		if !ValidRoleName(r.Name) {
			add(r.Name, 0, "%q is not a role name: lower case letters, digits and _, a letter first, at most 32 characters", r.Name)
			continue
		}
		if seen[r.Name] {
			add(r.Name, 0, "two roles have this name")
			continue
		}
		seen[r.Name] = true
		if r.Default {
			defaults = append(defaults, r.Name)
		}
		if strings.TrimSpace(r.Login) == "" {
			add(r.Name, 0, "login is empty: give the steps that sign this role in, as cases")
			continue
		}
		steps, err := ParseRoleLogin(r)
		if err != nil {
			problems = append(problems, Problem{File: RoleFile(r.Name), Message: err.Error()})
			continue
		}
		if len(steps) == 0 {
			add(r.Name, 0, "login has no steps: give the steps that sign this role in, as cases")
			continue
		}
		problems = append(problems, Validate([]File{{Name: RoleFile(r.Name), Cases: steps}}, scope, Options{Strict: true})...)
		captured := false
		for _, s := range steps {
			if s.Role != "" || s.Fallback != nil {
				add(r.Name, s.Line, "a login step is sent as no role: leave role and fallback out of it")
			}
			if _, ok := s.Capture[r.Token]; ok {
				captured = true
			}
		}
		switch {
		case !ValidVarName(r.Token):
			add(r.Name, 0, "token is %q; name the variable a step captures the token into (e.g. token)", r.Token)
		case !captured:
			add(r.Name, 0, "token is %s, but no login step captures it", r.Token)
		}
	}
	if len(defaults) > 1 {
		problems = append(problems, Problem{File: "roles", Message: fmt.Sprintf("%s are all the default; a fallback is sent as one role, so mark one", strings.Join(defaults, ", "))})
	}
	return problems
}

// checkRole is a case's role against the target's roles. Called only for a
// case that names one.
func checkRole(c Case, roles []RoleInfo, add func(string, ...interface{})) {
	var role *RoleInfo
	var names []string
	defaultName := ""
	for i := range roles {
		names = append(names, roles[i].Name)
		if roles[i].Name == c.Role {
			role = &roles[i]
		}
		if roles[i].Default {
			defaultName = roles[i].Name
		}
	}
	switch {
	case len(roles) == 0:
		add("role %s: the target has no roles; add them to the target first", c.Role)
		return
	case role == nil:
		add("role %s is not one of the target's roles (%s)", c.Role, strings.Join(names, ", "))
		return
	}
	if SendsToken(c.Auth) {
		add("role and auth: %s both say who sends the request; keep role", c.Auth)
	}
	for k := range c.Headers {
		if strings.EqualFold(k, "Authorization") {
			add("role sends the role's token as Authorization; a case with a role sets no Authorization header")
		}
	}
	if c.Sign != nil && c.Sign.SNAP == "request" {
		add("role and sign: snap request both send a Bearer token; keep one")
	}
	f := c.Fallback
	if f == nil {
		return
	}
	switch {
	case role.Default:
		add("fallback is set on the default role's own case; it is the role a fallback would be sent as")
	case defaultName == "":
		add("fallback is set, but the target has no default role to send it as; mark one")
	}
	if f.ExpectStatus == 0 {
		add("fallback has no expect_status: without it the default role passes on any status")
	} else if f.ExpectStatus < 100 || f.ExpectStatus > 599 {
		add("fallback expect_status %d is not an HTTP status", f.ExpectStatus)
	}
	for _, path := range sortedKeys(f.ExpectBody) {
		checkExpectation(path, f.ExpectBody[path], func(format string, args ...interface{}) {
			add("fallback "+format, args...)
		})
	}
}
