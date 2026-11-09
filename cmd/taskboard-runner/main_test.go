package main

import (
	"bytes"
	"testing"
)

// The fleet's page compares the reported version with the latest release, so
// both spellings must print exactly "taskboard-runner <version>" and a newline.
func TestVersionCommands(t *testing.T) {
	saved := version
	version = "v1.2.3"
	t.Cleanup(func() { version = saved })

	for _, arg := range []string{"version", "--version"} {
		t.Run(arg, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run([]string{arg}, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("run(%q) = %d, want 0", arg, code)
			}
			if got, want := stdout.String(), "taskboard-runner v1.2.3\n"; got != want {
				t.Errorf("stdout = %q, want %q", got, want)
			}
		})
	}
}

// Unknown commands must fail with exit code 2 and a usage message on stderr only,
// so a script never mistakes a typo for a successful run or parses it from stdout.
func TestUnknownCommandFails(t *testing.T) {
	for _, args := range [][]string{{"nope"}, {}, {"version", "extra"}} {
		var stdout, stderr bytes.Buffer
		code := run(args, &stdout, &stderr)
		if code != 2 {
			t.Errorf("run(%q) = %d, want 2", args, code)
		}
		if stdout.Len() != 0 {
			t.Errorf("run(%q) wrote to stdout: %q", args, stdout.String())
		}
		if stderr.Len() == 0 {
			t.Errorf("run(%q) wrote nothing to stderr", args)
		}
	}
}
