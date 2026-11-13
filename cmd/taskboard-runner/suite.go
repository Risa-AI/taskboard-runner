package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"sort"
	"strings"

	"github.com/Risa-AI/taskboard-runner/internal/environments"
	"github.com/Risa-AI/taskboard-runner/pkg/apitest"
	"go.yaml.in/yaml/v3"
)

// defaultEnvironments is where the container image mounts the file.
const defaultEnvironments = "/etc/tb/environments.yaml"

// common is what validate and test both read: the suite, its seeds, the
// environment's plain variables, and which environment's secrets apply.
type common struct {
	suite, seeds, vars, env, environments string
}

func (c *common) register(fs *flag.FlagSet) {
	fs.StringVar(&c.suite, "suite", "", "the directory of the suite's scenario files")
	fs.StringVar(&c.seeds, "seeds", "", "a YAML file of the suite's seeds")
	fs.StringVar(&c.vars, "vars", "", "a YAML file of the environment's plain variables: name: value")
	fs.StringVar(&c.env, "env", "", "the environment whose secrets apply, by its name in taskboard")
	fs.StringVar(&c.environments, "environments", "", "the environments file (default $TB_ENVIRONMENTS, else "+defaultEnvironments+")")
}

func (c *common) environmentsPath() string {
	switch {
	case c.environments != "":
		return c.environments
	case os.Getenv("TB_ENVIRONMENTS") != "":
		return os.Getenv("TB_ENVIRONMENTS")
	default:
		return defaultEnvironments
	}
}

// suite is a loaded suite: its files and seeds, the plain variables, and
// the environment's secrets (names only for validate).
type suite struct {
	files   []apitest.File
	seeds   apitest.Seeds
	vars    map[string]string
	secrets []string
	envFile environments.File
}

// load reads everything but the secrets' values. A name is a seed, a
// variable or a secret, never two: which value won would depend on the
// order they were applied.
func (c *common) load() (suite, error) {
	s := suite{seeds: apitest.Seeds{}, vars: map[string]string{}}
	if c.suite == "" {
		return s, errors.New("--suite is required")
	}
	files, err := apitest.LoadDir(c.suite)
	if err != nil {
		return s, err
	}
	s.files = files
	if err := readYAML(c.seeds, &s.seeds); err != nil {
		return s, err
	}
	if err := readYAML(c.vars, &s.vars); err != nil {
		return s, err
	}
	if c.env != "" {
		if s.envFile, err = environments.Load(c.environmentsPath()); err != nil {
			return s, err
		}
		if s.secrets, err = s.envFile.Names(c.env); err != nil {
			return s, err
		}
	}
	owner := map[string]string{}
	for _, name := range s.seeds.Names() {
		owner[name] = "a seed"
	}
	for _, name := range sortedKeys(s.vars) {
		if !apitest.ValidVarName(name) {
			return s, fmt.Errorf("variable %q is not a variable name", name)
		}
		if was, ok := owner[name]; ok {
			return s, fmt.Errorf("%s is both %s and a variable; give it one value", name, was)
		}
		owner[name] = "a variable"
	}
	for _, name := range s.secrets {
		if was, ok := owner[name]; ok {
			return s, fmt.Errorf("%s is both %s and a secret of %s; give it one value", name, was, c.env)
		}
	}
	return s, nil
}

