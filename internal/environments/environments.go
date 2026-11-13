// Package environments reads the runner's environments file: for each
// taskboard environment, by its name there, the secrets a suite run against
// it may use. Only secrets live in this file. The base URL and the plain
// variables stay in taskboard.
//
//	staging:
//	  admin_password: { env: STAGING_ADMIN_PASSWORD }  # from the runner's environment
//	  api_key:        { file: /run/secrets/staging_api_key }
//	  webhook_secret: "inline value"
//
// A value from env or file is read when a run asks for it and is never
// copied anywhere else.
package environments

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/Risa-AI/taskboard-runner/pkg/apitest"
	"go.yaml.in/yaml/v3"
)

// Source is where one secret's value comes from: exactly one of Inline, Env
// or File.
type Source struct {
	Inline string
	Env    string
	File   string
}

// UnmarshalYAML reads a source: a plain string, or {env: VAR}, or
// {file: PATH}.
func (s *Source) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		switch n.Tag {
		case "!!null": // left empty: the read says which secret is empty
		case "!!str":
			s.Inline = n.Value
		default:
			return fmt.Errorf("line %d: an inline secret must be a string; put it in quotes", n.Line)
		}
		return nil
	}
	var m map[string]string
	if n.Kind != yaml.MappingNode || n.Decode(&m) != nil || len(m) != 1 {
		return fmt.Errorf("line %d: a secret is a string, {env: VAR} or {file: PATH}", n.Line)
	}
	switch {
	case m["env"] != "":
		s.Env = m["env"]
	case m["file"] != "":
		s.File = m["file"]
	default:
		return fmt.Errorf("line %d: a secret is a string, {env: VAR} or {file: PATH}", n.Line)
	}
	return nil
}

// File is a loaded environments file.
type File struct {
	path string
	envs map[string]map[string]Source
}

// Load reads the environments file at path. It is refused if anyone but its
// owner may read or write it: it holds secrets, or says where they are.
func Load(path string) (File, error) {
	f := File{path: path}
	info, err := os.Stat(path)
	if err != nil {
		return f, err
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		return f, fmt.Errorf("%s is open to other users (mode %04o); run chmod 600 %s", path, mode, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return f, err
	}
	if err := yaml.Unmarshal(data, &f.envs); err != nil {
		return f, fmt.Errorf("%s: %s", path, withoutValues(err))
	}
	for env, secrets := range f.envs {
		if strings.TrimSpace(env) == "" {
			return f, fmt.Errorf("%s: an environment has no name", path)
		}
		for name := range secrets {
			if !apitest.ValidVarName(name) {
				return f, fmt.Errorf("%s: %s: %q is not a variable name", path, env, name)
			}
		}
	}
	return f, nil
}

// Environments lists the environments the file has, sorted.
func (f File) Environments() []string {
	out := make([]string, 0, len(f.envs))
	for env := range f.envs {
		out = append(out, env)
	}
	sort.Strings(out)
	return out
}

// Names lists env's secret names, sorted. It reads no value.
func (f File) Names(env string) ([]string, error) {
	secrets, ok := f.envs[env]
	if !ok {
		have := strings.Join(f.Environments(), ", ")
		if have == "" {
			have = "none"
		}
		return nil, fmt.Errorf("%s has no environment %q; it has: %s", f.path, env, have)
	}
	out := make([]string, 0, len(secrets))
	for name := range secrets {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// Secrets reads env's secret values. A secret that is empty, or whose
// variable or file is missing, fails the whole read, naming the secret and
// never a value. A file's value loses its trailing line break; a relative
// file path is relative to the environments file.
func (f File) Secrets(env string) (map[string]string, error) {
	names, err := f.Names(env)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(names))
	for _, name := range names {
		v, err := f.read(f.envs[env][name])
		if err != nil {
			return nil, fmt.Errorf("secret %s of environment %s: %w", name, env, err)
		}
		if v == "" {
			return nil, fmt.Errorf("secret %s of environment %s is empty", name, env)
		}
		out[name] = v
	}
	return out, nil
}

var errLine = regexp.MustCompile(`^line \d+`)

// withoutValues is a parse error with nothing of the file's content in it:
// yaml's type errors quote the value they could not decode, and in this file
// that value may be a secret written in the wrong place.
func withoutValues(err error) string {
	var te *yaml.TypeError
	if !errors.As(err, &te) {
		return err.Error()
	}
	var lines []string
	for _, e := range te.Errors {
		if at := errLine.FindString(e); at != "" {
			lines = append(lines, at)
		}
	}
	return "the file maps each environment to its secrets (name: value, {env: VAR} or {file: PATH}); see " + strings.Join(lines, ", ")
}

func (f File) read(s Source) (string, error) {
	switch {
	case s.Env != "":
		v, ok := os.LookupEnv(s.Env)
		if !ok {
			return "", fmt.Errorf("variable %s is not set", s.Env)
		}
		return v, nil
	case s.File != "":
		p := s.File
		if !filepath.IsAbs(p) {
			p = filepath.Join(filepath.Dir(f.path), p)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			var pe *os.PathError
			if errors.As(err, &pe) {
				return "", fmt.Errorf("cannot read %s: %v", p, pe.Err)
			}
			return "", err
		}
		return strings.TrimRight(string(data), "\r\n"), nil
	default:
		return s.Inline, nil
	}
}
