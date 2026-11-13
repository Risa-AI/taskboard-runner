package apitest

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// assertBody checks expect_body against a response. Each key is a dotted path
// (the same shape as capture); each value is an exact value (after ${var}
// substitution), a plain nested object (matched field by field), or an
// operator object whose keys start with "$".
//
// Keys are checked in sorted order, where the original runner walked Go's map in random
// order: with two wrong fields the reported one changed from run to run, and
// the same input must give the same result.
func (r *Runner) assertBody(spec map[string]interface{}, body []byte) error {
	var root interface{}
	if err := json.Unmarshal(body, &root); err != nil {
		return fmt.Errorf("the response is not JSON: %w", err)
	}
	for _, path := range sortedKeys(spec) {
		if err := r.matchValue(path, lookupPath(root, path), spec[path]); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runner) matchValue(path string, actual, expected interface{}) error {
	if m, ok := expected.(map[string]interface{}); ok {
		if hasOperatorKeys(m) {
			return r.matchOperators(path, actual, m)
		}
		// A plain nested object recurses field by field; a missing actual
		// field reaches the leaf as nil and fails there with its full path.
		actMap, _ := actual.(map[string]interface{})
		for _, k := range sortedKeys(m) {
			var child interface{}
			if actMap != nil {
				child = actMap[k]
			}
			if err := r.matchValue(path+"."+k, child, m[k]); err != nil {
				return err
			}
		}
		return nil
	}
	expected = r.substituteAny(expected)
	if !equalJSONValue(actual, expected) {
		return fmt.Errorf("%s: expected %v, got %v", path, show(expected), show(actual))
	}
	return nil
}

func hasOperatorKeys(m map[string]interface{}) bool {
	for k := range m {
		if strings.HasPrefix(k, "$") {
			return true
		}
	}
	return false
}

// operators is every operator a case may use. validate.go reads it too, so
// the validator and the runner cannot disagree about what exists.
var operators = map[string]bool{"$exists": true, "$type": true, "$len": true, "$regex": true, "$contains": true}

// jsonTypes is every value $type accepts.
var jsonTypes = map[string]bool{"string": true, "number": true, "bool": true, "array": true, "object": true, "null": true}

func (r *Runner) matchOperators(path string, actual interface{}, ops map[string]interface{}) error {
	for _, op := range sortedKeys(ops) {
		arg := ops[op]
		switch op {
		case "$exists":
			want, _ := arg.(bool)
			if present := actual != nil; present != want {
				return fmt.Errorf("%s: $exists=%t but actual present=%t", path, want, present)
			}
		case "$type":
			want, _ := arg.(string)
			if got := jsonTypeOf(actual); got != want {
				return fmt.Errorf("%s: $type=%q but actual is %q", path, want, got)
			}
		case "$len":
			want := toInt(arg)
			got := jsonLen(actual)
			if got < 0 {
				return fmt.Errorf("%s: $len requires string/array/object, got %s", path, jsonTypeOf(actual))
			}
			if got != want {
				return fmt.Errorf("%s: $len=%d but actual length is %d", path, want, got)
			}
		case "$regex":
			pat, _ := arg.(string)
			pat = r.substitute(pat)
			re, err := regexp.Compile(pat)
			if err != nil {
				return fmt.Errorf("%s: $regex bad pattern %q: %w", path, pat, err)
			}
			s, ok := actual.(string)
			if !ok {
				return fmt.Errorf("%s: $regex requires string, got %s", path, jsonTypeOf(actual))
			}
			if !re.MatchString(s) {
				return fmt.Errorf("%s: $regex %q did not match %q", path, pat, s)
			}
		case "$contains":
			needle := r.substituteAny(arg)
			if !jsonContains(actual, needle) {
				return fmt.Errorf("%s: $contains: %v not in %v", path, show(needle), show(actual))
			}
		default:
			return fmt.Errorf("%s: unknown operator %q", path, op)
		}
	}
	return nil
}

// lookupPath walks a dotted path into decoded JSON. A number segment indexes
// an array ("data.rules.0.type"); anything else is a map key. A missing key,
// an index out of range and a type mismatch all give nil, which assertions
// report as "got <nil>".
func lookupPath(root interface{}, path string) interface{} {
	cur := root
	for _, seg := range strings.Split(path, ".") {
		switch node := cur.(type) {
		case map[string]interface{}:
			cur = node[seg]
		case []interface{}:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(node) {
				return nil
			}
			cur = node[i]
		default:
			return nil
		}
	}
	return cur
}

func jsonTypeOf(v interface{}) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "bool"
	case float64, int, int64:
		return "number"
	case string:
		return "string"
	case []interface{}:
		return "array"
	case map[string]interface{}:
		return "object"
	}
	return "unknown"
}

func jsonLen(v interface{}) int {
	switch t := v.(type) {
	case string:
		return len(t)
	case []interface{}:
		return len(t)
	case map[string]interface{}:
		return len(t)
	}
	return -1
}

func toInt(v interface{}) int {
	switch t := v.(type) {
	case int:
		return t
	case int64:
		return int(t)
	case float64:
		return int(t)
	case string:
		n, _ := strconv.Atoi(t)
		return n
	}
	return 0
}

func jsonContains(haystack, needle interface{}) bool {
	switch h := haystack.(type) {
	case string:
		n, ok := needle.(string)
		return ok && strings.Contains(h, n)
	case []interface{}:
		for _, item := range h {
			if equalJSONValue(item, needle) {
				return true
			}
		}
	}
	return false
}

// equalJSONValue compares after a JSON round trip: YAML decodes 42 as an int
// and JSON as a float64, and reflect.DeepEqual calls those different.
func equalJSONValue(a, b interface{}) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	var an, bn interface{}
	_ = json.Unmarshal(ab, &an)
	_ = json.Unmarshal(bb, &bn)
	return reflect.DeepEqual(an, bn)
}

// show renders a value for a failure message: strings quoted, so "" and a
// missing value do not both read as nothing.
func show(v interface{}) string {
	switch t := v.(type) {
	case nil:
		return "<nil>"
	case string:
		return strconv.Quote(t)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
