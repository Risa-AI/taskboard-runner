package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Risa-AI/taskboard-runner/pkg/protocol"
	"github.com/stretchr/testify/require"
)

const (
	fileSecret      = "pw-from-file-1234"
	capturedToken   = "tok-captured-5678"
	uncapturedToken = "rt-uncaptured-9012"
	fleetToken      = "fleet-token-abcdef"
	fixtureBody     = "%PNG-not-really"
)

// fakeTaskboard answers /v1/runner/* as docs/protocol.md says, and keeps
// everything it was sent.
type fakeTaskboard struct {
	t   *testing.T
	srv *httptest.Server

	mu        sync.Mutex
	jobs      []protocol.Job
	claims    int
	reports   []protocol.EnvironmentsReport
	beats     int
	cancel    map[string]bool
	ended     map[string]bool
	cases     map[string]map[int]protocol.CaseResult
	posts     map[string]map[int]int
	completes map[string]protocol.Complete
	bodies    []string
	badHeader int
	// dropCases stores the next N case posts, then drops the connection
	// without an answer: the runner must send them again.
	dropCases int
	// refuse answers every call with this status, when set.
	refuse   int
	refusalM string
	done     chan string
}

func newTaskboard(t *testing.T) *fakeTaskboard {
	tb := &fakeTaskboard{
		t: t, cancel: map[string]bool{}, ended: map[string]bool{},
		cases: map[string]map[int]protocol.CaseResult{}, posts: map[string]map[int]int{},
		completes: map[string]protocol.Complete{}, done: make(chan string, 8),
	}
	tb.srv = httptest.NewServer(http.HandlerFunc(tb.serve))
	t.Cleanup(tb.srv.Close)
	return tb
}

