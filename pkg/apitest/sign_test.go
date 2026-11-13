package apitest

import (
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// snapServer verifies as a payment gateway does
// (internal/services/oauth/signature.go): the access token by RSA over
// "X-CLIENT-KEY|X-TIMESTAMP", every other call by HMAC-SHA512 over
// "METHOD:RequestURI:token:hex(sha256(body)):X-TIMESTAMP" -- with nothing in
// the body's place on a GET, as its symmetricSignature has it. A signature
// the runner got wrong is a 401 here, so a pass is the server's own verdict.
func snapServer(t *testing.T, pub *rsa.PublicKey, secret string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ts, sig := r.Header.Get("X-TIMESTAMP"), r.Header.Get("X-SIGNATURE")
		if r.URL.Path == "/auth/v1.0/access-token/b2b" {
			raw, _ := base64.StdEncoding.DecodeString(sig)
			sum := sha256.Sum256([]byte(r.Header.Get("X-CLIENT-KEY") + "|" + ts))
			if rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], raw) != nil {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"accessToken": "tok-123"})
			return
		}
		body, _ := io.ReadAll(r.Body)
		hash := ""
		if r.Method != http.MethodGet {
			sum := sha256.Sum256(body)
			hash = hex.EncodeToString(sum[:])
		}
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mac := hmac.New(sha512.New, []byte(secret))
		mac.Write([]byte(strings.ToUpper(r.Method) + ":" + r.RequestURI + ":" + token + ":" + hash + ":" + ts))
		if base64.StdEncoding.EncodeToString(mac.Sum(nil)) != sig {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"responseCode":"2002700"}`))
	}))
}

func pemKey(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	return key, string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

// A SNAP API is signed per request, over the moment it is sent, so no file
// can hold a signature: the runner signs the token request with the
// merchant's private key and every call after it with the HMAC key and the
// token it captured. A payment gateway's endpoints could only be tested for refusing
// unsigned calls before this.
func TestSign_SNAPTokenThenSignedCalls(t *testing.T) {
	key, pemText := pemKey(t)
	srv := snapServer(t, &key.PublicKey, "client-key-1")
	defer srv.Close()
	r, _ := fastRunner(srv.URL)
	r.Now = func() time.Time { return time.Date(2026, 9, 30, 1, 0, 0, 0, time.UTC) }
	r.Seed("snap_client_key", "client-key-1")
	r.Seed("snap_private_key", pemText)

	token := Case{Name: "token", Method: "POST", Path: "/auth/v1.0/access-token/b2b", ExpectStatus: 200,
		Request: map[string]interface{}{"grantType": "client_credentials"},
		Sign:    &Sign{SNAP: "token", ClientKey: "${snap_client_key}", PrivateKey: "${snap_private_key}", Zone: "Asia/Jakarta"},
		Capture: map[string]string{"snap_token": "accessToken"}}
	create := Case{Name: "create va", Method: "POST", Path: "/va/v1.0/transfer-va/create-va?x=1", ExpectStatus: 200,
		Request: map[string]interface{}{"partnerServiceId": "  123", "totalAmount": map[string]interface{}{"value": "10000.00", "currency": "IDR"}},
		Headers: map[string]string{"X-PARTNER-ID": "merchant-1"},
		Sign:    &Sign{SNAP: "request", Secret: "${snap_client_key}", Token: "${snap_token}"}}
	get := Case{Name: "get merchant", Method: "GET", Path: "/api/v1.0/merchant/m-1", ExpectStatus: 200,
		Sign: &Sign{SNAP: "request", Secret: "${snap_client_key}", Token: "${snap_token}", EmptyBody: "blank"}}

	results := r.Run([]Case{token, create, get})
	for _, res := range results {
		require.Equal(t, OutcomeOK, res.Outcome, "%s: %s", res.Name, res.Reason)
	}

	// The standard hashes an empty body; this server leaves it out, so
	// without empty_body: blank the GET is refused -- the option is load
	// bearing, not decoration.
	get.Sign.EmptyBody = ""
	results = r.Run([]Case{token, get})
	require.Equal(t, OutcomeFail, results[1].Outcome)
	require.Contains(t, results[1].Reason, "got 401")
}

// X-TIMESTAMP is the run's clock in the case's zone, always with a numeric
// offset -- UTC is "+00:00", never "Z", which a server parsing an offset
// refuses. A key pasted into a one-line field, its newlines gone, still reads.
func TestSign_TimestampAndAOneLineKey(t *testing.T) {
	_, pemText := pemKey(t)
	var stamps []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stamps = append(stamps, r.Header.Get("X-TIMESTAMP"))
	}))
	defer srv.Close()
	r, _ := fastRunner(srv.URL)
	r.Now = func() time.Time { return time.Date(2026, 9, 30, 1, 0, 0, 0, time.UTC) }
	r.Seed("pk", strings.ReplaceAll(pemText, "\n", ""))
	r.Seed("ck", "c")
	results := r.Run([]Case{
		{Name: "utc", Method: "POST", Path: "/t", ExpectStatus: 200, Sign: &Sign{SNAP: "token", ClientKey: "${ck}", PrivateKey: "${pk}"}},
		{Name: "jakarta", Method: "POST", Path: "/t", ExpectStatus: 200, Sign: &Sign{SNAP: "token", ClientKey: "${ck}", PrivateKey: "${pk}", Zone: "Asia/Jakarta"}},
	})
	require.Equal(t, OutcomeOK, results[0].Outcome, results[0].Reason)
	require.Equal(t, []string{"2026-09-30T01:00:00+00:00", "2026-09-30T08:00:00+07:00"}, stamps)
}

// A signing block the runner could not sign is refused before anything is
// sent, where the model can fix it; its keys are variables like any other,
// defined before the case.
func TestValidate_Sign(t *testing.T) {
	for _, tc := range []struct{ yaml, want string }{
		{"  sign: {snap: token, client_key: \"${ck}\"}\n", "needs client_key and private_key"},
		{"  sign: {snap: request, secret: \"${ck}\"}\n", "needs secret (the HMAC key) and token"},
		{"  sign: {snap: hmac}\n", "it is token (the access-token request, RSA) or request"},
		{"  sign: {snap: request, secret: \"${ck}\", token: \"${ck}\"}\n  auth: bearer\n", "leave auth out"},
		{"  sign: {snap: token, client_key: \"${ck}\", private_key: \"${ck}\", zone: Jakarta}\n", "is not a time zone"},
		{"  sign: {snap: token, client_key: \"${ck}\", private_key: \"${pk}\"}\n", "sign private_key uses ${pk}"},
		{"  sign: {snap: token, client_key: \"${ck}\", private_key: \"${ck}\"}\n  form: {file: x}\n", "a multipart form cannot be signed"},
	} {
		f := mustParse(t, "01_x.yml", "- name: x\n  method: POST\n  path: /x\n  expect_status: 200\n"+tc.yaml)
		ps := messages(Validate([]File{f}, Known{Target: []string{"ck"}}, Options{}))
		require.NotEmpty(t, ps, tc.yaml)
		require.Contains(t, strings.Join(ps, "\n"), tc.want, tc.yaml)
	}
	ok := mustParse(t, "01_x.yml", "- name: x\n  method: POST\n  path: /x\n  expect_status: 200\n  sign: {snap: token, client_key: \"${ck}\", private_key: \"${ck}\"}\n")
	require.Empty(t, messages(Validate([]File{ok}, Known{Target: []string{"ck"}}, Options{})))
}

// The signature goes into the failure dump like any header; it is masked
// with the other credential headers.
func TestRedact_MasksTheSignature(t *testing.T) {
	red := NewRedactor(nil)
	res := red.Result(Result{Outcome: OutcomeFail, Request: "POST /x HTTP/1.1\r\nX-Signature: abc+def==\r\nX-Partner-Id: m-1\r\n"})
	require.NotContains(t, res.Request, "abc+def==")
	require.Contains(t, res.Request, "X-Partner-Id: m-1")
}

// A misspelt signing key is refused at its line, not ignored: ignored, the
// signature is made without it and the API's 401 reads like a wrong key.
func TestParse_ASignFieldIsChecked(t *testing.T) {
	_, err := Parse("01_x.yml", []byte("- name: x\n  method: POST\n  path: /x\n  sign:\n    snap: token\n    privatekey: \"${pk}\"\n"))
	require.ErrorContains(t, err, `01_x.yml:6: unknown sign field "privatekey"`)
}

// A signing key written on the case, not inside sign, is told where it goes.
// "unknown field" alone read to a model as "the runner has no such option",
// and it dropped empty_body -- signing a payment gateway's GETs in a way it refuses.
func TestParse_ASignKeyOnTheCaseSaysWhereItGoes(t *testing.T) {
	_, err := Parse("05_x.yml", []byte("- name: x\n  method: GET\n  path: /x\n  sign: {snap: request, secret: \"${k}\", token: \"${t}\"}\n  empty_body: blank\n"))
	require.ErrorContains(t, err, `05_x.yml:5: "empty_body" goes inside sign: (sign: {snap: ..., empty_body: ...}), not on the case`)
}

// A private key reaches a target in three shapes, and each must sign the
// same: the PEM as is, the PEM with its line breaks gone (a one-line field),
// and the whole PEM file base64-encoded -- the usual way a key is kept in one
// line of a secret store, which decodes to the PEM text, not the key. A
// wrong key is still refused: decoding twice must not turn noise into a key.
func TestParseRSAPrivateKey_TheShapesAKeyArrivesIn(t *testing.T) {
	key, pemText := pemKey(t)
	for name, text := range map[string]string{
		"pem":                  pemText,
		"one line":             strings.ReplaceAll(pemText, "\n", ""),
		"base64 of the pem":    base64.StdEncoding.EncodeToString([]byte(pemText)),
		"base64, line-wrapped": wrap(base64.StdEncoding.EncodeToString([]byte(pemText)), 64),
	} {
		got, err := parseRSAPrivateKey(text)
		require.NoError(t, err, name)
		require.True(t, key.Equal(got), name)
	}

	_, err := parseRSAPrivateKey(base64.StdEncoding.EncodeToString([]byte("-----BEGIN PRIVATE KEY-----\nbm90IGEga2V5\n-----END PRIVATE KEY-----\n")))
	require.ErrorContains(t, err, "not an RSA private key")
	nested := base64.StdEncoding.EncodeToString([]byte(base64.StdEncoding.EncodeToString([]byte(pemText))))
	_, err = parseRSAPrivateKey(nested)
	require.Error(t, err, "one level of base64 around the PEM, never more")
}

func wrap(s string, n int) string {
	var b strings.Builder
	for len(s) > n {
		b.WriteString(s[:n] + "\n")
		s = s[n:]
	}
	b.WriteString(s)
	return b.String()
}
