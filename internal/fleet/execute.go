package fleet

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Risa-AI/taskboard-runner/internal/environments"
	"github.com/Risa-AI/taskboard-runner/pkg/apitest"
	"github.com/Risa-AI/taskboard-runner/pkg/protocol"
)

// maxFixture is the largest file a case may upload; taskboard stores none
// larger.
const maxFixture = 1 << 20

// execute runs one claimed job and reports it. Nothing it posts holds a
// secret: every case is masked before it is sent, and so is any reason a
// run could not run.
func (f *fleet) execute(parent context.Context, job protocol.Job) {
	limit := time.Duration(job.TimeoutS) * time.Second
	if limit <= 0 {
		limit = f.cfg.DefaultTimeout
	}
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	timer := time.AfterFunc(limit, func() { cancel(errTimeLimit) })
	defer timer.Stop()
	f.addRun(job.RunID, cancel)
	defer f.removeRun(job.RunID)

	// Fixtures go into a directory of this run's own, gone when it ends.
	dir, err := os.MkdirTemp("", "taskboard-run-")
	if err != nil {
		f.complete(ctx, job.RunID, refused(0, err.Error()))
		return
	}
	defer os.RemoveAll(dir)

	r, cases, red, why := f.prepare(ctx, job, dir)
	if why != "" {
		if red != nil {
			why = red.String(why)
		}
		f.complete(ctx, job.RunID, refused(len(cases), why))
		return
	}

	var posted []apitest.Result
	r.OnResult = func(res apitest.Result) {
		if ctx.Err() != nil {
			return
		}
		red.Learn(r.Vars)
		res = red.Result(res)
		if err := f.postCase(ctx, job.RunID, res); err != nil {
			var ref *Refusal
			switch {
			case fatal(err):
				f.halt(err)
				cancel(err)
			case errors.As(err, &ref) && (ref.Status == http.StatusNotFound || ref.Status == http.StatusConflict):
				cancel(errEnded)
			case ctx.Err() == nil:
				f.logf("run %s: case %d was not stored: %v", job.RunID, res.Index, err)
			}
			return
		}
		posted = append(posted, res)
	}
	results := r.Run(cases)

	switch cause := context.Cause(ctx); {
	case cause == nil:
		f.complete(ctx, job.RunID, protocol.Complete{Summary: summaryOf(apitest.Summarize(len(cases), results, apitest.Build{State: apitest.BuildNotChecked}))})
	case errors.Is(cause, errTimeLimit):
		f.complete(context.WithoutCancel(ctx), job.RunID, stopped(len(cases), posted, fmt.Sprintf("the run reached its time limit of %s", limit)))
	case errors.Is(cause, errCancelled), errors.Is(cause, errEnded):
		f.logf("run %s: %v; stopped", job.RunID, cause)
	case fatal(cause):
	default:
		// The runner itself is stopping. Say so rather than leave the run
		// to be failed as silent once its lease runs out.
		f.complete(context.WithoutCancel(ctx), job.RunID, stopped(len(cases), posted, "the runner was stopped before the run finished"))
	}
}

// prepare turns a job into a runner and its cases. A non-empty why says
// the run cannot run, before any request reaches the target. red is nil
// until the secrets are read.
func (f *fleet) prepare(ctx context.Context, job protocol.Job, dir string) (*apitest.Runner, []apitest.Case, *apitest.Redactor, string) {
	files := make([]apitest.File, 0, len(job.Suite.Files))
	for _, sf := range job.Suite.Files {
		file, err := apitest.Parse(sf.Name, []byte(sf.Content))
		if err != nil {
			return nil, nil, nil, err.Error()
		}
		files = append(files, file)
	}
	apitest.Order(files)
	cases := apitest.Cases(files)
	seeds := job.Suite.Seeds
	if seeds == nil {
		seeds = apitest.Seeds{}
	}
	env := job.Environment

	file, err := environments.Load(f.cfg.Environments)
	if err != nil {
		return nil, cases, nil, err.Error()
	}
	names, err := file.Names(env.Name)
	if err != nil {
		return nil, cases, nil, fmt.Sprintf("this fleet holds no secrets for environment %q; add it to the environments file", env.Name)
	}
	if why := oneOwner(seeds, env.Variables, names, env.Name); why != "" {
		return nil, cases, nil, why
	}
	target := append(sortedKeys(env.Variables), names...)
	if problems := apitest.Validate(files, apitest.Known{Seeds: seeds.Names(), Target: target}, apitest.Options{}); len(problems) > 0 {
		lines := make([]string, 0, 3)
		for i, p := range problems {
			if i == 3 {
				break
			}
			lines = append(lines, p.String())
		}
		return nil, cases, nil, fmt.Sprintf("the suite has %d problem(s): %s", len(problems), strings.Join(lines, "; "))
	}
	// Read now, used for this run only, and never written anywhere.
	secrets, err := file.Secrets(env.Name)
	if err != nil {
		return nil, cases, nil, err.Error()
	}
	values := make([]string, 0, len(secrets))
	for _, v := range secrets {
		values = append(values, v)
	}
	red := apitest.NewRedactor(values)

	if err := fetchFixtures(ctx, dir, job.Suite.Fixtures); err != nil {
		return nil, cases, red, err.Error()
	}
	start := time.Now()
	resolved, err := seeds.Resolve(start, rand.New(rand.NewPCG(uint64(start.UnixNano()), 0)))
	if err != nil {
		return nil, cases, red, err.Error()
	}

	r := apitest.NewRunner(env.BaseURL)
	r.Base = dir
	// The engine has no way to be stopped, so its requests and its waits are
	// tied to the run: once the run is cancelled or out of time, the cases
	// left fail at once and nothing more is posted.
	r.Client.Transport = runTransport{ctx: ctx, base: http.DefaultTransport}
	r.Sleep = func(d time.Duration) { _ = sleepCtx(ctx, d) }
	resolved.Apply(r)
	for name, v := range env.Variables {
		r.Seed(name, v)
	}
	for name, v := range secrets {
		r.Seed(name, v)
	}
	if err := r.Ping(); err != nil {
		return nil, cases, red, err.Error()
	}
	return r, cases, red, ""
}