func (tb *fakeTaskboard) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	tb.mu.Lock()
	tb.bodies = append(tb.bodies, string(body))
	if r.Header.Get(protocol.TokenHeader) != fleetToken || r.Header.Get(protocol.VersionHeader) != "1" {
		tb.badHeader++
	}
	refuse, refusalM := tb.refuse, tb.refusalM
	tb.mu.Unlock()
	if refuse != 0 {
		w.WriteHeader(refuse)
		_ = json.NewEncoder(w).Encode(protocol.Envelope[*struct{}]{Error: &protocol.Error{Code: "X", Message: refusalM}})
		return
	}
	path := strings.TrimPrefix(r.URL.Path, protocol.BasePath)
	switch {
	case path == "/environments" && r.Method == http.MethodPut:
		var rep protocol.EnvironmentsReport
		require.NoError(tb.t, json.Unmarshal(body, &rep))
		tb.mu.Lock()
		tb.reports = append(tb.reports, rep)
		tb.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case path == "/heartbeat":
		tb.mu.Lock()
		tb.beats++
		answer := protocol.HeartbeatAnswer{RunsToCancel: []string{}}
		for id := range tb.cancel {
			answer.RunsToCancel = append(answer.RunsToCancel, id)
		}
		tb.mu.Unlock()
		_ = json.NewEncoder(w).Encode(protocol.Envelope[protocol.HeartbeatAnswer]{Success: true, Data: answer})
	case path == "/claim":
		tb.mu.Lock()
		tb.claims++
		var job *protocol.Job
		if len(tb.jobs) > 0 {
			job = &tb.jobs[0]
			tb.jobs = tb.jobs[1:]
		}
		tb.mu.Unlock()
		if job == nil {
			time.Sleep(20 * time.Millisecond) // taskboard holds an empty claim
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_ = json.NewEncoder(w).Encode(protocol.Envelope[*protocol.Job]{Success: true, Data: job})
	case strings.HasPrefix(path, "/runs/") && strings.HasSuffix(path, "/cases"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/runs/"), "/cases")
		var cs []protocol.CaseResult
		require.NoError(tb.t, json.Unmarshal(body, &cs))
		tb.mu.Lock()
		if tb.ended[id] {
			tb.mu.Unlock()
			w.WriteHeader(http.StatusConflict)
			return
		}
		if tb.cases[id] == nil {
			tb.cases[id], tb.posts[id] = map[int]protocol.CaseResult{}, map[int]int{}
		}
		for _, c := range cs {
			tb.cases[id][c.Index] = c // kept once per (run, index)
			tb.posts[id][c.Index]++
		}
		drop := tb.dropCases > 0
		if drop {
			tb.dropCases--
		}
		tb.mu.Unlock()
		if drop {
			conn, _, err := w.(http.Hijacker).Hijack()
			require.NoError(tb.t, err)
			conn.Close()
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case strings.HasPrefix(path, "/runs/") && strings.HasSuffix(path, "/complete"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/runs/"), "/complete")
		var c protocol.Complete
		require.NoError(tb.t, json.Unmarshal(body, &c))
		tb.mu.Lock()
		tb.completes[id] = c
		tb.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		tb.done <- id
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (tb *fakeTaskboard) addJob(j protocol.Job) {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	tb.jobs = append(tb.jobs, j)
}

func (tb *fakeTaskboard) waitDone(t *testing.T, id string) protocol.Complete {
	t.Helper()
	select {
	case got := <-tb.done:
		require.Equal(t, id, got)
	case <-time.After(10 * time.Second):
		t.Fatalf("run %s never completed", id)
	}
	tb.mu.Lock()
	defer tb.mu.Unlock()
	return tb.completes[id]
}

// eventually polls cond until it holds or ten seconds pass.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// target is the customer's API: it signs in with the environment's secret
// and variable, takes an upload, and hands back what it was given.
type target struct {
	srv  *httptest.Server
	mu   sync.Mutex
	hits int
	cut  chan struct{}
}

func newTarget(t *testing.T) *target {
	tg := &target{cut: make(chan struct{}, 1)}
	tg.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			tg.mu.Lock()
			tg.hits++
			tg.mu.Unlock()
		}
		switch r.URL.Path {
		case "/health":
		case "/login":
			var b map[string]string
			_ = json.NewDecoder(r.Body).Decode(&b)
			if b["password"] != fileSecret || b["email"] != "qa@example.test" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"session":"` + capturedToken + `","refresh_token":"` + uncapturedToken + `"}`))
		case "/upload":
			f, _, err := r.FormFile("proof")
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			got, _ := io.ReadAll(f)
			if string(got) != fixtureBody {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
		case "/echo-secret":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"echo":"` + fileSecret + `"}`))
		case "/echo-session":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"held":"` + r.Header.Get("X-Session") + `"}`))
		case "/echo-refresh":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"refresh_token":"` + uncapturedToken + `"}`))
		case "/slow":
			select {
			case <-r.Context().Done():
				tg.cut <- struct{}{}
			case <-time.After(5 * time.Second):
			}
		}
	}))
	t.Cleanup(tg.srv.Close)
	return tg
}

func (tg *target) hitCount() int {
	tg.mu.Lock()
	defer tg.mu.Unlock()
	return tg.hits
}

// fixtureServer serves the one fixture, and says whether the fleet token
// was ever sent to it.
func fixtureServer(t *testing.T, sawToken *bool) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(protocol.TokenHeader) != "" {
			*sawToken = true
		}
		_, _ = w.Write([]byte(fixtureBody))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func envFile(t *testing.T, body string) string {
	p := filepath.Join(t.TempDir(), "environments.yaml")
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	return p
}

const loginCase = "- name: sign in\n  method: POST\n  path: /login\n" +
	"  request: {email: \"${admin_email}\", password: \"${admin_password}\"}\n" +
	"  expect_status: 200\n  capture: {session_token: session}\n"

