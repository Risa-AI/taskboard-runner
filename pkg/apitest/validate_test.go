package apitest

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func mustParse(t *testing.T, name, src string) File {
	t.Helper()
	f, err := Parse(name, []byte(src))
	require.NoError(t, err)
	return f
}

func messages(ps []Problem) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.String())
	}
	return out
}

// A variable used in a file that runs before the one capturing it is refused
// at its line, and the message says where it is captured. On a live server
// this is a request carrying a literal "${token}" and a 401 that reads like
// a broken login.
func TestValidate_AVariableMustBeCapturedEarlierInRunOrder(t *testing.T) {
	early := mustParse(t, "01_shop.yml", "- name: list shops\n  method: GET\n  path: /v1/shops\n  auth: owner\n  expect_status: 200\n")
	auth := mustParse(t, "02_auth.yml", "- name: login\n  method: POST\n  path: /v1/auth/login\n  expect_status: 200\n  capture:\n    token: data.token\n")
	late := mustParse(t, "03_me.yml", "- name: me\n  method: GET\n  path: /v1/me\n  auth: owner\n  expect_status: 200\n")

	ps := Validate([]File{late, auth, early}, Known{}, Options{})
	require.Len(t, ps, 1, messages(ps))
	require.Equal(t, "01_shop.yml", ps[0].File)
	require.Equal(t, 1, ps[0].Line)
	require.Contains(t, ps[0].Message, "${token}")
	require.Contains(t, ps[0].Message, "02_auth.yml:1 captures it; this case must run after that one")
}

// Seeds and target variables are defined from the start; a case's own
// captures count for its expect_body (which runs after them) and not for its
// request (which runs before) -- the order the original suite's runner uses.
func TestValidate_WhatIsDefinedWhere(t *testing.T) {
	f := mustParse(t, "01_x.yml", `- name: register
  method: POST
  path: /v1/auth/register
  request:
    email: "e2e-${rand}@example.com"
    password: "${adminPassword}"
  expect_status: 201
  capture:
    user_id: data.user.id
  expect_body:
    data.user.id: "${user_id}"
- name: uses its own capture too early
  method: GET
  path: /v1/users/${other_id}
  capture:
    other_id: data.id
`)
	ps := Validate([]File{f}, Known{Seeds: []string{"rand"}, Target: []string{"adminPassword"}}, Options{})
	require.Len(t, ps, 1, messages(ps))
	require.Equal(t, 12, ps[0].Line)
	require.Contains(t, ps[0].Message, "path uses ${other_id}")
}

// Each malformed thing is refused with a sentence that says what to write.
func TestValidate_MalformedCases(t *testing.T) {
	for _, tc := range []struct {
		name, src, want string
	}{
		{"no name", "- method: GET\n  path: /x\n", "a case needs a name"},
		{"lower-case method", "- name: a\n  method: get\n  path: /x\n", `method "get" is not an HTTP method`},
		{"relative path", "- name: a\n  method: GET\n  path: v1/x\n", "must start with /"},
		{"unknown auth", "- name: a\n  method: GET\n  path: /x\n  auth: admin\n", "set headers: {Authorization"},
		{"request and form", "- name: a\n  method: POST\n  path: /x\n  request: {a: 1}\n  form: {b: c}\n", "request and form are both set"},
		{"bad status", "- name: a\n  method: GET\n  path: /x\n  expect_status: 42\n", "expect_status 42 is not an HTTP status"},
		{"long delay", "- name: a\n  method: GET\n  path: /x\n  delay_ms: 600000\n", "delay_ms 600000 is outside"},
		{"unknown operator", "- name: a\n  method: GET\n  path: /x\n  expect_body:\n    data.a: {$eq: 1}\n", "unknown operator $eq"},
		{"bad $type", "- name: a\n  method: GET\n  path: /x\n  expect_body:\n    data.a: {$type: integer}\n", "$type is one of"},
		{"$len not a number", "- name: a\n  method: GET\n  path: /x\n  expect_body:\n    data.a: {$len: two}\n", "$len takes a whole number"},
		{"bad regex", "- name: a\n  method: GET\n  path: /x\n  expect_body:\n    data.a: {$regex: \"([\"}\n", "does not compile"},
		{"operators mixed with fields", "- name: a\n  method: GET\n  path: /x\n  expect_body:\n    data.a: {$exists: true, id: 1}\n", "mixes operators and fields"},
		{"nested unknown operator", "- name: a\n  method: GET\n  path: /x\n  expect_body:\n    data:\n      user: {$bogus: 1}\n", "data.user: unknown operator $bogus"},
		{"requires an undefined var", "- name: a\n  method: GET\n  path: /x\n  requires_vars: [adminEmial]\n", "the case would always skip"},
		{"bad capture name", "- name: a\n  method: GET\n  path: /x\n  capture:\n    9lives: data.x\n", "not a variable name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ps := Validate([]File{mustParse(t, "01.yml", tc.src)}, Known{}, Options{})
			require.NotEmpty(t, ps)
			require.Contains(t, strings.Join(messages(ps), "\n"), tc.want)
		})
	}
}

