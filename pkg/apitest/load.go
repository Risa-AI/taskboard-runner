package apitest

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// validName is what a scenario file may be called: a plain name ending in
// .yml or .yaml. It becomes a file on the executor and a key in a bundle, so
// nothing that could climb out of a directory gets through.
var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}\.ya?ml$`)

// ValidName reports whether name may name a scenario file.
func ValidName(name string) bool {
	return validName.MatchString(name) && !strings.Contains(name, "..")
}

var fixtureSegment = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

// ValidFixturePath reports whether p may name a fixture: the path a scenario
// writes after "@" (the original suite's "fixtures/logo.png"), relative to the suite,
// at most four segments of plain names. It becomes an object key and a file
// on the executor, so nothing that could climb out of its directory passes.
func ValidFixturePath(p string) bool {
	if p == "" || len(p) > 200 {
		return false
	}
	segs := strings.Split(p, "/")
	if len(segs) > 4 {
		return false
	}
	for _, s := range segs {
		if !fixtureSegment.MatchString(s) || strings.Contains(s, "..") {
			return false
		}
	}
	return true
}

// Parse reads one scenario file. Every case carries the line it starts on,
// and a key a case does not have is refused with that key's line -- a
// misspelt field decoded silently is a case that checks nothing and passes.
//
// A file of nothing but comments is an empty file, not an error: the original suite
// keeps section headers above the first case.
func Parse(name string, data []byte) (File, error) {
	if !ValidName(name) {
		return File{Name: name}, fmt.Errorf("%q is not a scenario file name: use letters, digits, '.', '_' or '-', ending in .yml or .yaml", name)
	}
	return parseCases(name, data)
}

// parseCases reads a list of cases, as Parse does, under any name: a role's
// login steps are cases too, reported as "role <name>".
func parseCases(name string, data []byte) (File, error) {
	file := File{Name: name}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return file, fmt.Errorf("%s: %s", name, strings.TrimPrefix(err.Error(), "yaml: "))
	}
	if doc.Kind == 0 || len(doc.Content) == 0 {
		return file, nil
	}
	root := doc.Content[0]
	if root.Kind != yaml.SequenceNode {
		return file, fmt.Errorf("%s:%d: a scenario file is a list of cases, each starting with '- '", name, root.Line)
	}
	for _, item := range root.Content {
		if item.Kind != yaml.MappingNode {
			return file, fmt.Errorf("%s:%d: a case is a map of fields (name, method, path, ...)", name, item.Line)
		}
		for i := 0; i+1 < len(item.Content); i += 2 {
			key := item.Content[i]
			if !caseFields[key.Value] {
				// A signing key written on the case read as "not supported" to
				// a model, which then dropped it and signed a payment gateway's GETs the
				// standard way; say where it goes.
				if signFields[key.Value] {
					return file, fmt.Errorf("%s:%d: %q goes inside sign: (sign: {snap: ..., %s: ...}), not on the case", name, key.Line, key.Value, key.Value)
				}
				return file, fmt.Errorf("%s:%d: unknown field %q; a case takes %s", name, key.Line, key.Value, fieldList())
			}
			if val := item.Content[i+1]; key.Value == "sign" && val.Kind == yaml.MappingNode {
				for j := 0; j+1 < len(val.Content); j += 2 {
					if k := val.Content[j]; !signFields[k.Value] {
						return file, fmt.Errorf("%s:%d: unknown sign field %q; sign takes snap, client_key, private_key, secret, token, empty_body, zone", name, k.Line, k.Value)
					}
				}
			}
			if val := item.Content[i+1]; key.Value == "fallback" && val.Kind == yaml.MappingNode {
				for j := 0; j+1 < len(val.Content); j += 2 {
					if k := val.Content[j]; !fallbackFields[k.Value] {
						return file, fmt.Errorf("%s:%d: fallback takes expect_status and expect_body only, not %q: the request is the case's own, sent as the default role", name, k.Line, k.Value)
					}
				}
			}
		}
		var c Case
		if err := item.Decode(&c); err != nil {
			return file, fmt.Errorf("%s:%d: %s", name, item.Line, strings.TrimPrefix(err.Error(), "yaml: "))
		}
		c.File, c.Line = name, item.Line
		file.Cases = append(file.Cases, c)
	}
	return file, nil
}

// fieldList is the field names, sorted, for a refusal that says what would
// have been accepted.
func fieldList() string {
	names := make([]string, 0, len(caseFields))
	for k := range caseFields {
		names = append(names, k)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// Order sorts files into run order: by name, as the original suite's loader does. Order
// matters because a file uses variables an earlier one captured, which is why
// the files carry numeric prefixes (01_health.yml, 02_auth.yml, 02a_...).
func Order(files []File) {
	sort.SliceStable(files, func(i, j int) bool { return files[i].Name < files[j].Name })
}

// Cases is every case of the files, in run order. The files must already be
// in order.
func Cases(files []File) []Case {
	var all []Case
	for _, f := range files {
		all = append(all, f.Cases...)
	}
	return all
}

// LoadDir reads every .yml and .yaml file in dir (not below it) and returns
// them in run order.
func LoadDir(dir string) ([]File, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !(strings.HasSuffix(name, ".yml") || strings.HasSuffix(name, ".yaml")) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		f, err := Parse(name, data)
		if err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		return nil, errors.New("no scenario files (*.yml, *.yaml) in " + dir)
	}
	Order(files)
	return files, nil
}