func job(id, baseURL string, files ...protocol.File) protocol.Job {
	return protocol.Job{
		RunID:       id,
		Suite:       protocol.Suite{Files: files},
		Environment: protocol.JobEnvironment{Name: "staging", BaseURL: baseURL, Variables: map[string]string{"admin_email": "qa@example.test"}},
		TimeoutS:    60,
		LeaseS:      120,
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// start runs the runner against tb until the test ends, and returns its log
// and its result.
func start(t *testing.T, tb *fakeTaskboard, envs string) (*syncBuffer, chan error) {
	log := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan error, 1)
	go func() {
		out <- Run(ctx, Config{
			URL: tb.srv.URL, Token: fleetToken, Slots: 1, Environments: envs, Version: "v0.0.0-test", Log: log,
			HeartbeatEvery: 20 * time.Millisecond, RetryBase: 5 * time.Millisecond, RetryMax: 20 * time.Millisecond,
		})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-out:
		case <-time.After(10 * time.Second):
			t.Error("the runner did not stop")
		}
	})
	return log, out
}

// A whole run, as a customer's fleet does it: the environments are
// reported by name, the job is claimed, its fixture downloaded into a
// directory that is gone afterwards, every case posted with its index, and
// the run completed. Every call carries the token and the protocol version,
// and the fixture's link is never sent the token.
func TestRun_ClaimsRunsReportsEveryCaseAndCompletes(t *testing.T) {
	envs := envFile(t, "staging:\n  admin_password: \""+fileSecret+"\"\n")
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp) // the run's directory is made here, and must be gone after
	tb, tg := newTaskboard(t), newTarget(t)
	var sawToken bool
	fx := fixtureServer(t, &sawToken)
	j := job("run-1", tg.srv.URL,
		protocol.File{Name: "01_login.yml", Content: loginCase},
		protocol.File{Name: "02_upload.yml", Content: "- name: upload\n  method: POST\n  path: /upload\n  form: {proof: \"@fixtures/proof.png\"}\n  expect_status: 200\n"},
	)
	j.Suite.Fixtures = []protocol.Fixture{{Name: "fixtures/proof.png", URL: fx.URL + "/f/1?sig=abc"}}
	tb.addJob(j)

	start(t, tb, envs)
	c := tb.waitDone(t, "run-1")

	require.Equal(t, protocol.Summary{Verdict: "passed", Total: 2, OK: 2}, c.Summary, c.Error)
	require.Empty(t, c.Error)
	tb.mu.Lock()
	require.Equal(t, protocol.EnvironmentsReport{{Name: "staging", Secrets: []string{"admin_password"}}}, tb.reports[0])
	require.Len(t, tb.cases["run-1"], 2)
	require.Equal(t, "upload", tb.cases["run-1"][1].Name)
	require.Equal(t, "ok", tb.cases["run-1"][1].Outcome)
	require.Zero(t, tb.badHeader, "every call carries the fleet token and the protocol version")
	tb.mu.Unlock()
	require.False(t, sawToken, "the fleet token goes to taskboard only")
	eventually(t, "the run's directory to be removed", func() bool {
		left, _ := os.ReadDir(tmp)
		return len(left) == 0
	})
	eventually(t, "a heartbeat", func() bool { tb.mu.Lock(); defer tb.mu.Unlock(); return tb.beats > 0 })
}

