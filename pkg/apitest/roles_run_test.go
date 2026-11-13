package apitest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// gatewayLike answers as an admin API does where roles matter: ADMIN
// MEMBER signs in with one call, a Finance Approver with 2FA with two (an
// otp_signature, then the code), and approving an unknown withdrawal is a
// 404 for the approver and a 401 for anyone else.
type gatewayLike struct {
	mu     sync.Mutex
	logins map[string]int
}

func (s *gatewayLike) handler(t *testing.T) http.Handler {
	s.logins = map[string]int{}
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, status int, v interface{}) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		var b map[string]string
		_ = json.NewDecoder(r.Body).Decode(&b)
		s.mu.Lock()
		s.logins[b["email"]]++
		s.mu.Unlock()
		switch {
		case b["email"] == "member@example.test" && b["password"] == "pw-member":
			write(w, 201, map[string]string{"access_token": "tok-member"})
		case b["email"] == "fa@example.test" && b["password"] == "pw-fa":
			write(w, 201, map[string]string{"otp_signature": "sig-fa"})
		default:
			write(w, 400, map[string]string{"message": "email atau password salah"})
		}
	})
	mux.HandleFunc("/verify", func(w http.ResponseWriter, r *http.Request) {
		var b map[string]string
		_ = json.NewDecoder(r.Body).Decode(&b)
		if b["otp_signature"] == "sig-fa" && b["otp_code"] == "8888" {
			write(w, 201, map[string]string{"access_token": "tok-fa"})
			return
		}
		write(w, 400, map[string]string{"message": "token is not verified"})
	})
	mux.HandleFunc("/w/approve", func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer tok-fa":
			write(w, 404, map[string]string{"message": "disbursement withdrawal tidak ditemukan"})
		default:
			write(w, 401, map[string]string{"message": "User tidak memiliki izin yang diperlukan"})
		}
	})
	mux.HandleFunc("/w/echo", func(w http.ResponseWriter, r *http.Request) {
		write(w, 500, map[string]string{"saw": r.Header.Get("Authorization")})
	})
	return mux
}

var runRoles = []Role{
	{Name: "admin", Default: true, Token: "token", Login: `- name: sign in
  method: POST
  path: /login
  request: {email: "${admin_email}", password: "${admin_password}"}
  expect_status: 201
  capture: {token: access_token}
`},
	{Name: "finance_approver", Token: "token", Login: `- name: sign in
  method: POST
  path: /login
  request: {email: "${fa_email}", password: "${fa_password}"}
  expect_status: 201
  capture: {otp_signature: otp_signature}
- name: confirm the code
  method: POST
  path: /verify
  request: {otp_signature: "${otp_signature}", otp_code: "8888"}
  expect_status: 201
  capture: {token: access_token}
`},
}

func roleRunner(t *testing.T, vars map[string]string) (*Runner, *gatewayLike) {
	t.Helper()
	s := &gatewayLike{}
	srv := httptest.NewServer(s.handler(t))
	t.Cleanup(srv.Close)
	r, _ := fastRunner(srv.URL)
	for k, v := range vars {
		r.Seed(k, v)
	}
	require.NoError(t, r.SetRoles(runRoles))
	return r, s
}

var bothAccounts = map[string]string{"admin_email": "member@example.test", "admin_password": "pw-member", "fa_email": "fa@example.test", "fa_password": "pw-fa"}

func approveAs(role string, fallback *Fallback) Case {
	return Case{File: "20_wd.yml", Line: 1, Name: "approve an unknown withdrawal", Method: "POST", Path: "/w/approve",
		Role: role, ExpectStatus: 404, ExpectBody: map[string]interface{}{"message": map[string]interface{}{"$contains": "tidak ditemukan"}}, Fallback: fallback}
}

var memberRefused = &Fallback{ExpectStatus: 401, ExpectBody: map[string]interface{}{"message": map[string]interface{}{"$contains": "tidak memiliki izin"}}}

// A role signs in once, the first time a case needs it, and every case of it
// goes with its token. What the login steps capture -- the otp_signature, the
// token -- stays with the role: a suite case reading ${token} must not get a
// role's token by accident and test the wrong identity.
func TestRun_ARoleSignsInOnceAndItsCasesGoWithItsToken(t *testing.T) {
	r, s := roleRunner(t, bothAccounts)
	plain := Case{File: "10_x.yml", Line: 1, Name: "no role", Method: "GET", Path: "/w/echo", ExpectStatus: 500}
	results := r.Run([]Case{plain, approveAs("finance_approver", nil), approveAs("finance_approver", nil)})

	require.Len(t, results, 5, "a case, two sign-in steps, two cases")
	require.Equal(t, 1, s.logins["fa@example.test"], "signed in once for two cases")
	require.Zero(t, s.logins["member@example.test"], "a role no case needs costs no request")

	steps := results[1:3]
	for _, st := range steps {
		require.True(t, st.SignIn)
		require.Equal(t, "finance_approver", st.Role)
		require.Equal(t, "role finance_approver", st.File)
		require.Equal(t, 1, st.Index, "a sign-in sorts just before the case that needed it")
		require.Equal(t, OutcomeOK, st.Outcome, st.Reason)
		// Its answer is the role's token, which the redactor may not know yet.
		require.Empty(t, st.Request+st.Response, "a sign-in step that passed keeps no exchange")
	}
	for _, c := range results[3:] {
		require.Equal(t, OutcomeOK, c.Outcome, c.Reason)
		require.Equal(t, "finance_approver", c.Role)
		require.Empty(t, c.RanAs)
		require.NotEmpty(t, c.Request, "a case that passed keeps its exchange")
	}
	require.NotContains(t, r.Vars, "otp_signature")
	require.NotContains(t, r.Vars, "token")
	require.Equal(t, []string{"tok-fa"}, r.RoleTokens())
}

