package main

import (
	"fmt"
	"strings"
)

// usageError makes main exit 2.
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func usagef(format string, a ...any) error { return usageError{fmt.Sprintf(format, a...)} }

// spec maps every accepted spelling to a canonical name; valued says whether
// the option takes an argument.
type spec struct {
	names  map[string]string
	valued map[string]bool
}

func newSpec(opts ...string) spec {
	s := spec{names: map[string]string{}, valued: map[string]bool{}}
	// Each entry is "long[,short][=]", e.g. "language,l=" or "emotion,e".
	for _, o := range opts {
		valued := strings.HasSuffix(o, "=")
		o = strings.TrimSuffix(o, "=")
		parts := strings.Split(o, ",")
		canon := parts[0]
		s.valued[canon] = valued
		s.names["--"+canon] = canon
		for _, short := range parts[1:] {
			s.names["-"+short] = canon
		}
	}
	return s
}

type parsed struct {
	values     map[string]string
	flags      map[string]bool
	positional []string
}

// parse accepts options anywhere, `--opt value`, `--opt=value`, and `--` to
// end options. A lone `-` is a positional.
func (s spec) parse(args []string) (parsed, error) {
	p := parsed{values: map[string]string{}, flags: map[string]bool{}}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			p.positional = append(p.positional, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			p.positional = append(p.positional, a)
			continue
		}
		name, val, hasVal := strings.Cut(a, "=")
		canon, ok := s.names[name]
		if !ok {
			return p, usagef("unknown option: %s", name)
		}
		if !s.valued[canon] {
			if hasVal {
				return p, usagef("%s takes no value", name)
			}
			p.flags[canon] = true
			continue
		}
		if !hasVal {
			if i+1 >= len(args) {
				return p, usagef("%s needs a value", name)
			}
			i++
			val = args[i]
		}
		p.values[canon] = val
	}
	return p, nil
}
