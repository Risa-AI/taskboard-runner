package apitest

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// A two-step admin login, as one is with 2FA on: the first call answers
// an otp_signature, the second the token.
const twoStepLogin = `- name: sign in
  method: POST
  path: /dashboard/v1/authenticate/login
  headers: {X-ADMIN: admin}
  request: {email: "${fa_email}", password: "${fa_password}"}
  expect_status: 201
  capture: {otp_signature: otp_signature}
- name: confirm the code
  method: POST
  path: /dashboard/v1/authenticate/verify-otp
  request: {otp_signature: "${otp_signature}", otp_code: "${fa_code}"}
  expect_status: 201
  capture: {token: access_token}
`

const oneStepLogin = `- name: sign in
  method: POST
  path: /dashboard/v1/authenticate/login
  headers: {X-ADMIN: admin}
  request: {email: "${admin_email}", password: "${admin_password}"}
  expect_status: 201
  capture: {token: access_token}
`

var adminRoles = []Role{
	{Name: "admin", Default: true, Login: oneStepLogin, Token: "token"},
	{Name: "finance_approver", Login: twoStepLogin, Token: "token"},
}

var adminKnown = Known{
	Target: []string{"admin_email", "admin_password", "fa_email", "fa_password", "fa_code"},
	Roles:  Infos(adminRoles),
}

func roleProblems(t *testing.T, roles []Role) []string {
	t.Helper()
	return messages(ValidateRoles(roles, Known{Target: adminKnown.Target}))
}

// The roles a suite will lean on are checked when they are saved, so a
// login that can never sign in is found on the Target tab and not as a run
// of skips that each look like a missing account.
func TestValidateRoles_ATargetsRolesAreCheckedLikeCases(t *testing.T) {
	require.Empty(t, roleProblems(t, adminRoles))

	t.Run("a step may use only what an earlier step of the same role captured", func(t *testing.T) {
		backwards := strings.Replace(twoStepLogin, "capture: {otp_signature: otp_signature}", "capture: {other: x}", 1)
		ps := roleProblems(t, []Role{{Name: "finance_approver", Login: backwards, Token: "token"}})
		require.Len(t, ps, 1, ps)
		require.Contains(t, ps[0], "role finance_approver:8: request uses ${otp_signature}")
	})
	t.Run("a step naming a variable the target does not have", func(t *testing.T) {
		ps := roleProblems(t, []Role{{Name: "viewer", Login: strings.ReplaceAll(oneStepLogin, "admin_email", "viewer_email"), Token: "token"}})
		require.Len(t, ps, 1, ps)
		require.Contains(t, ps[0], "${viewer_email}")
	})
	t.Run("a sign-in that passes on any status passes on a refused password", func(t *testing.T) {
		ps := roleProblems(t, []Role{{Name: "admin", Login: strings.Replace(oneStepLogin, "  expect_status: 201\n", "", 1), Token: "token"}})
		require.Len(t, ps, 1, ps)
		require.Contains(t, ps[0], "expect_status is missing")
	})
	t.Run("the token must be something a step captures", func(t *testing.T) {
		ps := roleProblems(t, []Role{{Name: "admin", Login: oneStepLogin, Token: "admin_token"}})
		require.Equal(t, []string{"role admin: token is admin_token, but no login step captures it"}, ps)
	})
	t.Run("a step is sent as no role", func(t *testing.T) {
		ps := roleProblems(t, []Role{{Name: "admin", Login: oneStepLogin + "- name: again\n  method: GET\n  path: /me\n  role: admin\n  expect_status: 200\n", Token: "token"}})
		require.Contains(t, strings.Join(ps, "\n"), "a login step is sent as no role")
	})
	t.Run("one default, because a fallback is sent as one role", func(t *testing.T) {
		ps := roleProblems(t, []Role{
			{Name: "admin", Default: true, Login: oneStepLogin, Token: "token"},
			{Name: "viewer", Default: true, Login: oneStepLogin, Token: "token"},
		})
		require.Equal(t, []string{"roles: admin, viewer are all the default; a fallback is sent as one role, so mark one"}, ps)
	})
	t.Run("names", func(t *testing.T) {
		ps := roleProblems(t, []Role{
			{Name: "Finance Approver", Login: oneStepLogin, Token: "token"},
			{Name: "admin", Login: oneStepLogin, Token: "token"},
			{Name: "admin", Login: oneStepLogin, Token: "token"},
			{Name: "viewer", Login: "  ", Token: "token"},
		})
		require.Len(t, ps, 3, ps)
		require.Contains(t, ps[0], "is not a role name")
		require.Contains(t, ps[1], "two roles have this name")
		require.Contains(t, ps[2], "login is empty")
	})
}