// oneOwner refuses a name that is two of seed, variable and secret: which
// value won would depend on the order they were applied.
func oneOwner(seeds apitest.Seeds, vars map[string]string, secrets []string, env string) string {
	owner := map[string]string{}
	for _, name := range seeds.Names() {
		owner[name] = "a seed"
	}
	for _, name := range sortedKeys(vars) {
		if was, ok := owner[name]; ok {
			return fmt.Sprintf("%s is both %s and a variable of %s; give it one value", name, was, env)
		}
		owner[name] = "a variable of " + env
	}
	for _, name := range secrets {
		if was, ok := owner[name]; ok {
			return fmt.Sprintf("%s is both %s and a secret of %s on this fleet; give it one value", name, was, env)
		}
	}
	return ""
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// fetchFixtures downloads each fixture into dir. The links are taskboard's
// short-lived ones, so the fleet token is not sent with them.
func fetchFixtures(ctx context.Context, dir string, fixtures []protocol.Fixture) error {
	hc := &http.Client{Timeout: time.Minute}
	for _, fx := range fixtures {
		if !apitest.ValidFixturePath(fx.Name) {
			return fmt.Errorf("fixture %q is not a fixture path", fx.Name)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fx.URL, nil)
		if err != nil {
			return fmt.Errorf("fixture %s has no usable link", fx.Name)
		}
		resp, err := hc.Do(req)
		if err != nil {
			var ue *url.Error
			if errors.As(err, &ue) {
				err = ue.Err
			}
			return fmt.Errorf("fixture %s could not be downloaded: %v", fx.Name, err)
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, maxFixture+1))
		resp.Body.Close()
		switch {
		case resp.StatusCode != http.StatusOK:
			return fmt.Errorf("fixture %s could not be downloaded: the link answered %d", fx.Name, resp.StatusCode)
		case err != nil:
			return fmt.Errorf("fixture %s could not be downloaded: %v", fx.Name, err)
		case len(data) > maxFixture:
			return fmt.Errorf("fixture %s is larger than 1 MB", fx.Name)
		}
		p := filepath.Join(dir, filepath.FromSlash(fx.Name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(p, data, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func (f *fleet) postCase(ctx context.Context, runID string, res apitest.Result) error {
	c := protocol.CaseResult{
		Index: res.Index, File: res.File, Line: res.Line, Name: res.Name, Method: res.Method, Path: res.Path,
		Status: res.Status, Outcome: string(res.Outcome), DurationMS: res.DurationMS,
		Reason: res.Reason, Request: res.Request, Response: res.Response,
	}
	_, err := f.api.call(ctx, http.MethodPost, "/runs/"+url.PathEscape(runID)+"/cases", []protocol.CaseResult{c}, nil)
	return err
}

// complete sends a run's last call. Within a minute; past that taskboard
// fails the run as silent anyway.
func (f *fleet) complete(ctx context.Context, runID string, c protocol.Complete) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	s := c.Summary
	if c.Error != "" {
		f.logf("run %s: %s: %s", runID, s.Verdict, c.Error)
	} else {
		f.logf("run %s: %s: %d ok, %d skipped, %d failed, %d not run of %d", runID, s.Verdict, s.OK, s.Skipped, s.Failed, s.NotRun, s.Total)
	}
	if _, err := f.api.call(ctx, http.MethodPost, "/runs/"+url.PathEscape(runID)+"/complete", c, nil); err != nil {
		f.stopOn(err)
	}
}

func summaryOf(s apitest.Summary) protocol.Summary {
	return protocol.Summary{Verdict: string(s.Verdict), Total: s.Total, OK: s.OK, Skipped: s.Skipped, Failed: s.Failed, NotRun: s.NotRun}
}

// refused is a run in which no case ran.
func refused(total int, why string) protocol.Complete {
	return protocol.Complete{Summary: summaryOf(apitest.Refused(total, apitest.Build{State: apitest.BuildNotChecked}, why)), Error: why}
}

// stopped is a run cut short: the cases it reported, the rest not run, and
// failed, whatever those cases said, because it did not finish.
func stopped(total int, posted []apitest.Result, why string) protocol.Complete {
	s := summaryOf(apitest.Summarize(total, posted, apitest.Build{State: apitest.BuildNotChecked}))
	s.Verdict = string(apitest.VerdictFailed)
	return protocol.Complete{Summary: s, Error: why}
}

// runTransport ties the engine's requests to the run: a request started
// after the run ended fails at once, and one in flight is cut off.
type runTransport struct {
	ctx  context.Context
	base http.RoundTripper
}

func (t runTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.ctx.Err() != nil {
		return nil, context.Cause(t.ctx)
	}
	ctx, cancel := context.WithCancel(req.Context())
	stop := context.AfterFunc(t.ctx, cancel)
	resp, err := t.base.RoundTrip(req.WithContext(ctx))
	if err != nil {
		stop()
		cancel()
		return nil, err
	}
	resp.Body = &releasing{ReadCloser: resp.Body, release: func() { stop(); cancel() }}
	return resp, nil
}

type releasing struct {
	io.ReadCloser
	release func()
}

func (r *releasing) Close() error {
	err := r.ReadCloser.Close()
	r.release()
	return err
}
