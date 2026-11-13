package apitest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Runner executes cases in order against one target, carrying the variables
// they capture from one case to the next. It is the original suite's e2e Runner; the
// comments on each guard say which bug it came from there.
type Runner struct {
	Client  *http.Client
	BaseURL string
	// Vars is every variable, seeded or captured, as it substitutes into a
	// string.
	Vars map[string]string
	// Numbers holds variables that substitute as a JSON number when a request
	// leaf is exactly "${name}" (see SeedNumber).
	Numbers map[string]float64
	// Headers go on every case's request, before the case's own, so a case
	// may still replace one: the target's variables that are sent as a
	// header, such as a staging API's rate-limit bypass.
	Headers map[string]string
	// Local says the target is one a local_only case may run against. It is
	// the target's own setting: the original runner guessed it from "localhost" in the
	// URL, which is wrong for a local stack reached by any other name.
	Local bool
	// Base is the directory a form value "@path" is read from. The original suite's
	// scenarios say "@fixtures/logo.png" relative to its e2e directory, so
	// Base is that directory's stand-in.
	Base string
	// Sleep and Now are time.Sleep and time.Now; tests replace them so a
	// rate-limit back-off does not wait a minute.
	Sleep func(time.Duration)
	Now   func() time.Time
	// OnResult is called with each case's result as it is decided, so a
	// caller can report progress while the run goes.
	OnResult func(Result)

	caseSeq int
	// roles are the target's roles by name (SetRoles), each with what it
	// has done this run; defaultRole is the one a fallback is sent as.
	roles       map[string]*roleRun
	defaultRole string
}

// roleRun is one role during a run: signed in at most once, at first use.
type roleRun struct {
	role  Role
	steps []Case
	// needs is every variable the steps use that no earlier step captures:
	// the role has an account on the target when each of them has a value.
	needs []string
	tried bool
	// token is what the steps captured into role.Token; failure says why
	// there is none, for every case that needed the role.
	token   string
	failure string
}

// NewRunner returns a runner for baseURL with the original suite's 10-second request
// timeout.
func NewRunner(baseURL string) *Runner {
	return &Runner{
		Client:  &http.Client{Timeout: 10 * time.Second},
		BaseURL: strings.TrimRight(baseURL, "/"),
		Vars:    map[string]string{},
		Numbers: map[string]float64{},
		Headers: map[string]string{},
		Sleep:   time.Sleep,
		Now:     time.Now,
	}
}

// Seed sets a variable before the run.
func (r *Runner) Seed(k, v string) { r.Vars[k] = v }

// SeedNumber sets a variable that lands in a JSON body as a number when a leaf
// is exactly "${k}", and as its decimal text anywhere else. A voucher rule's
// `days` decodes server-side into []int, and ["5"] fails there with an error
// about the rule rather than the thing the case meant to test.
func (r *Runner) SeedNumber(k string, v float64) {
	r.Numbers[k] = v
	r.Vars[k] = strconv.FormatFloat(v, 'f', -1, 64)
}

// ErrUnreachable is a target that answered nothing at all.
var ErrUnreachable = errors.New("the target is not reachable")

// Ping checks the target answers HTTP. Any answer counts, a 404 included:
// deployed environments route only /v1 through their gateway, so /health can
// 404 on a healthy API. Only a transport failure means there is no server.
//
// A run was asked for, so an unreachable target fails it. The original runner skipped
// when no target was named -- a green result that tested nothing -- and a
// runner that is only ever started for a named target has no such case.
func (r *Runner) Ping() error {
	resp, err := r.Client.Get(r.BaseURL + "/health")
	if err != nil {
		return fmt.Errorf("%w at %s: %v", ErrUnreachable, r.BaseURL, err)
	}
	resp.Body.Close()
	return nil
}

