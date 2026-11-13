package apitest

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	// The zone database, embedded: a seed says "Asia/Jakarta", and the
	// executor image is a slim one that may carry no /usr/share/zoneinfo.
	_ "time/tzdata"
)

// Seed is one variable a run starts with. The original suite computed these in Go
// (e2e_test.go); a runner serving many projects needs them as data, so each
// seed names exactly one source and, for a time, how to shape it.
//
// Sources, exactly one per seed:
//   - Value: a literal ("09:00").
//   - Random: "base36" -- the run's start in nanoseconds, in base 36, as
//     the original suite's ${rand}; unique per run, so re-runs never collide on a
//     unique column.
//   - Digits: N random decimal digits, zero-padded (the original suite's phoneSuffix).
//   - Template: other seeds substituted into a string ("0812${phoneSuffix}").
//   - Time: an IANA zone; the seed is the run's start in that zone.
//   - From: another time seed, which this one shapes further.
//
// A time seed (Time or From) may be shaped, in this order: At sets the clock
// time ("10:00"), Days adds calendar days, Offset adds a duration ("2h").
// Its value is Format (a Go layout) of the instant, RFC 3339 without one, or
// with Weekday its day of the week (0 = Sunday). Two seeds made From one
// instant share it -- which is the whole of the original suite's booking-pair rule:
// the date and the time of a booking must come from the same instant, or at
// 23:00 "+2h" gives today's date and tomorrow's 01:00.
//
// Number makes the value a JSON number where a request leaf is exactly
// "${name}" (Runner.SeedNumber).
type Seed struct {
	Value    *string `json:"value,omitempty" yaml:"value"`
	Random   string  `json:"random,omitempty" yaml:"random"`
	Digits   int     `json:"digits,omitempty" yaml:"digits"`
	Template string  `json:"template,omitempty" yaml:"template"`
	Time     string  `json:"time,omitempty" yaml:"time"`
	From     string  `json:"from,omitempty" yaml:"from"`

	At      string `json:"at,omitempty" yaml:"at"`
	Days    int    `json:"days,omitempty" yaml:"days"`
	Offset  string `json:"offset,omitempty" yaml:"offset"`
	Format  string `json:"format,omitempty" yaml:"format"`
	Weekday bool   `json:"weekday,omitempty" yaml:"weekday"`

	Number bool `json:"number,omitempty" yaml:"number"`
}

// Seeds is a target's seeds by variable name.
type Seeds map[string]Seed

// RandSource is where Digits draws from; math/rand/v2's *Rand satisfies it,
// and a test passes a fixed one.
type RandSource interface {
	IntN(n int) int
}

var varName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// ValidVarName reports whether name can be written as ${name}.
func ValidVarName(name string) bool { return varName.MatchString(name) }

// Resolved is what seeds produce: every value as a string, and the ones that
// are numbers again as numbers.
type Resolved struct {
	Vars    map[string]string
	Numbers map[string]float64
}

// Apply seeds a runner with them.
func (r Resolved) Apply(run *Runner) {
	for k, v := range r.Vars {
		run.Seed(k, v)
	}
	for k, n := range r.Numbers {
		run.SeedNumber(k, n)
	}
}

