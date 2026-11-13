package apitest

import (
	"crypto"
	"crypto/hmac"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Sign is how a case's request is signed. A signature covers the timestamp
// it is sent at, so no file can hold one: the runner computes it at send
// time from keys the suite names as ${variables} -- a target's secret
// variables, which the model writing the file never sees.
//
// One scheme family so far, SNAP (Bank Indonesia's open payment API
// standard), in its two forms, as a payment gateway verifies them
// (internal/services/oauth/signature.go):
//
//   - "token", the access-token request: X-SIGNATURE is base64 of an
//     RSA-SHA256 (PKCS #1 v1.5) signature over "client_key|timestamp" with
//     the merchant's private key; X-CLIENT-KEY is sent with it.
//   - "request", every call after it: X-SIGNATURE is base64 of an
//     HMAC-SHA512, keyed by Secret, over
//     "METHOD:path?query:token:hex(sha256(body)):timestamp", and the token is
//     sent as the bearer.
//
// Both send X-TIMESTAMP ("2006-01-02T15:04:05+07:00", always a numeric
// offset). Anything else a SNAP call carries -- X-PARTNER-ID, X-EXTERNAL-ID,
// CHANNEL-ID -- is the case's own headers.
type Sign struct {
	SNAP       string `yaml:"snap"`
	ClientKey  string `yaml:"client_key"`
	PrivateKey string `yaml:"private_key"`
	Secret     string `yaml:"secret"`
	Token      string `yaml:"token"`
	// EmptyBody is how a request with no body is hashed: "hash", the
	// SHA-256 of nothing, as the standard says (the default), or "blank",
	// nothing between the colons -- which is what a payment gateway's server computes
	// for a GET.
	EmptyBody string `yaml:"empty_body"`
	// Zone is the timestamp's zone, an IANA name; UTC by default.
	Zone string `yaml:"zone"`
}

// SignSchemes are the Sign.SNAP values.
var SignSchemes = []string{"token", "request"}

// timestampLayout always writes a numeric offset: a server parsing
// "+07:00" may not take "Z".
const timestampLayout = "2006-01-02T15:04:05-07:00"

// signRequest signs req, whose body is body, at now. Keys and the token are
// the case's ${variables}, substituted here.
func (r *Runner) signRequest(req *http.Request, s *Sign, body []byte, now time.Time) error {
	loc := time.UTC
	if s.Zone != "" {
		l, err := time.LoadLocation(s.Zone)
		if err != nil {
			return fmt.Errorf("sign: zone %q: %v", s.Zone, err)
		}
		loc = l
	}
	ts := now.In(loc).Format(timestampLayout)
	req.Header.Set("X-TIMESTAMP", ts)

	switch s.SNAP {
	case "token":
		key, err := parseRSAPrivateKey(r.substitute(s.PrivateKey))
		if err != nil {
			return fmt.Errorf("sign: private_key: %v", err)
		}
		clientKey := r.substitute(s.ClientKey)
		sum := sha256.Sum256([]byte(clientKey + "|" + ts))
		sig, err := rsa.SignPKCS1v15(nil, key, crypto.SHA256, sum[:])
		if err != nil {
			return fmt.Errorf("sign: %v", err)
		}
		req.Header.Set("X-CLIENT-KEY", clientKey)
		req.Header.Set("X-SIGNATURE", base64.StdEncoding.EncodeToString(sig))
	case "request":
		token := stripBearer(r.substitute(s.Token))
		bodyHash := ""
		if len(body) > 0 || s.EmptyBody != "blank" {
			sum := sha256.Sum256(body)
			bodyHash = hex.EncodeToString(sum[:])
		}
		toSign := strings.ToUpper(req.Method) + ":" + req.URL.RequestURI() + ":" + token + ":" + bodyHash + ":" + ts
		mac := hmac.New(sha512.New, []byte(r.substitute(s.Secret)))
		mac.Write([]byte(toSign))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-SIGNATURE", base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	default:
		return fmt.Errorf("sign: snap is %q; it is token or request", s.SNAP)
	}
	return nil
}

var pemArmor = regexp.MustCompile(`-----(BEGIN|END) [A-Z ]+-----`)

// parseRSAPrivateKey reads a PEM private key, PKCS #1 or PKCS #8. The
// armour and every line break are optional: a key pasted into a one-line
// field arrives with its newlines gone, and the base64 between the lines is
// all that matters.
func parseRSAPrivateKey(text string) (*rsa.PrivateKey, error) {
	b64 := strings.Join(strings.Fields(pemArmor.ReplaceAllString(text, "")), "")
	if b64 == "" {
		return nil, errors.New("empty -- set the variable it names")
	}
	der, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, errors.New("not a PEM key: its body is not base64")
	}
	// A whole PEM file base64-encoded, the usual way a key is put in one
	// line of an env file or a secret store, decodes to the PEM text, not to
	// the key: read that text as the key. One level, so a value cannot nest.
	if inner := string(der); pemArmor.MatchString(inner) && !pemArmor.MatchString(text) {
		return parseRSAPrivateKey(inner)
	}
	if k, err := x509.ParsePKCS1PrivateKey(der); err == nil {
		return k, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, errors.New("not an RSA private key (PKCS #1 or PKCS #8)")
	}
	k, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("a private key, but not an RSA one")
	}
	return k, nil
}