// Nothing the runner posts may hold a secret: not the environment's secret
// the target echoes back, not a token a case captured under a *_token name,
// and not a refresh_token no case captured. Each failing case below puts one
// of them in the answer the runner posts.
func TestRun_NoSecretInAnythingPosted(t *testing.T) {
	tb, tg := newTaskboard(t), newTarget(t)
	tb.addJob(job("run-2", tg.srv.URL,
		protocol.File{Name: "01_login.yml", Content: loginCase},
		protocol.File{Name: "02_echo.yml", Content: "" +
			"- name: echoes the secret\n  method: GET\n  path: /echo-secret\n  expect_status: 200\n" +
			"- name: echoes the session\n  method: GET\n  path: /echo-session\n  headers: {X-Session: \"${session_token}\"}\n  expect_status: 200\n" +
			"- name: echoes the refresh token\n  method: GET\n  path: /echo-refresh\n  expect_status: 200\n"},
	))
	log, _ := start(t, tb, envFile(t, "staging:\n  admin_password: \""+fileSecret+"\"\n"))
	c := tb.waitDone(t, "run-2")
	require.Equal(t, "failed", c.Summary.Verdict)
	require.Equal(t, 3, c.Summary.Failed)

	tb.mu.Lock()
	defer tb.mu.Unlock()
	for i := 1; i <= 3; i++ {
		require.Equal(t, "fail", tb.cases["run-2"][i].Outcome)
		require.Contains(t, tb.cases["run-2"][i].Response, "[redacted]", "each failure posts the answer it got, masked")
	}
	for _, body := range append(tb.bodies, log.String()) {
		require.NotContains(t, body, fileSecret, "the environment's secret")
		require.NotContains(t, body, capturedToken, "a token captured as session_token")
		require.NotContains(t, body, uncapturedToken, "a refresh_token nobody captured")
		require.NotContains(t, body, fleetToken, "the fleet token")
	}
}

// A run cancelled in taskboard is named on the next heartbeat. The runner
// cuts off the request in flight, posts nothing more about the run and does
// not complete it -- taskboard already ended it -- and goes back to claiming.
func TestRun_ACancelFromTheHeartbeatStopsTheRun(t *testing.T) {
	tb, tg := newTaskboard(t), newTarget(t)
	tb.addJob(job("run-3", tg.srv.URL, protocol.File{Name: "01.yml", Content: "" +
		"- name: slow\n  method: GET\n  path: /slow\n  expect_status: 200\n" +
		"- name: after\n  method: GET\n  path: /after\n  expect_status: 200\n"}))
	start(t, tb, envFile(t, "staging:\n  admin_password: \""+fileSecret+"\"\n"))

	eventually(t, "the run to start", func() bool { return tg.hitCount() > 0 })
	tb.mu.Lock()
	tb.cancel["run-3"] = true
	tb.mu.Unlock()
	select {
	case <-tg.cut:
	case <-time.After(3 * time.Second):
		t.Fatal("the request in flight was not cut off")
	}
	eventually(t, "the runner to claim again", func() bool { tb.mu.Lock(); defer tb.mu.Unlock(); return tb.claims >= 3 })

	tb.mu.Lock()
	defer tb.mu.Unlock()
	require.Empty(t, tb.cases["run-3"], "nothing is posted about a cancelled run")
	require.NotContains(t, tb.completes, "run-3")
	require.Equal(t, 1, tg.hits, "the case after the cancel never ran")
}

// A run that already ended in taskboard (409 on a case) is stopped the same
// way: no more cases, no complete.
func TestRun_ARunEndedInTaskboardIsStopped(t *testing.T) {
	tb, tg := newTaskboard(t), newTarget(t)
	tb.ended["run-4"] = true
	tb.addJob(job("run-4", tg.srv.URL, protocol.File{Name: "01.yml", Content: "" +
		"- name: a\n  method: GET\n  path: /a\n  expect_status: 200\n" +
		"- name: b\n  method: GET\n  path: /b\n  expect_status: 200\n"}))
	start(t, tb, envFile(t, "staging:\n  admin_password: \""+fileSecret+"\"\n"))
	eventually(t, "the runner to claim again", func() bool { tb.mu.Lock(); defer tb.mu.Unlock(); return tb.claims >= 3 })
	tb.mu.Lock()
	defer tb.mu.Unlock()
	require.NotContains(t, tb.completes, "run-4")
	require.Equal(t, 1, tg.hits, "case b never ran")
}

