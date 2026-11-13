package apitest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fastRunner is a runner whose waits cost nothing, recording what it slept.
func fastRunner(url string) (*Runner, *[]time.Duration) {
	r := NewRunner(url)
	var slept []time.Duration
	r.Sleep = func(d time.Duration) { slept = append(slept, d) }
	return r, &slept
}

// Ported from the original suite's TestRunnerEndToEnd: a value captured by one case
// substitutes into the next, and `auth: owner` sends the captured token. This
// is the whole reason the suite is one sequence.
func TestRun_CapturesFlowIntoLaterCases(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/auth/login", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["email"] != "e2e-abc@example.com" {
			t.Errorf("substitution failed, got email=%q", body["email"])
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"data":    map[string]interface{}{"token": "jwt-123", "user": map[string]interface{}{"id": "usr_1"}, "shops": []interface{}{map[string]interface{}{"id": "shop_9"}}},
		})
	})
	mux.HandleFunc("/v1/auth/me", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer jwt-123" {
			t.Errorf("auth header not injected, got %q", got)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	r, _ := fastRunner(srv.URL)
	r.Seed("rand", "abc")
	require.NoError(t, r.Ping())

	results := r.Run([]Case{
		{Name: "login", Method: "POST", Path: "/v1/auth/login",
			Request:      map[string]interface{}{"email": "e2e-${rand}@example.com", "password": "password123"},
			ExpectStatus: 200,
			Capture:      map[string]string{"token": "data.token", "user_id": "data.user.id", "shop_id": "data.shops.0.id"}},
		{Name: "me", Method: "GET", Path: "/v1/auth/me", Auth: "owner", ExpectStatus: 200},
	})
	for _, res := range results {
		require.Equal(t, OutcomeOK, res.Outcome, "%s: %s", res.Name, res.Reason)
	}
	require.Equal(t, "jwt-123", r.Vars["token"])
	require.Equal(t, "usr_1", r.Vars["user_id"])
	require.Equal(t, "shop_9", r.Vars["shop_id"], "a number segment indexes an array")
}

// Ported from the original suite's TestSubstituteAny, plus the number rule: a leaf that
// is exactly one number variable is a JSON number, because a server decoding
// `days` into []int refuses ["5"] with an error about something else.
func TestSubstituteAny_NestedAndNumbers(t *testing.T) {
	r := NewRunner("")
	r.Seed("id", "42")
	r.SeedNumber("dow", 5)

	out := r.substituteAny(map[string]interface{}{
		"shop_id": "${id}",
		"nested":  map[string]interface{}{"list": []interface{}{"${id}", "static"}},
		"days":    []interface{}{"${dow}"},
		"label":   "day ${dow}",
	})
	b, _ := json.Marshal(out)
	require.JSONEq(t, `{"nested":{"list":["42","static"]},"shop_id":"42","days":[5],"label":"day 5"}`, string(b))
	require.Equal(t, "${unknown}", r.substitute("${unknown}"), "an unknown variable stays visible in the failure")
}

// Ported from the original suite's TestRequiresVarsSkipsOnlyWhenAbsent. Both halves
// fail in opposite directions: a missing credential that did not skip turns a
// suite red for a reason nobody can fix from the repository, and a present one
// that still skipped reports green while asserting nothing.
func TestRun_RequiresVarsSkipsOnlyWhenAbsent(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
	}))
	defer srv.Close()
	newCase := func() Case {
		return Case{Name: "admin login", Method: "POST", Path: "/v1/admin/auth/login", ExpectStatus: 200, RequiresVars: []string{"adminEmail", "adminPassword"}}
	}

	for _, tt := range []struct{ name, email, password string }{
		{"unset", "", ""},
		{"one of two blank", "admin@example.com", ""},
		{"whitespace only", "admin@example.com", "   "},
	} {
		t.Run(tt.name, func(t *testing.T) {
			hits = 0
			r, _ := fastRunner(srv.URL)
			r.Seed("adminEmail", tt.email)
			r.Seed("adminPassword", tt.password)
			res := r.Run([]Case{newCase()})
			require.Equal(t, OutcomeSkip, res[0].Outcome)
			require.Contains(t, res[0].Reason, "requires_vars", "the skip says which variable to supply")
			require.Zero(t, hits, "the server must not be called for a skipped case")
		})
	}

	hits = 0
	r, _ := fastRunner(srv.URL)
	r.Seed("adminEmail", "admin@example.com")
	r.Seed("adminPassword", "s3cret")
	res := r.Run([]Case{newCase()})
	require.Equal(t, OutcomeOK, res[0].Outcome, "supplied credentials must run the case, not skip it")
	require.Equal(t, 1, hits)
}

