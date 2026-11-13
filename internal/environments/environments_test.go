package environments

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func write(t *testing.T, dir, name, body string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(p, []byte(body), mode))
	require.NoError(t, os.Chmod(p, mode))
	return p
}

// The file holds secrets, or says where they are, so a copy another user on
// the host can read is refused before anything in it is used -- and the
// refusal says how to fix it.
func TestLoad_RefusesAFileOthersCanOpen(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "environments.yaml", "staging:\n  api_key: \"k-123456\"\n", 0o644)
	_, err := Load(p)
	require.ErrorContains(t, err, "chmod 600")

	require.NoError(t, os.Chmod(p, 0o600))
	_, err = Load(p)
	require.NoError(t, err)
}

// Each source gives its value: inline, from the runner's environment, or from
// a file -- relative to the environments file, without the line break an
// editor or `echo` leaves at its end.
func TestSecrets_ReadsEverySource(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "key.txt", "file-value-123\n", 0o600)
	t.Setenv("TB_TEST_ADMIN_PASSWORD", "env-value-123")
	p := write(t, dir, "environments.yaml", ""+
		"staging:\n"+
		"  admin_password: { env: TB_TEST_ADMIN_PASSWORD }\n"+
		"  api_key: { file: key.txt }\n"+
		"  webhook_secret: \"inline-value-123\"\n", 0o600)
	f, err := Load(p)
	require.NoError(t, err)
	got, err := f.Secrets("staging")
	require.NoError(t, err)
	require.Equal(t, map[string]string{
		"admin_password": "env-value-123",
		"api_key":        "file-value-123",
		"webhook_secret": "inline-value-123",
	}, got)
}

// A run with a secret missing would send requests that fail for a reason
// nobody can see, so the read fails, naming the secret. It never names a
// value: this error is printed, and in a fleet it goes back to taskboard.
func TestSecrets_AMissingOrEmptySecretFailsByName(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct{ name, entry, want string }{
		{"empty inline", "pw: \"\"", "secret pw of environment staging is empty"},
		{"left blank", "pw:", "secret pw of environment staging is empty"},
		{"unset variable", "pw: { env: TB_TEST_NOT_SET_ANYWHERE }", "secret pw of environment staging: variable TB_TEST_NOT_SET_ANYWHERE is not set"},
		{"missing file", "pw: { file: nope.txt }", "secret pw of environment staging: cannot read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := write(t, dir, "environments.yaml", "staging:\n  other: \"other-value-123\"\n  "+tc.entry+"\n", 0o600)
			f, err := Load(p)
			require.NoError(t, err)
			_, err = f.Secrets("staging")
			require.ErrorContains(t, err, tc.want)
			require.NotContains(t, err.Error(), "other-value-123")
		})
	}
}

// Validating a suite needs the secrets' names and nothing else, so Names
// reads no value: it works on a machine where none of them is set.
func TestNames_ReadsNoValue(t *testing.T) {
	p := write(t, t.TempDir(), "environments.yaml", "staging:\n  b_key: { env: TB_TEST_NOT_SET_ANYWHERE }\n  a_key: { file: /nonexistent }\n", 0o600)
	f, err := Load(p)
	require.NoError(t, err)
	names, err := f.Names("staging")
	require.NoError(t, err)
	require.Equal(t, []string{"a_key", "b_key"}, names)

	_, err = f.Names("production")
	require.ErrorContains(t, err, `no environment "production"; it has: staging`)
}

// A secret written where the file expects an environment's map, or a number
// left unquoted, must not come back in the error that says so.
func TestLoad_AMisplacedValueIsNotQuotedInTheError(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"value instead of a map": "staging: hunter2-misplaced\n",
		"unquoted number":        "staging:\n  pin: 987654321\n",
		"list instead of a map":  "staging:\n  - hunter2-misplaced\n",
	} {
		t.Run(name, func(t *testing.T) {
			p := write(t, dir, "environments.yaml", body, 0o600)
			_, err := Load(p)
			require.Error(t, err)
			require.NotContains(t, err.Error(), "hunter2-misplaced")
			require.NotContains(t, err.Error(), "987654321")
			require.Contains(t, err.Error(), "line ")
		})
	}
}

// A secret becomes a ${name} in a suite, so its name must be one.
func TestLoad_RefusesANameASuiteCannotUse(t *testing.T) {
	p := write(t, t.TempDir(), "environments.yaml", "staging:\n  \"api key\": \"value-123\"\n", 0o600)
	_, err := Load(p)
	require.ErrorContains(t, err, `"api key" is not a variable name`)
}
