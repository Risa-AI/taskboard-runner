package apitest

import (
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// Redactor masks what must not leave the runner: every secret variable's
// value, every captured token, the credential headers of a request dump, and
// the value of every credential-named field in a body.
// It is applied before anything is printed, because what the runner prints
// becomes a span, the run's output and a page anyone in the workspace reads.
type Redactor struct {
	secrets []string
}

// minSecretLen is the shortest value masked. Masking a one-character "secret"
// everywhere it appears would garble every body without protecting anything;
// a credential is longer than three characters.
const minSecretLen = 4

// maxBody is how much of a failure's request or response is kept.
const maxBody = 2048

const mask = "[redacted]"

// NewRedactor masks these values wherever they appear.
func NewRedactor(secrets []string) *Redactor {
	r := &Redactor{}
	for _, s := range secrets {
		r.add(s)
	}
	return r
}

func (r *Redactor) add(s string) {
	if len(s) < minSecretLen {
		return
	}
	for _, have := range r.secrets {
		if have == s {
			return
		}
	}
	r.secrets = append(r.secrets, s)
	// Longest first, so a secret containing another is masked whole.
	sort.SliceStable(r.secrets, func(i, j int) bool { return len(r.secrets[i]) > len(r.secrets[j]) })
}

// isTokenName is a captured variable holding a credential: "token" or a name
// ending "_token" (the original suite's token, support_token, dev_admin_token).
func isTokenName(name string) bool {
	return name == "token" || strings.HasSuffix(name, "_token")
}

// Learn adds the tokens captured so far. Called before each result is
// printed, so a token captured by case 2 is masked in case 40's failure.
func (r *Redactor) Learn(vars map[string]string) {
	for name, v := range vars {
		if isTokenName(name) {
			r.add(v)
			r.add(stripBearer(v))
		}
	}
}

// Mask adds values to mask whatever their name: a role's token lives in the
// role's scope, under the name the target gave it, so Learn never sees it.
func (r *Redactor) Mask(values ...string) {
	for _, v := range values {
		r.add(v)
		r.add(stripBearer(v))
	}
}

var credentialHeader = regexp.MustCompile(`(?im)^((?:proxy-)?authorization|cookie|x-api-key|x-signature):[^\r\n]*`)

// jsonPair is a JSON string field with a string value, read as text: a body is
// masked where it stands, so its spacing -- and a body holding two JSON values
// -- reads as the server wrote it. Text inside a value is never a field.
var jsonPair = regexp.MustCompile(`"((?:[^"\\]|\\.)*)"(\s*:\s*)"((?:[^"\\]|\\.)*)"`)

// credentialWords are what a field's name says when its value is a
// credential, read lowercased without "_" and "-": refresh_token,
// refreshToken and Refresh-Token are one name.
var credentialWords = []string{"token", "secret", "password", "apikey", "privatekey", "authorization"}

// isCredentialField is a field whose value is a credential whatever it holds.
// A *type field names a kind of credential ("token_type": "Bearer").
func isCredentialField(name string) bool {
	n := strings.NewReplacer("_", "", "-", "").Replace(strings.ToLower(name))
	if strings.HasSuffix(n, "type") {
		return false
	}
	for _, w := range credentialWords {
		if strings.Contains(n, w) {
			return true
		}
	}
	return false
}

// maskFields masks the value of every credential-named field in a body. The
// values the redactor was given or saw captured are not enough: a login's
// refresh_token, which no case captured, was in every passing login's
// response once passes kept their bodies (31 Oct). An empty value
// stays empty: it says no credential came back.
func maskFields(s string) string {
	return jsonPair.ReplaceAllStringFunc(s, func(pair string) string {
		m := jsonPair.FindStringSubmatch(pair)
		if m[3] == "" || !isCredentialField(m[1]) {
			return pair
		}
		return `"` + m[1] + `"` + m[2] + `"` + mask + `"`
	})
}

// String masks secrets in s.
func (r *Redactor) String(s string) string {
	for _, secret := range r.secrets {
		s = strings.ReplaceAll(s, secret, mask)
	}
	return s
}

// Result masks every text field of a result, then cuts its bodies. Masking
// comes first: cutting first could leave half a secret on the cut's edge.
func (r *Redactor) Result(res Result) Result {
	res.Reason = r.String(res.Reason)
	res.Path = r.String(res.Path)
	res.Request = cut(maskFields(r.String(credentialHeader.ReplaceAllString(res.Request, "$1: "+mask))))
	res.Response = cut(maskFields(r.String(res.Response)))
	return res
}

// cut keeps the first maxBody bytes, backing off to a character boundary:
// a cut through a multi-byte character is a string Postgres refuses to store,
// which once took a whole conversation turn down.
func cut(s string) string {
	if len(s) <= maxBody {
		return s
	}
	n := maxBody
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "… (cut)"
}