func caseProblems(t *testing.T, src string) []string {
	t.Helper()
	f := mustParse(t, "20_wd.yml", src)
	return messages(Validate([]File{f}, adminKnown, Options{}))
}

// A case's role is checked against the target's roles, and nothing else may
// also say who sends it: with two, the test passes or fails on whichever the
// runner happened to apply.
func TestValidate_ACasesRole(t *testing.T) {
	const approve = "- name: approve\n  method: POST\n  path: /w/1/approve\n  expect_status: 404\n"

	require.Empty(t, caseProblems(t, approve+"  role: finance_approver\n"))

	ps := caseProblems(t, approve+"  role: finance_maker\n")
	require.Equal(t, []string{"20_wd.yml:1: role finance_maker is not one of the target's roles (admin, finance_approver)"}, ps)

	ps = caseProblems(t, approve+"  role: finance_approver\n  auth: bearer\n")
	require.Contains(t, strings.Join(ps, "\n"), "role and auth: bearer both say who sends the request")

	ps = caseProblems(t, approve+"  role: finance_approver\n  headers: {authorization: \"Bearer x\"}\n")
	require.Equal(t, []string{"20_wd.yml:1: role sends the role's token as Authorization; a case with a role sets no Authorization header"}, ps)

	f := mustParse(t, "20_wd.yml", approve+"  role: admin\n")
	ps = messages(Validate([]File{f}, Known{}, Options{}))
	require.Equal(t, []string{"20_wd.yml:1: role admin: the target has no roles; add them to the target first"}, ps)
}

// A fallback is only expectations, for a role with no account, sent as the
// default: anything else would be a different test under the same name, or
// one that can never run.
func TestValidate_AFallback(t *testing.T) {
	const approve = "- name: approve\n  method: POST\n  path: /w/1/approve\n  expect_status: 404\n"
	const fallback = "  fallback:\n    expect_status: 401\n    expect_body: {message: {$contains: \"tidak memiliki izin\"}}\n"

	require.Empty(t, caseProblems(t, approve+"  role: finance_approver\n"+fallback))

	ps := caseProblems(t, approve+fallback)
	require.Equal(t, []string{"20_wd.yml:1: fallback is set but role is not: a fallback is what the case expects when its role has no account on the target"}, ps)

	ps = caseProblems(t, approve+"  role: admin\n"+fallback)
	require.Equal(t, []string{"20_wd.yml:1: fallback is set on the default role's own case; it is the role a fallback would be sent as"}, ps)

	ps = caseProblems(t, approve+"  role: finance_approver\n  fallback:\n    expect_body: {message: {$contains: x}}\n")
	require.Equal(t, []string{"20_wd.yml:1: fallback has no expect_status: without it the default role passes on any status"}, ps)

	ps = caseProblems(t, approve+"  role: finance_approver\n  fallback:\n    expect_status: 401\n    expect_body: {message: {$has: x}}\n")
	require.Contains(t, strings.Join(ps, "\n"), "fallback expect_body message: unknown operator $has")

	ps = caseProblems(t, approve+"  role: finance_approver\n  fallback:\n    expect_status: 401\n    expect_body: {id: \"${later}\"}\n")
	require.Contains(t, strings.Join(ps, "\n"), "fallback expect_body uses ${later}")

	noDefault := Known{Target: adminKnown.Target, Roles: []RoleInfo{{Name: "admin"}, {Name: "finance_approver"}}}
	f := mustParse(t, "20_wd.yml", approve+"  role: finance_approver\n"+fallback)
	ps = messages(Validate([]File{f}, noDefault, Options{}))
	require.Equal(t, []string{"20_wd.yml:1: fallback is set, but the target has no default role to send it as; mark one"}, ps)

	_, err := Parse("20_wd.yml", []byte(approve+"  role: finance_approver\n  fallback:\n    expect_status: 401\n    request: {otp_code: \"1\"}\n"))
	require.ErrorContains(t, err, "20_wd.yml:8: fallback takes expect_status and expect_body only, not \"request\"")
}
