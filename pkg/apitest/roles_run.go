package apitest

import (
	"fmt"
	"strings"
)

// SetRoles gives the runner the target's roles. They should have passed
// ValidateRoles; a login that does not parse is refused here too, since a
// run with a role it cannot sign in would report every case of it skipped
// for the wrong reason.
func (r *Runner) SetRoles(roles []Role) error {
	r.roles, r.defaultRole = map[string]*roleRun{}, ""
	for _, role := range roles {
		steps, err := ParseRoleLogin(role)
		if err != nil {
			return err
		}
		r.roles[role.Name] = &roleRun{role: role, steps: steps, needs: needsOf(steps)}
		if role.Default {
			r.defaultRole = role.Name
		}
	}
	return nil
}

// RoleNeeds is every variable a role's login uses that no earlier step
// captures: the role has an account on a target when each has a value.
func RoleNeeds(role Role) ([]string, error) {
	steps, err := ParseRoleLogin(role)
	if err != nil {
		return nil, err
	}
	return needsOf(steps), nil
}

func needsOf(steps []Case) []string {
	var needs []string
	seen, captured := map[string]bool{}, map[string]bool{}
	need := func(name string) {
		if !captured[name] && !seen[name] {
			seen[name] = true
			needs = append(needs, name)
		}
	}
	for _, s := range steps {
		for _, ref := range requestRefs(s) {
			need(ref.name)
		}
		for _, name := range s.RequiresVars {
			need(name)
		}
		for name := range s.Capture {
			captured[name] = true
		}
	}
	return needs
}

// RoleTokens is every role's token signed in so far, for the redactor: a
// role's token lives in the role's scope, never in Vars, so learning the
// captured *_token variables does not find it.
func (r *Runner) RoleTokens() []string {
	var out []string
	for _, rr := range r.roles {
		if rr.token != "" {
			out = append(out, rr.token)
		}
	}
	return out
}

// noAccount names the first variable the role's login needs that has no
// value: the role has no account on this target. "" when it has one.
func (r *Runner) noAccount(rr *roleRun) string {
	for _, name := range rr.needs {
		if strings.TrimSpace(r.Vars[name]) == "" {
			return name
		}
	}
	return ""
}

// runAsRole sends a case as its role, or -- when the role has no account and
// the case says what the default role gets -- as the default role, checked
// against the fallback. The request is the case's own either way.
func (r *Runner) runAsRole(c *Case, index int, emit func(Result)) Result {
	res := Result{File: c.File, Line: c.Line, Name: c.Name, Method: c.Method, Path: c.Path, Role: c.Role}
	skip := func(reason string) Result {
		res.Outcome, res.Reason = OutcomeSkip, reason
		return res
	}
	rr := r.roles[c.Role]
	if rr == nil {
		return fail(res, fmt.Sprintf("role %s is not one of the roles this run was given", c.Role))
	}
	sendAs, tc := rr, c
	if missing := r.noAccount(rr); missing != "" {
		gone := fmt.Sprintf("role %s has no account on this target (%s is empty)", c.Role, missing)
		if c.Fallback == nil {
			return skip(gone)
		}
		def := r.roles[r.defaultRole]
		if def == nil {
			return skip(gone + ", and the target has no default role to send its fallback as")
		}
		if m := r.noAccount(def); m != "" {
			return skip(fmt.Sprintf("%s, nor has the default role %s (%s is empty)", gone, def.role.Name, m))
		}
		sendAs = def
		fb := *c
		fb.ExpectStatus, fb.ExpectBody = c.Fallback.ExpectStatus, c.Fallback.ExpectBody
		tc = &fb
	}
	if why := r.signIn(sendAs, index, emit); why != "" {
		return skip(why)
	}
	out := r.runOne(tc, sendAs.token)
	out.Role = c.Role
	if sendAs != rr {
		out.RanAs = sendAs.role.Name
	}
	return out
}

// signIn runs a role's login steps once a run, in the role's own scope: the
// steps see the target's variables and seeds, and what they capture -- an
// otp_signature, the token -- stays with the role. Each step is a result of
// its own, so a refused password is a failure a person sees, never a run of
// cases that read as skipped. It returns why the role is not signed in, or "".
func (r *Runner) signIn(rr *roleRun, index int, emit func(Result)) string {
	if rr.tried {
		return rr.failure
	}
	rr.tried = true
	name := rr.role.Name

	shared := r.Vars
	scope := make(map[string]string, len(shared))
	for k, v := range shared {
		scope[k] = v
	}
	r.Vars = scope
	defer func() { r.Vars = shared }()

	for i := range rr.steps {
		step := rr.steps[i]
		res := r.runOne(&step, "")
		res.Index, res.Role, res.SignIn = index, name, true
		// A sign-in step that passed answered with the role's token, which
		// the redactor learns only once the role is signed in (RoleTokens)
		// or when its name says token: the step keeps no exchange to leak it.
		if res.Outcome == OutcomeOK {
			res.Request, res.Response = "", ""
		}
		emit(res)
		if res.Outcome != OutcomeOK {
			rr.failure = fmt.Sprintf("role %s did not sign in: %s:%d (%s) %s", name, step.File, step.Line, step.Name, res.Outcome)
			return rr.failure
		}
	}
	tok := strings.TrimSpace(scope[rr.role.Token])
	if tok == "" {
		rr.failure = fmt.Sprintf("role %s did not sign in: its steps passed, but none captured its token %s", name, rr.role.Token)
		emit(Result{File: RoleFile(name), Index: index, Name: "the role's token", Outcome: OutcomeFail, Reason: rr.failure, Role: name, SignIn: true})
		return rr.failure
	}
	rr.token = tok
	return ""
}