// An unreachable target is an error, never a skip. The original runner skipped when no
// target was named, and a nightly that could not reach staging reported green
// while asserting nothing; a runner started for a named target has no reason
// to skip.
func TestPing_AnUnreachableTargetIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	r := NewRunner(url)
	err := r.Ping()
	require.ErrorIs(t, err, ErrUnreachable)

	up := httptest.NewServer(http.NotFoundHandler())
	defer up.Close()
	require.NoError(t, NewRunner(up.URL).Ping(), "a 404 on /health is a healthy API behind a gateway that routes only /v1")
}

// local_only follows the target's flag, not the URL. The original runner looked for
// "localhost" in the URL, so a local stack reached as http://devbox:8080
// skipped its local cases and a deployed host with localhost in its name
// would have run them.
func TestRun_LocalOnlyFollowsTheTargetsFlag(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits++ }))
	defer srv.Close()
	require.Contains(t, srv.URL, "127.0.0.1")

	c := Case{Name: "health", Method: "GET", Path: "/health", LocalOnly: true, ExpectStatus: 200}
	r, _ := fastRunner(srv.URL)
	res := r.Run([]Case{c})
	require.Equal(t, OutcomeSkip, res[0].Outcome, "a 127.0.0.1 URL is not a local target unless the target says so")
	require.Zero(t, hits)

	r.Local = true
	res = r.Run([]Case{c})
	require.Equal(t, OutcomeOK, res[0].Outcome)
	require.Equal(t, 1, hits)
}

// A 429 is backed off to the advertised reset and retried twice, then fails;
// a case that expects the 429 gets it at once. The original suite's auth burst budget
// is 12 a minute and a suite spends that in seconds.
func TestRun_BacksOffA429ThenFails(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.Header().Set("X-Ratelimit-Reset", "1000030") // 30 s after the fake now
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	r, slept := fastRunner(srv.URL)
	r.Now = func() time.Time { return time.Unix(1000000, 0) }
	res := r.Run([]Case{{Name: "login", Method: "POST", Path: "/v1/auth/login", ExpectStatus: 200}})
	require.Equal(t, OutcomeFail, res[0].Outcome)
	require.Equal(t, 3, hits, "one try and two retries, then give up rather than hang")
	require.Equal(t, []time.Duration{31 * time.Second, 31 * time.Second}, *slept, "the wait is the advertised reset plus a second")

	hits = 0
	r2, slept2 := fastRunner(srv.URL)
	res = r2.Run([]Case{{Name: "lockout", Method: "POST", Path: "/v1/auth/login", ExpectStatus: 429}})
	require.Equal(t, OutcomeOK, res[0].Outcome)
	require.Equal(t, 1, hits)
	require.Empty(t, *slept2)
}