// A connection lost while posting a case is not a lost case: the runner
// sends it again until taskboard answers, and since taskboard keeps a case
// once per (run, index), the repeats change nothing.
func TestRun_ALostConnectionMidRunIsRetried(t *testing.T) {
	tb, tg := newTaskboard(t), newTarget(t)
	tb.dropCases = 2
	tb.addJob(job("run-5", tg.srv.URL, protocol.File{Name: "01.yml", Content: "" +
		"- name: a\n  method: GET\n  path: /a\n  expect_status: 200\n" +
		"- name: b\n  method: GET\n  path: /b\n  expect_status: 200\n"}))
	start(t, tb, envFile(t, "staging:\n  admin_password: \""+fileSecret+"\"\n"))
	c := tb.waitDone(t, "run-5")
	require.Equal(t, protocol.Summary{Verdict: "passed", Total: 2, OK: 2}, c.Summary)
	tb.mu.Lock()
	defer tb.mu.Unlock()
	require.Len(t, tb.cases["run-5"], 2)
	require.Equal(t, 3, tb.posts["run-5"][0], "case 0 was sent until it was answered")
	require.Equal(t, 1, tb.posts["run-5"][1])
}

// A secret the suite uses that this host does not have, or has empty, fails
// the run before any request reaches the target: a request without it would
// fail for a reason nobody could see. The reason names the secret, never a
// value.
func TestRun_AMissingSecretFailsTheRunBeforeAnyRequest(t *testing.T) {
	for name, file := range map[string]string{
		"not in the file": "staging:\n  other: \"other-value-123\"\n",
		"left empty":      "staging:\n  admin_password: \"\"\n  other: \"other-value-123\"\n",
		"variable unset":  "staging:\n  admin_password: { env: TB_TEST_NOT_SET_ANYWHERE }\n  other: \"other-value-123\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			tb, tg := newTaskboard(t), newTarget(t)
			tb.addJob(job("run-6", tg.srv.URL, protocol.File{Name: "01_login.yml", Content: loginCase}))
			start(t, tb, envFile(t, file))
			c := tb.waitDone(t, "run-6")
			require.Equal(t, "refused", c.Summary.Verdict)
			require.Equal(t, 1, c.Summary.NotRun)
			require.Contains(t, c.Error, "admin_password")
			require.NotContains(t, c.Error, "other-value-123")
			require.Zero(t, tg.hitCount())
		})
	}
}

// A run past its time limit is stopped and completed as failed, saying why,
// with the cases it never reached counted as not run.
func TestRun_ARunPastItsTimeLimitIsStopped(t *testing.T) {
	tb, tg := newTaskboard(t), newTarget(t)
	j := job("run-7", tg.srv.URL, protocol.File{Name: "01.yml", Content: "" +
		"- name: slow\n  method: GET\n  path: /slow\n  expect_status: 200\n" +
		"- name: after\n  method: GET\n  path: /after\n  expect_status: 200\n"})
	j.TimeoutS = 1
	tb.addJob(j)
	start(t, tb, envFile(t, "staging:\n  admin_password: \""+fileSecret+"\"\n"))
	c := tb.waitDone(t, "run-7")
	require.Equal(t, "failed", c.Summary.Verdict)
	require.Equal(t, 2, c.Summary.NotRun)
	require.Contains(t, c.Error, "time limit of 1s")
	require.Equal(t, 1, tg.hitCount())
}

// A refused token or a protocol too old stops the runner itself, with
// taskboard's words and never the token: no run could succeed, and retrying
// would only hammer taskboard.
func TestRun_ARefusedTokenOrProtocolStopsTheRunner(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusUpgradeRequired} {
		tb := newTaskboard(t)
		tb.refuse, tb.refusalM = status, "update the runner to v0.3.0 or newer"
		_, out := start(t, tb, envFile(t, "staging:\n  admin_password: \""+fileSecret+"\"\n"))
		select {
		case err := <-out:
			require.Error(t, err)
			require.Contains(t, err.Error(), "update the runner to v0.3.0 or newer")
			require.NotContains(t, err.Error(), fleetToken)
			out <- err // for the cleanup
		case <-time.After(5 * time.Second):
			t.Fatalf("%d did not stop the runner", status)
		}
	}
}

