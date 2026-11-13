package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func fixClock(t *testing.T) {
	saved := now
	now = func() time.Time { return time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { now = saved })
}

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	return p
}

// writeSuite writes the scenario files into a directory of their own.
func writeSuite(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		writeFile(t, dir, name, body)
	}
	return dir
}

const (
	fileSecret      = "pw-from-file-1234"
	capturedToken   = "tok-captured-5678"
	uncapturedToken = "rt-uncaptured-9012"
)

// target is an API that signs in with the environment's secret and
// variable, then hands what it was given back in three failing answers.
func target(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
		case "/v1/auth/login":
			var b map[string]string
			_ = json.NewDecoder(r.Body).Decode(&b)
			if b["password"] != fileSecret || b["email"] != "qa@example.test" || b["ref"] != "run-0" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"data":{"session":"` + capturedToken + `","refresh_token":"` + uncapturedToken + `"}}`))
		case "/v1/echo-secret":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"echo":"` + fileSecret + `"}`))
		case "/v1/echo-session":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"held":"` + r.Header.Get("X-Session") + `"}`))
		case "/v1/echo-refresh":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"refresh_token":"` + uncapturedToken + `"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// fixture is a suite, its seeds and variables, and an environments file
// holding the one secret, for the environment "staging".
type fixture struct{ suite, seeds, vars, envs string }

func newFixture(t *testing.T) fixture {
	dir := t.TempDir()
	return fixture{
		suite: writeSuite(t, map[string]string{
			"01_login.yml": "- name: sign in\n  method: POST\n  path: /v1/auth/login\n" +
				"  request: {email: \"${admin_email}\", password: \"${admin_password}\", ref: \"run-${n}\"}\n" +
				"  expect_status: 200\n  capture: {session_token: data.session}\n",
			"02_echo.yml": "- name: echoes the secret\n  method: GET\n  path: /v1/echo-secret\n  expect_status: 200\n" +
				"- name: echoes the session\n  method: GET\n  path: /v1/echo-session\n  headers: {X-Session: \"${session_token}\"}\n  expect_status: 200\n" +
				"- name: echoes the refresh token\n  method: GET\n  path: /v1/echo-refresh\n  expect_status: 200\n",
		}),
		seeds: writeFile(t, dir, "seeds.yaml", "n: {value: \"0\"}\n"),
		vars:  writeFile(t, dir, "vars.yaml", "admin_email: qa@example.test\n"),
		envs:  writeFile(t, dir, "environments.yaml", "staging:\n  admin_password: \""+fileSecret+"\"\n"),
	}
}

func (f fixture) testArgs(baseURL string) []string {
	return []string{"test", "--suite", f.suite, "--env", "staging", "--base-url", baseURL,
		"--seeds", f.seeds, "--vars", f.vars, "--environments", f.envs}
}

// What `test` prints is what a person pastes into an issue, so nothing the
// runner holds as a credential may reach it: the environment's secret (masked
// by its value), a token a case captured under a *_token name (learned as
// the run goes), and a refresh_token no case captured (masked by its field's
// name). Each answer below puts one of them where only its own rule hides
// it, and the failing cases print those answers.
func TestTest_MasksEverySecretItSaw(t *testing.T) {
	fixClock(t)
	f := newFixture(t)
	var stdout, stderr bytes.Buffer
	code := run(f.testArgs(target(t).URL), &stdout, &stderr)
	out := stdout.String() + stderr.String()

	require.Equal(t, 1, code, out)
	require.Contains(t, out, "OK    POST /v1/auth/login → 200", "the sign-in used the file's secret, the variable and the seed")
	require.Equal(t, 3, strings.Count(out, "      response: "), "each failure prints the answer it got:\n%s", out)
	for _, line := range strings.Split(out, "\n") {
		require.NotContains(t, line, fileSecret, "the environment's secret, masked by its value")
		require.NotContains(t, line, capturedToken, "a token captured as session_token, masked once it is learned")
		require.NotContains(t, line, uncapturedToken, "a refresh_token nobody captured, masked by its field's name")
	}
	require.Contains(t, out, "FAILED: 1 ok, 0 skipped, 3 failed, 0 not run of 4")
}

// A secret the run needs and the host does not have fails the run before
// any request: a request without it fails for a reason nobody can see. The
// refusal names the secret and never a value.
func TestTest_AMissingSecretRefusesBeforeAnyRequest(t *testing.T) {
	fixClock(t)
	f := newFixture(t)
	writeFile(t, filepath.Dir(f.envs), "environments.yaml", "staging:\n  admin_password: { env: TB_TEST_NOT_SET_ANYWHERE }\n  other: \"other-value-123\"\n")
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	defer srv.Close()

	var stdout bytes.Buffer
	code := run(f.testArgs(srv.URL), &stdout, &bytes.Buffer{})
	require.Equal(t, 2, code)
	require.Zero(t, hits)
	require.Contains(t, stdout.String(), "REFUSED")
	require.Contains(t, stdout.String(), "secret admin_password of environment staging: variable TB_TEST_NOT_SET_ANYWHERE is not set")
	require.NotContains(t, stdout.String(), "other-value-123")
}

// Validating needs the secrets' names, not their values, so --env makes
// ${admin_password} known on a machine where it is not set. Without it the
// suite's use of the secret is a problem, reported as file:line, exit 1.
func TestValidate_KnowsTheEnvironmentsSecretsByName(t *testing.T) {
	f := newFixture(t)
	writeFile(t, filepath.Dir(f.envs), "environments.yaml", "staging:\n  admin_password: { env: TB_TEST_NOT_SET_ANYWHERE }\n")
	args := []string{"validate", "--suite", f.suite, "--seeds", f.seeds, "--vars", f.vars}

	var stdout bytes.Buffer
	require.Equal(t, 0, run(append(args, "--env", "staging", "--environments", f.envs), &stdout, &bytes.Buffer{}), stdout.String())
	require.Equal(t, "2 files, 4 cases: no problems\n", stdout.String())

	stdout.Reset()
	require.Equal(t, 1, run(args, &stdout, &bytes.Buffer{}))
	require.Contains(t, stdout.String(), "01_login.yml:1: ")
	require.Contains(t, stdout.String(), "admin_password")
}

// The image mounts the file at a fixed path; a binary on a laptop names it in
// TB_ENVIRONMENTS rather than on every command.
func TestValidate_ReadsTheEnvironmentsFileFromTBEnvironments(t *testing.T) {
	f := newFixture(t)
	t.Setenv("TB_ENVIRONMENTS", f.envs)
	var stdout, stderr bytes.Buffer
	code := run([]string{"validate", "--suite", f.suite, "--seeds", f.seeds, "--vars", f.vars, "--env", "staging"}, &stdout, &stderr)
	require.Equal(t, 0, code, stderr.String())
}

// A name is a seed, a variable or a secret, never two: which value won would
// depend on the order they were applied. An environment the file does not
// have is named, with the ones it has.
func TestValidate_RefusesWhatCannotBeLoaded(t *testing.T) {
	f := newFixture(t)
	dir := filepath.Dir(f.envs)
	clash := writeFile(t, dir, "clash.yaml", "admin_password: plain\n")
	seedClash := writeFile(t, dir, "seed-clash.yaml", "n: plain\n")
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"variable and secret": {[]string{"--vars", clash, "--env", "staging"}, "admin_password is both a variable and a secret of staging"},
		"seed and variable":   {[]string{"--seeds", f.seeds, "--vars", seedClash}, "n is both a seed and a variable"},
		"unknown environment": {[]string{"--env", "production"}, `no environment "production"; it has: staging`},
		"no suite":            {nil, "--suite is required"},
	} {
		t.Run(name, func(t *testing.T) {
			args := []string{"validate", "--environments", f.envs}
			if name != "no suite" {
				args = append(args, "--suite", f.suite)
			}
			var stderr bytes.Buffer
			require.Equal(t, 2, run(append(args, tc.args...), &bytes.Buffer{}, &stderr))
			require.Contains(t, stderr.String(), tc.want)
		})
	}
}

// test needs to know where to send requests and whose secrets to use; it
// never guesses either.
func TestTest_RequiresTheBaseURLAndTheEnvironment(t *testing.T) {
	f := newFixture(t)
	for _, args := range [][]string{
		{"test", "--suite", f.suite, "--env", "staging"},
		{"test", "--suite", f.suite, "--base-url", "http://127.0.0.1:1"},
	} {
		var stderr bytes.Buffer
		require.Equal(t, 2, run(args, &bytes.Buffer{}, &stderr))
		require.Contains(t, stderr.String(), "--base-url and --env are required")
	}
}