// A run goes on past a failure, holding back only what depended on it. The
// run used to stop at the first failure, so each run showed one: a suite with
// several guessed statuses took a run apiece, and a model allowed three runs
// a turn could not get through one. A case using what a failed case should
// have captured is still not sent -- it would fail on the literal ${name}
// and bury the real failure -- and neither is anything that needed its
// captures in turn; each says which case it waited on. A later case that
// captures the same name again frees what comes after it.
func TestRun_GoesOnPastAFailureHoldingBackItsDependents(t *testing.T) {
	var sent []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent = append(sent, r.URL.Path)
		switch r.URL.Path {
		case "/bad", "/login-bad":
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`{"error":"boom"}`))
		case "/login":
			_, _ = w.Write([]byte(`{"data":{"token":"t2"}}`))
		default:
			_, _ = w.Write([]byte(`{"data":{"id":"x1"}}`))
		}
	}))
	defer srv.Close()
	r, _ := fastRunner(srv.URL)
	var reported []string
	r.OnResult = func(res Result) { reported = append(reported, res.Name+":"+string(res.Outcome)) }

	res := r.Run([]Case{
		{Name: "a", Method: "GET", Path: "/ok", ExpectStatus: 200, File: "01.yml", Line: 1},
		{Name: "b", Method: "GET", Path: "/bad", ExpectStatus: 200, File: "01.yml", Line: 5},
		{Name: "unrelated", Method: "GET", Path: "/ok", ExpectStatus: 200},
		{Name: "login", Method: "POST", Path: "/login-bad", ExpectStatus: 200, File: "02.yml", Line: 1, Capture: map[string]string{"token": "data.token"}},
		{Name: "as bearer", Method: "GET", Path: "/me", Auth: "bearer", ExpectStatus: 200, Capture: map[string]string{"me_id": "data.id"}},
		{Name: "uses me_id", Method: "GET", Path: "/users/${me_id}", ExpectStatus: 200},
		{Name: "login again", Method: "POST", Path: "/login", ExpectStatus: 200, Capture: map[string]string{"token": "data.token"}},
		{Name: "as bearer after", Method: "GET", Path: "/me", Auth: "bearer", ExpectStatus: 200},
	})
	require.Len(t, res, 8)
	require.Equal(t, "expected status 200, got 500", res[1].Reason)
	require.Equal(t, `{"error":"boom"}`, res[1].Response, "a failure keeps its response")
	require.Contains(t, res[1].Request, "GET /bad")
	require.Equal(t, "01.yml", res[1].File)
	require.Equal(t, 5, res[1].Line)
	require.Equal(t, OutcomeOK, res[2].Outcome, "a failure that captured nothing holds nothing back")

	require.Equal(t, OutcomeSkip, res[4].Outcome)
	require.Equal(t, "not run: it needs ${token}, which 02.yml:1 (login) should have captured", res[4].Reason)
	require.Equal(t, OutcomeSkip, res[5].Outcome, "held back in turn: me_id was never captured")
	require.Contains(t, res[5].Reason, "${me_id}")
	require.Equal(t, OutcomeOK, res[7].Outcome, "a fresh capture of token frees what follows")
	require.Equal(t, []string{"/ok", "/bad", "/ok", "/login-bad", "/login", "/me"}, sent, "nothing held back was sent")
	require.Equal(t, []string{"a:ok", "b:fail", "unrelated:ok", "login:fail", "as bearer:skip", "uses me_id:skip", "login again:ok", "as bearer after:ok"}, reported)
}

// Each case gets its own X-Forwarded-For, and a case that pins its own wins
// -- the only way a lockout scenario can keep its attempts in one bucket.
// A doubled scheme in a case's own Authorization is repaired; a single one is
// left alone, so "Bearer not.a.jwt" still tests a bad token.
func TestRun_HeadersPerCase(t *testing.T) {
	var seen []http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Clone())
	}))
	defer srv.Close()
	r, _ := fastRunner(srv.URL)
	r.Seed("support_token", "bearer jwt-admin")

	r.Run([]Case{
		{Name: "a", Method: "GET", Path: "/a"},
		{Name: "b", Method: "GET", Path: "/b", Headers: map[string]string{"X-Forwarded-For": "10.9.9.9", "Authorization": "Bearer ${support_token}"}},
		{Name: "c", Method: "GET", Path: "/c", Headers: map[string]string{"Authorization": "Bearer not.a.jwt"}},
	})
	require.Len(t, seen, 3)
	require.Equal(t, "10.0.0.1", seen[0].Get("X-Forwarded-For"))
	require.Equal(t, "10.9.9.9", seen[1].Get("X-Forwarded-For"))
	require.Equal(t, "Bearer jwt-admin", seen[1].Get("Authorization"))
	require.Equal(t, "Bearer not.a.jwt", seen[2].Get("Authorization"))
	require.Equal(t, "10.0.0.3", seen[2].Get("X-Forwarded-For"))
}