// A secret added to the file while the runner is up reaches taskboard on a
// later heartbeat, so a run needing it is not refused for a fleet that has
// it. Names only.
func TestRun_TheEnvironmentsAreReportedAgainWhenTheFileChanges(t *testing.T) {
	tb := newTaskboard(t)
	p := envFile(t, "staging:\n  admin_password: \""+fileSecret+"\"\n")
	start(t, tb, p)
	eventually(t, "the first report", func() bool { tb.mu.Lock(); defer tb.mu.Unlock(); return len(tb.reports) == 1 })
	require.NoError(t, os.WriteFile(p, []byte("staging:\n  admin_password: \""+fileSecret+"\"\n  api_key: \"key-value-123\"\n"), 0o600))
	eventually(t, "a second report", func() bool { tb.mu.Lock(); defer tb.mu.Unlock(); return len(tb.reports) == 2 })
	tb.mu.Lock()
	defer tb.mu.Unlock()
	require.Equal(t, protocol.EnvironmentsReport{{Name: "staging", Secrets: []string{"admin_password", "api_key"}}}, tb.reports[1])
}

// The token travels on every call, so it never goes over plain HTTP to
// another host; a runner missing its address or token says which.
func TestConfig_Check(t *testing.T) {
	for want, cfg := range map[string]Config{
		"TB_URL is not set":    {Token: "t", Slots: 1},
		"TB_URL must be https": {URL: "http://taskboard.example.com", Token: "t", Slots: 1},
		"TB_FLEET_TOKEN":       {URL: "https://taskboard.example.com", Slots: 1},
		"TB_SLOTS":             {URL: "https://taskboard.example.com", Token: "t"},
	} {
		require.ErrorContains(t, cfg.Check(), want)
	}
	require.NoError(t, Config{URL: "http://127.0.0.1:8080", Token: "t", Slots: 1}.Check(), "plain HTTP to this host is allowed")
	require.NoError(t, Config{URL: "https://taskboard.example.com/", Token: "t", Slots: 2}.Check())
}

// A runner stopped mid-run (Ctrl-C, docker stop) says so for the run in
// progress, instead of leaving it to be failed as silent when its lease
// runs out.
func TestRun_StoppingTheRunnerReportsTheRunInProgress(t *testing.T) {
	tb, tg := newTaskboard(t), newTarget(t)
	tb.addJob(job("run-8", tg.srv.URL, protocol.File{Name: "01.yml", Content: "- name: slow\n  method: GET\n  path: /slow\n  expect_status: 200\n"}))
	ctx, stop := context.WithCancel(context.Background())
	out := make(chan error, 1)
	go func() {
		out <- Run(ctx, Config{URL: tb.srv.URL, Token: fleetToken, Slots: 1, Environments: envFile(t, "staging:\n  admin_password: \""+fileSecret+"\"\n"),
			HeartbeatEvery: 20 * time.Millisecond, RetryBase: 5 * time.Millisecond, RetryMax: 20 * time.Millisecond})
	}()
	eventually(t, "the run to start", func() bool { return tg.hitCount() > 0 })
	stop()
	c := tb.waitDone(t, "run-8")
	require.Equal(t, "failed", c.Summary.Verdict)
	require.Equal(t, 1, c.Summary.NotRun)
	require.Contains(t, c.Error, "the runner was stopped")
	select {
	case err := <-out:
		require.NoError(t, err, "a runner stopped on purpose exits cleanly")
	case <-time.After(10 * time.Second):
		t.Fatal("the runner did not stop")
	}
}

// The image runs as its own user, so a host file kept to its owner is
// unreadable unless the container runs as that owner. The refusal says how.
func TestRun_AnUnreadableFileSaysHowToRunTheContainer(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads any file")
	}
	p := envFile(t, "staging:\n  admin_password: \""+fileSecret+"\"\n")
	require.NoError(t, os.Chmod(p, 0o000))
	err := Run(context.Background(), Config{URL: "http://127.0.0.1:1", Token: fleetToken, Slots: 1, Environments: p})
	require.ErrorContains(t, err, `--user "$(id -u):$(id -g)"`)
}
