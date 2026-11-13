// Command taskboard-runner runs taskboard Test Maker suites beside your own
// services: `run` takes runs from taskboard as a fleet, and `validate` and
// `test` check and run a suite from disk.
package main

import (
	"fmt"
	"io"
	"os"
	"time"
)

// version is stamped at release with -ldflags "-X main.version=<tag>".
var version = "dev"

// now is the clock a run's seeds start from; tests fix it.
var now = time.Now

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

const usage = `usage:
  taskboard-runner version
  taskboard-runner run [--environments FILE]    (TB_URL, TB_FLEET_TOKEN, TB_SLOTS)
  taskboard-runner validate --suite DIR [--seeds FILE] [--vars FILE] [--env NAME] [--environments FILE]
  taskboard-runner test --suite DIR --env NAME --base-url URL [--seeds FILE] [--vars FILE] [--fixtures DIR] [--environments FILE]`

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "version" || args[0] == "--version") {
		fmt.Fprintf(stdout, "taskboard-runner %s\n", version)
		return 0
	}
	if len(args) > 0 {
		switch args[0] {
		case "validate":
			return validate(args[1:], stdout, stderr)
		case "test":
			return testSuite(args[1:], stdout, stderr)
		case "run":
			return runFleet(args[1:], stderr)
		}
	}
	fmt.Fprintln(stderr, usage)
	return 2
}
