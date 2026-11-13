package apitest

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

// What the runner prints becomes a span, the run's output and a page every
// member of the workspace reads, so a secret variable's value, a captured
// token and a credential header are masked wherever they appear.
func TestRedactor_MasksSecretsTokensAndHeaders(t *testing.T) {
	red := NewRedactor([]string{"supportpass123", "abc"})
	red.Learn(map[string]string{"support_token": "bearer eyJhbGciOi.payload.sig", "token": "jwt-owner-1", "shop_id": "shop_42"})

	res := red.Result(Result{
		Reason:   `expect_body: data.echo: expected "x", got "supportpass123"`,
		Request:  "POST /v1/admin/auth/login HTTP/1.1\r\nAuthorization: Bearer eyJhbGciOi.payload.sig\r\nCookie: sid=zzz\r\n\r\n{\"password\":\"supportpass123\"}",
		Response: `{"token":"jwt-owner-1","shop_id":"shop_42","note":"abc"}`,
	})
	require.NotContains(t, res.Reason, "supportpass123")
	require.NotContains(t, res.Request, "supportpass123")
	require.NotContains(t, res.Request, "eyJhbGciOi.payload.sig")
	require.NotContains(t, res.Request, "sid=zzz")
	require.Contains(t, res.Request, "Authorization: [redacted]")
	require.NotContains(t, res.Response, "jwt-owner-1", "a captured token is masked in every later case")
	require.Contains(t, res.Response, "shop_42", "an id is not a credential")
	require.Contains(t, res.Response, `"note":"abc"`, "a three-character value is too short to mask without garbling every body")
}

// A credential the suite never captured is still a credential: a passing
// login's refresh_token was printed in the clear once passes kept their
// bodies (found on a dashboard login suite, 31 Oct). A field whose name
// says credential has its value masked whatever it holds, in the request and
// the response, and the body keeps its shape.
func TestRedactor_MasksCredentialFieldsItWasNeverTold(t *testing.T) {
	res := NewRedactor(nil).Result(Result{
		Request: "POST /login HTTP/1.1\r\nContent-Type: application/json\r\n\r\n{\"email\":\"a@x.co\",\"password\":\"inline-pass\"}",
		Response: `{"data":{"token":"jwt-1","refresh_token":"rt-long-lived","token_type":"Bearer","expires_in":900}}` + "\n" +
			`{"accessToken": "snap-abc", "tokenType": "Bearer", "client-secret":"cs-1", "id_token":"", "note":"say \"password\": \"x\""}`,
	})
	require.NotContains(t, res.Request, "inline-pass")
	require.Contains(t, res.Request, `"email":"a@x.co"`)
	for _, v := range []string{"jwt-1", "rt-long-lived", "snap-abc", "cs-1"} {
		require.NotContains(t, res.Response, v)
	}
	require.Contains(t, res.Response, `"refresh_token":"[redacted]"`, "masked where it stands")
	require.Contains(t, res.Response, `"accessToken": "[redacted]"`, "camelCase, and the spacing kept")
	require.Contains(t, res.Response, `"token_type":"Bearer"`, "a token's type is not a credential")
	require.Contains(t, res.Response, `"tokenType": "Bearer"`)
	require.Contains(t, res.Response, `"id_token":""`, "an empty value says no credential came back")
	require.Contains(t, res.Response, `"expires_in":900`)
	require.Contains(t, res.Response, `"note":"say \"password\": \"x\""`, "text inside a value is not a field")
}

// Masking happens before cutting: cut first, and a secret straddling the cut
// would leave its first half on the page.
func TestRedactor_MasksBeforeItCuts(t *testing.T) {
	secret := "s3cr3t-value-that-is-long"
	body := strings.Repeat("a", maxBody-10) + secret + strings.Repeat("b", 100)
	res := NewRedactor([]string{secret}).Result(Result{Response: body})
	require.NotContains(t, res.Response, "s3cr3t")
	require.True(t, strings.HasSuffix(res.Response, "… (cut)"))
}

// A cut lands on a character boundary. A cut through a multi-byte character
// is invalid UTF-8, which Postgres refuses to store.
func TestCut_KeepsValidUTF8(t *testing.T) {
	body := strings.Repeat("x", maxBody-1) + "…more"
	got := cut(body)
	require.True(t, utf8.ValidString(got))
	require.Equal(t, strings.Repeat("x", maxBody-1)+"… (cut)", got)
	require.Equal(t, "short", cut("short"))
}

// The verdict counts skips: "passed" only when nothing was skipped, and the
// cases a failure stopped are counted as not run rather than vanishing.
func TestSummarize(t *testing.T) {
	ok := Result{Outcome: OutcomeOK, DurationMS: 5}
	skip := Result{Outcome: OutcomeSkip}
	failed := Result{Outcome: OutcomeFail, DurationMS: 7}

	s := Summarize(2, []Result{ok, ok}, Build{State: BuildNotChecked})
	require.Equal(t, VerdictPassed, s.Verdict)
	require.Equal(t, int64(10), s.DurationMS)

	s = Summarize(3, []Result{ok, skip, skip}, Build{})
	require.Equal(t, VerdictPassedWithSkips, s.Verdict)
	require.Equal(t, 2, s.Skipped)

	s = Summarize(10, []Result{ok, failed}, Build{})
	require.Equal(t, VerdictFailed, s.Verdict)
	require.Equal(t, 8, s.NotRun)

	r := Refused(10, Build{State: BuildRefused}, "build mismatch")
	require.Equal(t, VerdictRefused, r.Verdict)
	require.Equal(t, 10, r.NotRun)
}

// The build check: a match passes, a mismatch refuses and says what to do, and
// a server with no stamp is "not checked" -- never the words a pass uses.
func TestCheckBuild(t *testing.T) {
	build := "unset"
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		if build != "unset" {
			_, _ = w.Write([]byte(`{"status":"ok","build":"` + build + `"}`))
		}
	}))
	defer srv.Close()
	c := srv.Client()

	build = "1234"
	require.Equal(t, BuildPassed, CheckBuild(c, srv.URL, "1234").State)

	got := CheckBuild(c, srv.URL, "9999")
	require.Equal(t, BuildRefused, got.State)
	require.Contains(t, got.Note, "built from 1234, and this run expects 9999")

	build = "dev"
	require.Equal(t, BuildNotChecked, CheckBuild(c, srv.URL, "9999").State)
	build = ""
	require.Equal(t, BuildNotChecked, CheckBuild(c, srv.URL, "9999").State)

	status = http.StatusNotFound
	got = CheckBuild(c, srv.URL, "9999")
	require.Equal(t, BuildNotChecked, got.State, "a gateway that routes only /v1 hides /health; that is unknown, not a mismatch")
	require.Contains(t, got.Note, "answered 404")

	require.Equal(t, BuildNotChecked, CheckBuild(c, srv.URL, "").State)
}