// A role with no account on the target: a case that says what the default
// role gets is sent as the default and checked against that; a case that
// does not is skipped, naming the variable to fill. Sent as the approver's
// own expectations, the member's 401 would fail with nothing broken.
func TestRun_ARoleWithNoAccountFallsBackOnlyWhereWritten(t *testing.T) {
	r, s := roleRunner(t, map[string]string{"admin_email": "member@example.test", "admin_password": "pw-member", "fa_email": "", "fa_password": ""})
	results := r.Run([]Case{approveAs("finance_approver", memberRefused), approveAs("finance_approver", nil)})

	require.Len(t, results, 3, "the default's sign-in, then both cases")
	require.True(t, results[0].SignIn)
	require.Equal(t, "admin", results[0].Role)

	fell := results[1]
	require.Equal(t, OutcomeOK, fell.Outcome, fell.Reason)
	require.Equal(t, 401, fell.Status)
	require.Equal(t, "finance_approver", fell.Role)
	require.Equal(t, "admin", fell.RanAs)

	skipped := results[2]
	require.Equal(t, OutcomeSkip, skipped.Outcome)
	require.Equal(t, "role finance_approver has no account on this target (fa_email is empty)", skipped.Reason)
	require.Zero(t, s.logins["fa@example.test"]+s.logins[""], "nothing is sent for a role with no account")

	t.Run("and the default has none either", func(t *testing.T) {
		r, _ := roleRunner(t, map[string]string{"admin_email": "", "admin_password": "", "fa_email": "", "fa_password": ""})
		res := r.Run([]Case{approveAs("finance_approver", memberRefused)})
		require.Len(t, res, 1)
		require.Equal(t, OutcomeSkip, res[0].Outcome)
		require.Equal(t, "role finance_approver has no account on this target (fa_email is empty), nor has the default role admin (admin_email is empty)", res[0].Reason)
	})
}

// A wrong password is a failure a person sees, not an absent account to fall
// back from: the sign-in step fails the run, and every case of the role is
// skipped naming it -- trying again for each case would only lock the
// account.
func TestRun_AFailedSignInFailsTheRunAndItsCasesSayWhy(t *testing.T) {
	vars := map[string]string{}
	for k, v := range bothAccounts {
		vars[k] = v
	}
	vars["fa_password"] = "wrong"
	r, s := roleRunner(t, vars)
	results := r.Run([]Case{approveAs("finance_approver", memberRefused), approveAs("finance_approver", nil)})

	require.Len(t, results, 3)
	require.True(t, results[0].SignIn)
	require.Equal(t, OutcomeFail, results[0].Outcome)
	for _, c := range results[1:] {
		require.Equal(t, OutcomeSkip, c.Outcome)
		require.Equal(t, "role finance_approver did not sign in: role finance_approver:1 (sign in) fail", c.Reason)
		require.Empty(t, c.RanAs, "a refused password never falls back")
	}
	require.Equal(t, 1, s.logins["fa@example.test"])

	sum := Summarize(2, results, Build{})
	require.Equal(t, VerdictFailed, sum.Verdict)
	require.Equal(t, 3, sum.Total, "two cases and the sign-in step")
	require.Equal(t, 1, sum.Failed)
	require.Equal(t, 2, sum.Skipped)
	require.Zero(t, sum.NotRun)
}

// A role's token never leaves the runner: it is masked in a failure's dump
// whatever the target named its variable, since Learn only knows *_token.
func TestRun_ARoleTokenIsMaskedInAFailure(t *testing.T) {
	r, _ := roleRunner(t, bothAccounts)
	results := r.Run([]Case{{File: "20_wd.yml", Line: 1, Name: "echo", Method: "GET", Path: "/w/echo", Role: "finance_approver", ExpectStatus: 200}})
	failed := results[len(results)-1]
	require.Equal(t, OutcomeFail, failed.Outcome)
	require.Contains(t, failed.Response, "tok-fa", "the server echoed it")

	red := NewRedactor(nil)
	red.Learn(r.Vars)
	red.Mask(r.RoleTokens()...)
	masked := red.Result(failed)
	require.NotContains(t, masked.Response, "tok-fa")
	require.NotContains(t, masked.Request, "tok-fa")
	require.True(t, strings.Contains(masked.Response, mask))
}