func readYAML(path string, into interface{}) error {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := yaml.Unmarshal(data, into); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (s suite) problems() []apitest.Problem {
	k := apitest.Known{Seeds: s.seeds.Names(), Target: append(sortedKeys(s.vars), s.secrets...)}
	return apitest.Validate(s.files, k, apitest.Options{})
}

// validate checks a suite without sending a request: each problem as
// file:line: message, exit 1 if there is any, 2 if the suite cannot be read.
func validate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var c common
	c.register(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	s, err := c.load()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	problems := s.problems()
	for _, p := range problems {
		fmt.Fprintln(stdout, p.String())
	}
	if len(problems) > 0 {
		return 1
	}
	fmt.Fprintf(stdout, "%d files, %d cases: no problems\n", len(s.files), len(apitest.Cases(s.files)))
	return 0
}

// testSuite runs a suite from disk against one environment and prints each
// case, masked, then the summary. It sends nothing to taskboard. It exits 0
// when the run passed (skips included: the summary counts them), 1 when a
// case failed, and 2 when no case ran.
func testSuite(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var c common
	c.register(fs)
	baseURL := fs.String("base-url", "", "the environment's base URL, e.g. https://staging.example.com")
	fixtures := fs.String("fixtures", "", `the directory a form value "@path" is read from (default: the suite)`)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *baseURL == "" || c.env == "" {
		fmt.Fprintln(stderr, "--base-url and --env are required")
		return 2
	}
	build := apitest.Build{State: apitest.BuildNotChecked}
	s, err := c.load()
	if err != nil {
		summary(stdout, apitest.Refused(0, build, err.Error()))
		return 2
	}
	cases := apitest.Cases(s.files)
	if problems := s.problems(); len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintln(stdout, p.String())
		}
		summary(stdout, apitest.Refused(len(cases), build, fmt.Sprintf("the suite has %d problem(s); fix them before it runs", len(problems))))
		return 2
	}
	secrets, err := s.envFile.Secrets(c.env)
	if err != nil {
		summary(stdout, apitest.Refused(len(cases), build, err.Error()))
		return 2
	}
	start := now()
	resolved, err := s.seeds.Resolve(start, rand.New(rand.NewPCG(uint64(start.UnixNano()), 0)))
	if err != nil {
		summary(stdout, apitest.Refused(len(cases), build, err.Error()))
		return 2
	}

	r := apitest.NewRunner(*baseURL)
	r.Now = now
	r.Base = c.suite
	if *fixtures != "" {
		r.Base = *fixtures
	}
	resolved.Apply(r)
	for name, v := range s.vars {
		r.Seed(name, v)
	}
	values := make([]string, 0, len(secrets))
	for name, v := range secrets {
		r.Seed(name, v)
		values = append(values, v)
	}
	red := apitest.NewRedactor(values)

	if err := r.Ping(); err != nil {
		summary(stdout, apitest.Refused(len(cases), build, red.String(err.Error())))
		return 2
	}
	r.OnResult = func(res apitest.Result) {
		red.Learn(r.Vars)
		printResult(stdout, red.Result(res))
	}
	sum := apitest.Summarize(len(cases), r.Run(cases), build)
	summary(stdout, sum)
	if sum.Verdict == apitest.VerdictFailed {
		return 1
	}
	return 0
}

// printResult writes one case as an OK, SKIP or FAIL line. A failure shows
// the response it got, already masked.
func printResult(w io.Writer, res apitest.Result) {
	switch res.Outcome {
	case apitest.OutcomeOK:
		fmt.Fprintf(w, "OK    %s %s → %d\n", res.Method, res.Path, res.Status)
	case apitest.OutcomeSkip:
		fmt.Fprintf(w, "SKIP  %s (%s)\n", res.Name, res.Reason)
	default:
		fmt.Fprintf(w, "FAIL  %s %s (%s:%d %s): %s\n", res.Method, res.Path, res.File, res.Line, res.Name, res.Reason)
		if res.Response != "" {
			fmt.Fprintf(w, "      response: %s\n", res.Response)
		}
	}
}

func summary(w io.Writer, s apitest.Summary) {
	fmt.Fprintf(w, "\n%s: %d ok, %d skipped, %d failed, %d not run of %d\n", strings.ToUpper(strings.ReplaceAll(string(s.Verdict), "_", " ")), s.OK, s.Skipped, s.Failed, s.NotRun, s.Total)
	if s.Refusal != "" {
		fmt.Fprintln(w, s.Refusal)
	}
}