// Run executes the cases in order, and goes on past a failure. What a failed
// case would have captured is missing, so a later case using any of it --
// in its request, its expectations, its signature, or as the token auth
// sends -- is not sent: it is skipped, naming the case it waited on, and
// what it would have captured is missing in turn. Every other case runs.
//
// Stopping at the first failure (the original suite's runner did) keeps a run from
// a cascade of failures about nothing, but it also shows one failure a run:
// a suite with five wrong statuses took five runs to learn them, and a
// model allowed three runs a turn could not finish one. Holding back only
// what depends on the failure keeps the first and loses the second.
func (r *Runner) Run(cases []Case) []Result {
	results := make([]Result, 0, len(cases))
	emit := func(res Result) {
		results = append(results, res)
		if r.OnResult != nil {
			r.OnResult(res)
		}
	}
	// missing is a variable a failed or held-back case should have
	// captured, and where: a later capture of the same name clears it.
	missing := map[string]string{}
	for i := range cases {
		c := &cases[i]
		var res Result
		if name, from := waitsOn(c, missing); name != "" {
			res = Result{File: c.File, Line: c.Line, Name: c.Name, Method: c.Method, Path: c.Path, Outcome: OutcomeSkip,
				Reason: fmt.Sprintf("not run: it needs ${%s}, which %s should have captured", name, from)}
		} else if c.Role != "" {
			res = r.runAsRole(c, i, emit)
		} else {
			res = r.runOne(c, "")
		}
		res.Index = i
		emit(res)
		for name := range c.Capture {
			if res.Outcome == OutcomeOK {
				delete(missing, name)
			} else if _, held := missing[name]; !held || res.Outcome == OutcomeFail {
				missing[name] = fmt.Sprintf("%s:%d (%s)", c.File, c.Line, c.Name)
			}
		}
	}
	return results
}

// waitsOn names the first variable a case uses that is missing, and the case
// that should have captured it.
func waitsOn(c *Case, missing map[string]string) (string, string) {
	if len(missing) == 0 {
		return "", ""
	}
	var names []string
	for _, ref := range requestRefs(*c) {
		names = append(names, ref.name)
	}
	if SendsToken(c.Auth) {
		names = append(names, "token")
	}
	names = append(names, refsIn(c.ExpectBody)...)
	for _, name := range names {
		if from, ok := missing[name]; ok {
			return name, from
		}
	}
	return "", ""
}

// callerIP gives each case its own client address, sent as X-Forwarded-For.
// The original suite's brute-force gates (auth 12/min) key anonymous callers on the
// client IP, and a suite registers and logs in far more often than that: from
// one address it spent minutes sleeping on 429s. The blind spot is that the
// 429 path is no longer exercised by default; a case asserting a lockout pins
// its own X-Forwarded-For, which wins. 10/8 is private, so a server resolving
// the address to a place finds none.
func callerIP(seq int) string {
	return fmt.Sprintf("10.%d.%d.%d", seq>>16&0xFF, seq>>8&0xFF, seq&0xFF)
}

// stripBearer drops a scheme the token already carries. The original suite's admin
// login returns "bearer <jwt>" verbatim while the owner login returns a bare
// JWT, so "Bearer " + token came out "Bearer bearer <jwt>" and every admin
// case 401ed.
func stripBearer(tok string) string {
	if len(tok) >= 7 && strings.EqualFold(tok[:7], "bearer ") {
		return strings.TrimSpace(tok[7:])
	}
	return tok
}

// collapseAuthScheme repairs a case's own Authorization header the same way,
// and only a doubled scheme: "Bearer not.a.jwt" sent to test a bad token must
// stay exactly that, or the 401 comes back for the wrong reason.
func collapseAuthScheme(v string) string {
	if len(v) >= 7 && strings.EqualFold(v[:7], "bearer ") {
		return "Bearer " + stripBearer(strings.TrimSpace(v[7:]))
	}
	return v
}

const (
	// rateLimitRetries bounds the back-off so a stuck limiter fails the run
	// instead of hanging it.
	rateLimitRetries = 2
	// rateLimitMaxWait caps one wait; a per-minute budget never needs more.
	rateLimitMaxWait = 75 * time.Second
)

// rateLimitWait reads when the server's budget resets (X-Ratelimit-Reset, unix
// seconds), falling back to five seconds.
func (r *Runner) rateLimitWait(resp *http.Response) time.Duration {
	if v := resp.Header.Get("X-Ratelimit-Reset"); v != "" {
		if epoch, err := strconv.ParseInt(v, 10, 64); err == nil {
			if d := time.Unix(epoch, 0).Sub(r.Now()) + time.Second; d > 0 {
				return min(d, rateLimitMaxWait)
			}
		}
	}
	return 5 * time.Second
}

// missingVar names the first variable in requires_vars that is empty, or "".
// It reads Vars only, the pool substitution reads, so a seeded value and a
// captured one are treated the same.
func (r *Runner) missingVar(tc *Case) string {
	for _, name := range tc.RequiresVars {
		if strings.TrimSpace(r.Vars[name]) == "" {
			return name
		}
	}
	return ""
}