// Resolve computes every seed at now. It refuses, by name, a seed with no
// source or two, a From or Template naming a seed that does not exist or is
// not a time, a cycle, a bad zone, clock time or offset, and a Number whose
// value is not one. The error names the first seed in name order, so the same
// spec always gives the same refusal.
func (s Seeds) Resolve(now time.Time, rng RandSource) (Resolved, error) {
	out := Resolved{Vars: map[string]string{}, Numbers: map[string]float64{}}
	instants := map[string]time.Time{}
	state := map[string]int{} // 0 unseen, 1 resolving, 2 done

	var resolve func(name string, path []string) error
	resolve = func(name string, path []string) error {
		switch state[name] {
		case 2:
			return nil
		case 1:
			return fmt.Errorf("seed %s: a cycle: %s", name, strings.Join(append(path, name), " -> "))
		}
		seed, ok := s[name]
		if !ok {
			return fmt.Errorf("seed %s: no such seed", name)
		}
		state[name] = 1
		path = append(path, name)
		if err := checkShape(name, seed); err != nil {
			return err
		}

		var value string
		switch {
		case seed.Value != nil:
			value = *seed.Value
		case seed.Random != "":
			value = strconv.FormatInt(now.UnixNano(), 36)
		case seed.Digits > 0:
			var b strings.Builder
			for i := 0; i < seed.Digits; i++ {
				b.WriteByte(byte('0' + rng.IntN(10)))
			}
			value = b.String()
		case seed.Template != "":
			for _, m := range varRE.FindAllStringSubmatch(seed.Template, -1) {
				if _, ok := s[m[1]]; !ok {
					return fmt.Errorf("seed %s: the template uses ${%s}, which is not a seed", name, m[1])
				}
				if err := resolve(m[1], path); err != nil {
					return err
				}
			}
			value = varRE.ReplaceAllStringFunc(seed.Template, func(m string) string {
				return out.Vars[varRE.FindStringSubmatch(m)[1]]
			})
		default: // Time or From
			var base time.Time
			if seed.Time != "" {
				loc, err := time.LoadLocation(seed.Time)
				if err != nil {
					return fmt.Errorf("seed %s: %q is not a time zone (use an IANA name such as Asia/Jakarta)", name, seed.Time)
				}
				base = now.In(loc)
			} else {
				from, ok := s[seed.From]
				if !ok {
					return fmt.Errorf("seed %s: from %q, which is not a seed", name, seed.From)
				}
				if from.Time == "" && from.From == "" {
					return fmt.Errorf("seed %s: from %q, which is not a time seed", name, seed.From)
				}
				if err := resolve(seed.From, path); err != nil {
					return err
				}
				base = instants[seed.From]
			}
			at, err := shape(name, seed, base)
			if err != nil {
				return err
			}
			instants[name] = at
			switch {
			case seed.Weekday:
				value = strconv.Itoa(int(at.Weekday()))
			case seed.Format != "":
				value = at.Format(seed.Format)
			default:
				value = at.Format(time.RFC3339)
			}
		}

		out.Vars[name] = value
		if seed.Number {
			n, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return fmt.Errorf("seed %s: number is set but %q is not a number", name, value)
			}
			out.Numbers[name] = n
		}
		state[name] = 2
		return nil
	}

	names := make([]string, 0, len(s))
	for name := range s {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := resolve(name, nil); err != nil {
			return Resolved{}, err
		}
	}
	return out, nil
}

// checkShape refuses a seed that names no source or several, and shaping on a
// seed that is not a time.
func checkShape(name string, seed Seed) error {
	if !ValidVarName(name) {
		return fmt.Errorf("seed %q: a variable name is letters, digits and '_', not starting with a digit", name)
	}
	sources := 0
	for _, set := range []bool{seed.Value != nil, seed.Random != "", seed.Digits > 0, seed.Template != "", seed.Time != "", seed.From != ""} {
		if set {
			sources++
		}
	}
	if sources != 1 {
		return fmt.Errorf("seed %s: give exactly one of value, random, digits, template, time or from (it has %d)", name, sources)
	}
	if seed.Random != "" && seed.Random != "base36" {
		return fmt.Errorf("seed %s: random is %q; the one kind is base36", name, seed.Random)
	}
	if seed.Digits > 18 {
		return fmt.Errorf("seed %s: digits is %d; at most 18", name, seed.Digits)
	}
	isTime := seed.Time != "" || seed.From != ""
	if !isTime && (seed.At != "" || seed.Days != 0 || seed.Offset != "" || seed.Format != "" || seed.Weekday) {
		return fmt.Errorf("seed %s: at, days, offset, format and weekday shape a time seed (time or from)", name)
	}
	if seed.Weekday && seed.Format != "" {
		return fmt.Errorf("seed %s: weekday and format both say what the value is; give one", name)
	}
	return nil
}

// shape applies At, then Days, then Offset.
func shape(name string, seed Seed, t time.Time) (time.Time, error) {
	if seed.At != "" {
		hm, err := time.Parse("15:04", seed.At)
		if err != nil {
			return t, fmt.Errorf("seed %s: at is %q; write a clock time such as 10:00", name, seed.At)
		}
		t = time.Date(t.Year(), t.Month(), t.Day(), hm.Hour(), hm.Minute(), 0, 0, t.Location())
	}
	if seed.Days != 0 {
		t = t.AddDate(0, 0, seed.Days)
	}
	if seed.Offset != "" {
		d, err := time.ParseDuration(seed.Offset)
		if err != nil {
			return t, fmt.Errorf("seed %s: offset is %q; write a duration such as 2h or -30m", name, seed.Offset)
		}
		t = t.Add(d)
	}
	return t, nil
}

// Names is the seeds' variable names, for the validator.
func (s Seeds) Names() []string {
	names := make([]string, 0, len(s))
	for k := range s {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
