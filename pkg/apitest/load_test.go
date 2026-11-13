package apitest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// A file in the original suite's shape parses field for field. The format's
// promise -- the original 667 cases run unchanged -- so a field that decodes
// into the wrong place breaks every suite that uses it.
func TestParse_ReadsTheOriginalFormat(t *testing.T) {
	src := `# ── Auth ──────────────────────
- name: Register owner
  method: POST
  path: /v1/auth/register
  request:
    email: "e2e-${rand}@example.com"
    phone: "${ownerPhone}"
  expect_status: 201
  capture:
    token: data.token
  expect_body:
    success: true
    data.token:
      $type: string
      $exists: true

- name: Upload logo
  method: POST
  path: /v1/shops/${shop_id}/logo
  auth: owner
  form:
    logo: "@fixtures/logo.png"
  expect_status: 200
  requires_vars: [shop_id]
  local_only: true
  delay_ms: 50
  headers:
    X-Test: "yes"
`
	f, err := Parse("02_auth.yml", []byte(src))
	require.NoError(t, err)
	require.Len(t, f.Cases, 2)

	reg := f.Cases[0]
	require.Equal(t, "Register owner", reg.Name)
	require.Equal(t, 201, reg.ExpectStatus)
	require.Equal(t, map[string]string{"token": "data.token"}, reg.Capture)
	require.Equal(t, "e2e-${rand}@example.com", reg.Request.(map[string]interface{})["email"])
	require.Equal(t, map[string]interface{}{"$type": "string", "$exists": true}, reg.ExpectBody["data.token"])
	require.Equal(t, "02_auth.yml", reg.File)
	require.Equal(t, 2, reg.Line, "the line a case starts on is what a problem points at")

	up := f.Cases[1]
	require.Equal(t, "owner", up.Auth)
	require.Equal(t, "@fixtures/logo.png", up.Form["logo"])
	require.Equal(t, []string{"shop_id"}, up.RequiresVars)
	require.True(t, up.LocalOnly)
	require.Equal(t, 50, up.DelayMS)
	require.Equal(t, "yes", up.Headers["X-Test"])
}

// A misspelt field is refused with its line. Decoded silently, a case with
// `expect_statsu: 403` asserts nothing and passes on any status -- a green
// run that checked nothing.
func TestParse_RefusesAnUnknownFieldAtItsLine(t *testing.T) {
	src := "- name: a\n  method: GET\n  path: /x\n  expect_statsu: 403\n"
	_, err := Parse("10_x.yml", []byte(src))
	require.Error(t, err)
	require.Contains(t, err.Error(), `10_x.yml:4: unknown field "expect_statsu"`)
	require.Contains(t, err.Error(), "expect_status", "the refusal lists what would have been accepted")
}

// The original suite keeps a section header above the first case, and a file may be
// all header while a section is being written. That is an empty file, not a
// broken one.
func TestParse_ACommentOnlyFileIsEmpty(t *testing.T) {
	f, err := Parse("99_later.yml", []byte("# ── To do ──\n"))
	require.NoError(t, err)
	require.Empty(t, f.Cases)
}

func TestParse_RefusesWhatIsNotAListOfCases(t *testing.T) {
	_, err := Parse("a.yml", []byte("name: not a list\n"))
	require.ErrorContains(t, err, "a.yml:1: a scenario file is a list of cases")

	_, err = Parse("a.yml", []byte("- just a string\n"))
	require.ErrorContains(t, err, "a.yml:1: a case is a map")

	_, err = Parse("a.yml", []byte("- name: [unclosed\n"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "a.yml:")
}

// A file name becomes a path on the executor and a key in the stored bundle,
// so one that could climb out of its directory never gets that far.
func TestValidName(t *testing.T) {
	for _, ok := range []string{"01_health.yml", "02a_auth_extras.yaml", "x.yml"} {
		require.True(t, ValidName(ok), ok)
	}
	for _, bad := range []string{"../01.yml", "a/b.yml", ".hidden.yml", "01.txt", "a..yml", ""} {
		require.False(t, ValidName(bad), bad)
	}
	_, err := Parse("../x.yml", []byte("[]"))
	require.Error(t, err)
}

// A fixture path is what a scenario writes after "@". It becomes an object key
// and a file on the executor, so a path that climbs, starts hidden or is
// absolute never gets that far.
func TestValidFixturePath(t *testing.T) {
	for _, ok := range []string{"fixtures/logo.png", "logo.png", "a/b/c/d.jpg"} {
		require.True(t, ValidFixturePath(ok), ok)
	}
	for _, bad := range []string{"", "../x.png", "/etc/passwd", "fixtures/../x", ".env", "a//b", "a/b/c/d/e.png", "fixtures/a..png"} {
		require.False(t, ValidFixturePath(bad), bad)
	}
}

// Run order is name order, the rule the original suite's scenario README states: a
// file uses variables an earlier one captured, so 02a_ must run between 02_
// and 03_ on every machine.
func TestOrder_IsNameOrder(t *testing.T) {
	files := []File{{Name: "03_shop.yml"}, {Name: "02a_extra.yml"}, {Name: "10_x.yml"}, {Name: "02_auth.yml"}, {Name: "01_health.yml"}}
	Order(files)
	var got []string
	for _, f := range files {
		got = append(got, f.Name)
	}
	require.Equal(t, []string{"01_health.yml", "02_auth.yml", "02a_extra.yml", "03_shop.yml", "10_x.yml"}, got)
}

func TestLoadDir_ReadsOnlyScenarioFilesInOrder(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600))
	}
	write("02_b.yml", "- name: b\n  method: GET\n  path: /b\n")
	write("01_a.yaml", "- name: a\n  method: GET\n  path: /a\n")
	write("README.md", "not a scenario")
	require.NoError(t, os.Mkdir(filepath.Join(dir, "sub"), 0o700))

	files, err := LoadDir(dir)
	require.NoError(t, err)
	require.Len(t, files, 2)
	cases := Cases(files)
	require.Equal(t, []string{"a", "b"}, []string{cases[0].Name, cases[1].Name})

	_, err = LoadDir(t.TempDir())
	require.ErrorContains(t, err, "no scenario files")
}
