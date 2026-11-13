package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/Risa-AI/taskboard-runner/internal/fleet"
)

// runFleet is the runner itself: it takes runs from taskboard until it is
// stopped (Ctrl-C or SIGTERM). Exit 2 is a configuration it cannot start
// with; exit 1 is taskboard refusing it, or an environments file it cannot
// read.
func runFleet(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var c common
	fs.StringVar(&c.environments, "environments", "", "the environments file (default $TB_ENVIRONMENTS, else "+defaultEnvironments+")")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	cfg, err := fleetConfig(c.environmentsPath())
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	cfg.Version = version
	cfg.Log = stderr

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := fleet.Run(ctx, cfg); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

// fleetConfig reads TB_URL, TB_FLEET_TOKEN and TB_SLOTS (default 1).
func fleetConfig(environments string) (fleet.Config, error) {
	cfg := fleet.Config{URL: os.Getenv("TB_URL"), Token: os.Getenv("TB_FLEET_TOKEN"), Slots: 1, Environments: environments}
	if s := os.Getenv("TB_SLOTS"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			return cfg, errors.New("TB_SLOTS must be a whole number, 1 or more")
		}
		cfg.Slots = n
	}
	return cfg, cfg.Check()
}
