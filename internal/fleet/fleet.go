// Package fleet is the runner's loop: it tells taskboard which secrets this
// host holds, heartbeats, claims runs, executes them and reports each case,
// masked. It only ever calls out; see docs/protocol.md.
package fleet

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"sync"
	"time"

	"github.com/Risa-AI/taskboard-runner/internal/environments"
	"github.com/Risa-AI/taskboard-runner/pkg/protocol"
)

// Config is what the runner is started with: TB_URL, TB_FLEET_TOKEN,
// TB_SLOTS and the environments file.
type Config struct {
	URL          string
	Token        string
	Slots        int
	Environments string
	// Version is the runner's release, sent on every heartbeat.
	Version string
	// Log receives one line per event. It never receives a secret or the
	// token.
	Log io.Writer

	// HeartbeatEvery, RetryBase and RetryMax default to 30 s, 1 s and 30 s;
	// tests shorten them.
	HeartbeatEvery time.Duration
	RetryBase      time.Duration
	RetryMax       time.Duration
	// DefaultTimeout bounds a run whose job names no timeout (20 minutes).
	DefaultTimeout time.Duration
}

// Check refuses a configuration the runner cannot start with. The token
// travels on every call, so taskboard must be reached over HTTPS; plain
// HTTP is allowed only to this host itself.
func (c Config) Check() error {
	u, err := url.Parse(c.URL)
	switch {
	case c.URL == "":
		return errors.New("TB_URL is not set: give the taskboard address, e.g. https://taskboard.example.com")
	case err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http"):
		return errors.New("TB_URL is not an http(s) address")
	case u.Scheme == "http" && !loopback(u.Hostname()):
		return errors.New("TB_URL must be https: the fleet token is sent on every call")
	case c.Token == "":
		return errors.New("TB_FLEET_TOKEN is not set: use the token taskboard showed when the fleet was registered")
	case c.Slots < 1:
		return errors.New("TB_SLOTS must be 1 or more")
	}
	return nil
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (c *Config) defaults() {
	if c.HeartbeatEvery == 0 {
		c.HeartbeatEvery = 30 * time.Second
	}
	if c.RetryBase == 0 {
		c.RetryBase = time.Second
	}
	if c.RetryMax == 0 {
		c.RetryMax = 30 * time.Second
	}
	if c.DefaultTimeout == 0 {
		c.DefaultTimeout = 20 * time.Minute
	}
	if c.Log == nil {
		c.Log = io.Discard
	}
}

// fleet is one running runner.
type fleet struct {
	cfg    Config
	api    *client
	logMu  sync.Mutex
	runsMu sync.Mutex
	runs   map[string]context.CancelCauseFunc
	fatal  chan error
}

// Run starts the runner and returns when ctx ends (nil), or when taskboard
// refuses the runner itself: its token (401) or its protocol (426).
func Run(ctx context.Context, cfg Config) error {
	if err := cfg.Check(); err != nil {
		return err
	}
	cfg.defaults()
	f := &fleet{
		cfg: cfg,
		api: &client{
			base:      trimSlash(cfg.URL),
			token:     cfg.Token,
			http:      &http.Client{Timeout: 60 * time.Second},
			retryBase: cfg.RetryBase,
			retryMax:  cfg.RetryMax,
			sleep:     sleepCtx,
		},
		runs:  map[string]context.CancelCauseFunc{},
		fatal: make(chan error, 1),
	}

	// The file is read before anything is claimed: a runner that cannot read
	// its secrets would claim runs only to fail them.
	report, err := readReport(cfg.Environments)
	if errors.Is(err, fs.ErrPermission) {
		return fmt.Errorf("%w: this user cannot read the environments file; in Docker, run the container as the file's owner: --user \"$(id -u):$(id -g)\"", err)
	}
	if err != nil {
		return err
	}
	if _, err := f.api.call(ctx, http.MethodPut, "/environments", report, nil); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("reporting the environments: %w", err)
	}
	f.logf("taskboard-runner %s: %d environment(s) reported, %d slot(s)", cfg.Version, len(report), cfg.Slots)

	ctx, stop := context.WithCancel(ctx)
	defer stop()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); f.heartbeat(ctx, report) }()
	for i := 0; i < cfg.Slots; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); f.work(ctx) }()
	}

	var out error
	select {
	case <-ctx.Done():
	case out = <-f.fatal:
	}
	stop()
	wg.Wait()
	return out
}

func trimSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

// readReport loads the environments file and lists each environment's
// secret names. It reads no value.
func readReport(path string) (protocol.EnvironmentsReport, error) {
	file, err := environments.Load(path)
	if err != nil {
		return nil, err
	}
	report := protocol.EnvironmentsReport{}
	for _, env := range file.Environments() {
		names, err := file.Names(env)
		if err != nil {
			return nil, err
		}
		report = append(report, protocol.Environment{Name: env, Secrets: names})
	}
	return report, nil
}

// halt stops the runner with err, once.
func (f *fleet) halt(err error) {
	select {
	case f.fatal <- err:
	default:
	}
}

func (f *fleet) logf(format string, args ...interface{}) {
	f.logMu.Lock()
	defer f.logMu.Unlock()
	fmt.Fprintf(f.cfg.Log, format+"\n", args...)
}

// heartbeat tells taskboard this fleet is alive and how many slots are free,
// stops the runs it names, and reports the environments again whenever the
// file's names change.
func (f *fleet) heartbeat(ctx context.Context, sent protocol.EnvironmentsReport) {
	t := time.NewTicker(f.cfg.HeartbeatEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if report, err := readReport(f.cfg.Environments); err != nil {
			f.logf("the environments file cannot be read (%v); taskboard keeps the last report", err)
		} else if !reflect.DeepEqual(report, sent) {
			if _, err := f.api.call(ctx, http.MethodPut, "/environments", report, nil); err == nil {
				sent = report
				f.logf("the environments file changed: %d environment(s) reported", len(report))
			} else if f.stopOn(err) {
				return
			}
		}
		var answer protocol.HeartbeatAnswer
		_, err := f.api.call(ctx, http.MethodPost, "/heartbeat", protocol.Heartbeat{Version: f.cfg.Version, SlotsFree: f.slotsFree()}, &answer)
		if err != nil {
			if f.stopOn(err) {
				return
			}
			continue
		}
		for _, id := range answer.RunsToCancel {
			f.cancelRun(id, errCancelled)
		}
	}
}

// stopOn halts the runner on a fatal refusal and logs any other error.
func (f *fleet) stopOn(err error) bool {
	if fatal(err) {
		f.halt(err)
		return true
	}
	if !errors.Is(err, context.Canceled) {
		f.logf("%v", err)
	}
	return false
}

// work claims runs one after another, for one slot.
func (f *fleet) work(ctx context.Context) {
	for ctx.Err() == nil {
		var job protocol.Job
		asked := time.Now()
		status, err := f.api.call(ctx, http.MethodPost, "/claim", struct{}{}, &job)
		if err != nil {
			if f.stopOn(err) {
				return
			}
			// A refusal of the claim itself (a bug on one side) is not
			// retried at full speed.
			if sleepCtx(ctx, f.cfg.RetryMax) != nil {
				return
			}
			continue
		}
		if status != http.StatusOK || job.RunID == "" {
			// taskboard holds an empty claim for 25 s; one answered at once
			// is not asked again at once, or the loop would hammer it.
			if time.Since(asked) < time.Second && sleepCtx(ctx, f.cfg.RetryBase) != nil {
				return
			}
			continue
		}
		f.execute(ctx, job)
	}
}

var (
	errCancelled = errors.New("cancelled in taskboard")
	errEnded     = errors.New("the run already ended in taskboard")
	errTimeLimit = errors.New("time limit")
)

func (f *fleet) addRun(id string, cancel context.CancelCauseFunc) {
	f.runsMu.Lock()
	defer f.runsMu.Unlock()
	f.runs[id] = cancel
}

func (f *fleet) removeRun(id string) {
	f.runsMu.Lock()
	defer f.runsMu.Unlock()
	delete(f.runs, id)
}

func (f *fleet) cancelRun(id string, cause error) {
	f.runsMu.Lock()
	cancel := f.runs[id]
	f.runsMu.Unlock()
	if cancel != nil {
		cancel(cause)
	}
}

func (f *fleet) slotsFree() int {
	f.runsMu.Lock()
	defer f.runsMu.Unlock()
	return f.cfg.Slots - len(f.runs)
}