// A form value "@path" is a fixture read from under Base, sent with the path
// as its file name; one that climbs out of Base is refused before anything is
// read.
func TestRun_FormUploadsAFixture(t *testing.T) {
	base := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(base, "fixtures"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(base, "fixtures", "logo.png"), []byte("PNGDATA"), 0o600))

	// The raw Content-Disposition, because Go's own form parser keeps only the
	// base name and would hide what was sent: the path, as the original runner sends it.
	var gotName, gotData, gotField string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mr, err := r.MultipartReader()
		require.NoError(t, err)
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			require.NoError(t, err)
			b, _ := io.ReadAll(part)
			switch part.FormName() {
			case "logo":
				gotName, gotData = part.Header.Get("Content-Disposition"), string(b)
			case "caption":
				gotField = string(b)
			}
		}
	}))
	defer srv.Close()

	r, _ := fastRunner(srv.URL)
	r.Base = base
	r.Seed("rand", "xyz")
	res := r.Run([]Case{{Name: "upload", Method: "POST", Path: "/logo", ExpectStatus: 200,
		Form: map[string]string{"logo": "@fixtures/logo.png", "caption": "shop ${rand}"}}})
	require.Equal(t, OutcomeOK, res[0].Outcome, res[0].Reason)
	require.Contains(t, gotName, `filename="fixtures/logo.png"`)
	require.Equal(t, "PNGDATA", gotData)
	require.Equal(t, "shop xyz", gotField)

	res = r.Run([]Case{{Name: "escape", Method: "POST", Path: "/logo", Form: map[string]string{"f": "@../../etc/passwd"}}})
	require.Equal(t, OutcomeFail, res[0].Outcome)
	require.Contains(t, res[0].Reason, "leaves the suite's directory")
}

// A transport failure mid-run is that case's failure, with what was sent.
func TestRun_ATransportFailureFailsTheCase(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	r, _ := fastRunner(url)
	res := r.Run([]Case{{Name: "x", Method: "GET", Path: "/x"}})
	require.Equal(t, OutcomeFail, res[0].Outcome)
	require.True(t, strings.Contains(res[0].Reason, "connect") || strings.Contains(res[0].Reason, "refused"), res[0].Reason)
}

// bearer sends the captured token, as the original suite's owner and barber do: the
// runner knows no roles, only "send Bearer ${token}".
func TestRun_BearerSendsTheCapturedToken(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("Authorization"))
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{"token": "jwt-9"}})
	}))
	defer srv.Close()
	r, _ := fastRunner(srv.URL)
	results := r.Run([]Case{
		{Name: "login", Method: "POST", Path: "/v1/login", ExpectStatus: 200, Capture: map[string]string{"token": "data.token"}},
		{Name: "as bearer", Method: "GET", Path: "/v1/me", Auth: "bearer", ExpectStatus: 200},
		{Name: "as owner", Method: "GET", Path: "/v1/me", Auth: "owner", ExpectStatus: 200},
		{Name: "anonymous", Method: "GET", Path: "/v1/me", ExpectStatus: 200},
	})
	require.Len(t, results, 4)
	require.Equal(t, []string{"", "Bearer jwt-9", "Bearer jwt-9", ""}, got)
}

// A case that passed keeps what it sent and got, as a failure does: the page
// reads a passing case's exchange to see what it checked (the owner's choice
// of 30 Oct). The redactor masks and cuts both before anything is printed.
func TestRun_APassKeepsItsExchange(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":7}`))
	}))
	defer srv.Close()
	r, _ := fastRunner(srv.URL)
	results := r.Run([]Case{{Name: "create", Method: "POST", Path: "/v1/things", Request: map[string]interface{}{"name": "x"}, ExpectStatus: 201}})
	require.Len(t, results, 1)
	require.Equal(t, OutcomeOK, results[0].Outcome, results[0].Reason)
	require.Contains(t, results[0].Request, "POST /v1/things HTTP/1.1")
	require.Contains(t, results[0].Request, `{"name":"x"}`)
	require.Equal(t, `{"id":7}`, results[0].Response)
}