// runOne sends one case and checks its answer. bearer, when set, is a role's
// token, sent as the Authorization header.
func (r *Runner) runOne(tc *Case, bearer string) Result {
	res := Result{File: tc.File, Line: tc.Line, Name: tc.Name, Method: tc.Method, Path: tc.Path}
	switch {
	case tc.Skip:
		res.Outcome, res.Reason = OutcomeSkip, "skip: true"
		return res
	case tc.LocalOnly && !r.Local:
		res.Outcome, res.Reason = OutcomeSkip, "local_only: the target is not marked local"
		return res
	}
	if name := r.missingVar(tc); name != "" {
		// Reported as a skip, never a pass: a case whose credential is absent
		// asserted nothing, and saying which variable is what gets it supplied.
		res.Outcome, res.Reason = OutcomeSkip, fmt.Sprintf("requires_vars: %s is empty", name)
		return res
	}
	if tc.DelayMS > 0 {
		r.Sleep(time.Duration(tc.DelayMS) * time.Millisecond)
	}

	r.caseSeq++
	ip := callerIP(r.caseSeq)
	path := r.substitute(tc.Path)
	res.Path = path
	url := r.BaseURL + path

	var bodyBytes []byte
	contentType := ""
	switch {
	case tc.Form != nil:
		buf, ct, err := r.buildMultipart(tc.Form)
		if err != nil {
			return fail(res, err.Error())
		}
		bodyBytes, contentType = buf.Bytes(), ct
	case tc.Request != nil:
		b, err := json.Marshal(r.substituteAny(tc.Request))
		if err != nil {
			return fail(res, "request: "+err.Error())
		}
		bodyBytes, contentType = b, "application/json"
	}

	// Rebuilt per attempt: a retry needs its own unread body.
	newRequest := func() (*http.Request, error) {
		var body io.Reader
		if bodyBytes != nil {
			body = bytes.NewReader(bodyBytes)
		}
		req, err := http.NewRequest(tc.Method, url, body)
		if err != nil {
			return nil, err
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		// Before the case's own headers, so a case pinning its address wins.
		req.Header.Set("X-Forwarded-For", ip)
		for _, k := range sortedKeys(r.Headers) {
			req.Header.Set(k, r.Headers[k])
		}
		for _, k := range sortedKeys(tc.Headers) {
			val := r.substitute(tc.Headers[k])
			if strings.EqualFold(k, "Authorization") {
				val = collapseAuthScheme(val)
			}
			req.Header.Set(k, val)
		}
		if SendsToken(tc.Auth) {
			if tok, ok := r.Vars["token"]; ok {
				req.Header.Set("Authorization", "Bearer "+stripBearer(tok))
			}
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+stripBearer(bearer))
		}
		// Last, over the request as it goes: its timestamp is this
		// attempt's, so a retry is signed afresh.
		if tc.Sign != nil {
			if err := r.signRequest(req, tc.Sign, bodyBytes, r.Now()); err != nil {
				return nil, err
			}
		}
		return req, nil
	}

	started := r.Now()
	var (
		resp     *http.Response
		respBody []byte
		dump     []byte
	)
	for attempt := 0; ; attempt++ {
		req, err := newRequest()
		if err != nil {
			return fail(res, err.Error())
		}
		dump, _ = httputil.DumpRequestOut(req, true)
		resp, err = r.Client.Do(req)
		if err != nil {
			res.DurationMS = r.Now().Sub(started).Milliseconds()
			res.Request = string(dump)
			return fail(res, err.Error())
		}
		respBody, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		// Back off only when the case was not asking for the 429.
		if resp.StatusCode != http.StatusTooManyRequests || tc.ExpectStatus == http.StatusTooManyRequests || attempt >= rateLimitRetries {
			break
		}
		r.Sleep(r.rateLimitWait(resp))
	}
	res.DurationMS = r.Now().Sub(started).Milliseconds()
	res.Status = resp.StatusCode

	failed := func(reason string) Result {
		res.Request, res.Response = string(dump), string(respBody)
		return fail(res, reason)
	}
	if tc.ExpectStatus != 0 && resp.StatusCode != tc.ExpectStatus {
		return failed(fmt.Sprintf("expected status %d, got %d", tc.ExpectStatus, resp.StatusCode))
	}
	if len(tc.Capture) > 0 {
		if err := r.captureVars(tc.Capture, respBody); err != nil {
			return failed("capture: " + err.Error())
		}
	}
	if len(tc.ExpectBody) > 0 {
		if err := r.assertBody(tc.ExpectBody, respBody); err != nil {
			return failed("expect_body: " + err.Error())
		}
	}
	// A pass keeps its exchange too (the owner's choice of 30 Oct): what a
	// case checked is read from what it sent and got. Masked and cut like a
	// failure's, by the redactor, before it is printed.
	res.Request, res.Response = string(dump), string(respBody)
	res.Outcome = OutcomeOK
	return res
}

