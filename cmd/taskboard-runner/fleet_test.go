package main

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

// A runner that cannot start says why and exits 2 before it calls anyone,
// and never prints the token it was given.
func TestRunFleet_RefusesAConfigurationItCannotStartWith(t *testing.T) {
	for want, env := range map[string]map[string]string{
		"TB_URL is not set":    {"TB_URL": "", "TB_FLEET_TOKEN": "secret-token-123", "TB_SLOTS": ""},
		"TB_URL must be https": {"TB_URL": "http://taskboard.example.com", "TB_FLEET_TOKEN": "secret-token-123", "TB_SLOTS": ""},
		"TB_FLEET_TOKEN":       {"TB_URL": "https://taskboard.example.com", "TB_FLEET_TOKEN": "", "TB_SLOTS": ""},
		"TB_SLOTS must be":     {"TB_URL": "https://taskboard.example.com", "TB_FLEET_TOKEN": "secret-token-123", "TB_SLOTS": "two"},
	} {
		for k, v := range env {
			t.Setenv(k, v)
		}
		var stdout, stderr bytes.Buffer
		require.Equal(t, 2, run([]string{"run"}, &stdout, &stderr), want)
		require.Contains(t, stderr.String(), want)
		require.NotContains(t, stderr.String(), "secret-token-123")
		require.Empty(t, stdout.String())
	}
}

// A runner whose environments file is missing or open to others exits 1:
// claiming runs it cannot give secrets to would only fail them.
func TestRunFleet_RefusesAnEnvironmentsFileItCannotUse(t *testing.T) {
	t.Setenv("TB_URL", "http://127.0.0.1:1")
	t.Setenv("TB_FLEET_TOKEN", "secret-token-123")
	t.Setenv("TB_SLOTS", "")
	var stderr bytes.Buffer
	require.Equal(t, 1, run([]string{"run", "--environments", t.TempDir() + "/missing.yaml"}, &bytes.Buffer{}, &stderr))
	require.Contains(t, stderr.String(), "missing.yaml")
	require.NotContains(t, stderr.String(), "secret-token-123")
}