// Strict is what an AI-written case is held to: a status. Without one a case
// passes on any 2xx. A hand-written suite is not held to it, because
// the original suite's own cases are not all written that way.
func TestValidate_StrictRequiresAStatus(t *testing.T) {
	f := mustParse(t, "01.yml", "- name: a\n  method: GET\n  path: /x\n")
	require.Empty(t, Validate([]File{f}, Known{}, Options{}))
	ps := Validate([]File{f}, Known{}, Options{Strict: true})
	require.Len(t, ps, 1)
	require.Contains(t, ps[0].Message, "passes on any status")
}

// With CheckFixtures, a form upload must name a stored fixture; a ${name} in
// a pattern is not a compile error.
func TestValidate_FixturesAndPatterns(t *testing.T) {
	f := mustParse(t, "01.yml", `- name: upload
  method: POST
  path: /logo
  form:
    logo: "@fixtures/logo.png"
    cover: "@fixtures/cover.jpg"
  expect_body:
    data.url: {$regex: "^https://cdn/${rand}/"}
`)
	ps := Validate([]File{f}, Known{Seeds: []string{"rand"}, Fixtures: []string{"fixtures/logo.png"}}, Options{CheckFixtures: true})
	require.Len(t, ps, 1, messages(ps))
	require.Contains(t, ps[0].Message, "form cover names the fixture fixtures/cover.jpg")
	require.Empty(t, Validate([]File{f}, Known{Seeds: []string{"rand"}}, Options{}), "fixtures are not checked unless asked")
}

func TestValidate_DuplicateFileNames(t *testing.T) {
	a := mustParse(t, "01.yml", "- name: a\n  method: GET\n  path: /x\n")
	ps := Validate([]File{a, a}, Known{}, Options{})
	require.Len(t, ps, 1)
	require.Equal(t, "01.yml: two files have this name", ps[0].String())
}

// "bearer" is the project-neutral way to send the captured token. A
// payment-gateway conversation (30 Sep) asked for "owner" logins because the
// format only knew the original suite's two role names; those stay accepted, meaning
// exactly bearer, so an imported suite validates unchanged. Each
// needs ${token} captured first, and an unknown value names what is accepted.
func TestValidate_BearerIsTheNeutralAuth(t *testing.T) {
	login := mustParse(t, "01_auth.yml", "- name: login\n  method: POST\n  path: /v1/login\n  expect_status: 200\n  capture:\n    token: data.token\n")
	for _, auth := range []string{"bearer", "owner", "barber"} {
		use := mustParse(t, "02_use.yml", "- name: use\n  method: GET\n  path: /v1/me\n  auth: "+auth+"\n  expect_status: 200\n")
		require.Empty(t, messages(Validate([]File{login, use}, Known{}, Options{})), auth)
		ps := Validate([]File{use}, Known{}, Options{})
		require.Len(t, ps, 1, auth)
		require.Contains(t, ps[0].Message, "${token}", "%s sends the captured token, so it needs one", auth)
	}
	odd := mustParse(t, "02_use.yml", "- name: use\n  method: GET\n  path: /v1/me\n  auth: merchant\n  expect_status: 200\n")
	ps := Validate([]File{odd}, Known{}, Options{})
	require.Len(t, ps, 1)
	require.Contains(t, ps[0].Message, "it is bearer (sends Bearer ${token}) or none")
}
