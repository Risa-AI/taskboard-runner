// Command taskboard-runner runs taskboard Test Maker suites beside your own
// services. This version only reports its version and the commands that run
// suites come with the engine and the fleet loop.
package main

import (
	"fmt"
	"io"
	"os"
)

// version is stamped at release with -ldflags "-X main.version=<tag>".
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "version" || args[0] == "--version") {
		fmt.Fprintf(stdout, "taskboard-runner %s\n", version)
		return 0
	}
	fmt.Fprintln(stderr, "usage: taskboard-runner version")
	return 2
}