func fail(res Result, reason string) Result {
	res.Outcome, res.Reason = OutcomeFail, reason
	return res
}

// buildMultipart builds a form body; a value starting with "@" is a file read
// from under Base, and one that would leave Base is refused.
func (r *Runner) buildMultipart(form map[string]string) (*bytes.Buffer, string, error) {
	buf := &bytes.Buffer{}
	w := multipart.NewWriter(buf)
	for _, k := range sortedKeys(form) {
		v := r.substitute(form[k])
		if !strings.HasPrefix(v, "@") {
			if err := w.WriteField(k, v); err != nil {
				return nil, "", err
			}
			continue
		}
		rel := strings.TrimPrefix(v, "@")
		full, err := within(r.Base, rel)
		if err != nil {
			return nil, "", err
		}
		data, err := os.ReadFile(full)
		if err != nil {
			return nil, "", fmt.Errorf("fixture %s: %w", rel, err)
		}
		part, err := w.CreateFormFile(k, rel)
		if err != nil {
			return nil, "", err
		}
		if _, err := part.Write(data); err != nil {
			return nil, "", err
		}
	}
	if err := w.Close(); err != nil {
		return nil, "", err
	}
	return buf, w.FormDataContentType(), nil
}

// within joins rel onto base and refuses a result outside base.
func within(base, rel string) (string, error) {
	if base == "" {
		return "", fmt.Errorf("fixture %s: this run has no fixtures", rel)
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("fixture %s: a fixture path is relative to the suite", rel)
	}
	full := filepath.Join(base, rel)
	if full != filepath.Clean(base) && !strings.HasPrefix(full, filepath.Clean(base)+string(filepath.Separator)) {
		return "", fmt.Errorf("fixture %s: the path leaves the suite's directory", rel)
	}
	return full, nil
}

// varRE is a ${name} reference.
var varRE = regexp.MustCompile(`\$\{([a-zA-Z_][a-zA-Z0-9_]*)\}`)

// substitute replaces every ${name} that has a value; an unknown one is left
// as written, so a failure shows what was not substituted.
func (r *Runner) substitute(s string) string {
	return varRE.ReplaceAllStringFunc(s, func(m string) string {
		if v, ok := r.Vars[varRE.FindStringSubmatch(m)[1]]; ok {
			return v
		}
		return m
	})
}

// substituteAny substitutes through a decoded body. A leaf that is exactly one
// number variable becomes a JSON number; anything else stays a string.
func (r *Runner) substituteAny(v interface{}) interface{} {
	switch t := v.(type) {
	case string:
		if m := varRE.FindStringSubmatch(t); m != nil && m[0] == t {
			if n, ok := r.Numbers[m[1]]; ok {
				return n
			}
		}
		return r.substitute(t)
	case map[string]interface{}:
		out := make(map[string]interface{}, len(t))
		for k, val := range t {
			out[k] = r.substituteAny(val)
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(t))
		for i, val := range t {
			out[i] = r.substituteAny(val)
		}
		return out
	default:
		return v
	}
}

// captureVars stores each captured path's value as its string form. A path
// the response does not have is skipped, as the original runner did: the case that
// needs it fails on a visible unsubstituted ${name}.
func (r *Runner) captureVars(spec map[string]string, body []byte) error {
	var root interface{}
	if err := json.Unmarshal(body, &root); err != nil {
		return fmt.Errorf("the response is not JSON: %w", err)
	}
	for _, name := range sortedKeys(spec) {
		if v := lookupPath(root, spec[name]); v != nil {
			r.Vars[name] = stringify(v)
		}
	}
	return nil
}

// stringify renders a captured JSON value: numbers without trailing zeros,
// booleans as true/false, anything else as JSON.
func stringify(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	case nil:
		return ""
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}
