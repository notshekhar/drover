package object

import (
	"fmt"
	"sort"
	"strings"
)

// Placeholder kinds. There are three syntaxes because there are three
// genuinely different sources, and collapsing them would let an agent reach a
// value it should never be able to set.
//
//	{{name}}  from the selected Environment (variables, then secrets)
//	${NAME}   from the server process environment
//	{name}    from a declared parameter, filled by the caller
//
// Only the third is ever exposed as a tool parameter.
type PlaceholderKind int

const (
	FromEnvironment PlaceholderKind = iota // {{name}}
	FromProcessEnv                         // ${NAME}
	FromParam                              // {name}
)

func (k PlaceholderKind) String() string {
	switch k {
	case FromEnvironment:
		return "environment variable"
	case FromProcessEnv:
		return "process environment"
	case FromParam:
		return "parameter"
	}
	return "unknown"
}

// Placeholder is one reference found in a template.
type Placeholder struct {
	Kind PlaceholderKind
	Name string
}

// isPlaceholderName reports whether the text between braces is a name rather
// than incidental content.
//
// This is what lets a JSON body template work at all. `{"tenant": "x"}` opens
// with a brace, and without this check the scanner would read
// `"tenant": "x"` as a parameter name and mangle the document. A name is an
// identifier: letters, digits, underscore, dash. Anything else -- a quote, a
// space, a colon -- means the braces are data, not a placeholder.
func isPlaceholderName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := c == '_' || c == '-' ||
			(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9')
		if !ok {
			return false
		}
	}
	return true
}

// ScanPlaceholders finds every reference in a template string.
//
// Order matters: {{...}} is matched before {...}, or every environment
// reference would also register as a stray parameter.
func ScanPlaceholders(s string) []Placeholder {
	var out []Placeholder
	for i := 0; i < len(s); {
		switch {
		case strings.HasPrefix(s[i:], "{{"):
			end := strings.Index(s[i+2:], "}}")
			if end < 0 {
				i += 2
				continue
			}
			name := strings.TrimSpace(s[i+2 : i+2+end])
			if !isPlaceholderName(name) {
				i++
				continue
			}
			out = append(out, Placeholder{FromEnvironment, name})
			i += 2 + end + 2

		case strings.HasPrefix(s[i:], "${"):
			end := strings.Index(s[i+2:], "}")
			if end < 0 {
				i += 2
				continue
			}
			name := strings.TrimSpace(s[i+2 : i+2+end])
			if !isPlaceholderName(name) {
				i++
				continue
			}
			out = append(out, Placeholder{FromProcessEnv, name})
			i += 2 + end + 1

		case s[i] == '{':
			end := strings.Index(s[i+1:], "}")
			if end < 0 {
				i++
				continue
			}
			name := strings.TrimSpace(s[i+1 : i+1+end])
			if !isPlaceholderName(name) {
				i++
				continue
			}
			out = append(out, Placeholder{FromParam, name})
			i += 1 + end + 1

		default:
			i++
		}
	}
	return out
}

// PlaceholderNames returns the distinct names of one kind, sorted.
func PlaceholderNames(s string, kind PlaceholderKind) []string {
	seen := map[string]bool{}
	for _, p := range ScanPlaceholders(s) {
		if p.Kind == kind {
			seen[p.Name] = true
		}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Resolver supplies values for the three sources. A nil lookup means that
// source has nothing, which is an error for any placeholder that needs it.
type Resolver struct {
	// Env is the selected Environment's variables and secrets.
	Env func(name string) (string, bool)
	// Process is the server process environment.
	Process func(name string) (string, bool)
	// Param is a caller-supplied parameter.
	Param func(name string) (string, bool)

	// EnvName names the selected environment, for error messages. Knowing
	// which stage was selected is most of the diagnosis.
	EnvName string

	// Escape transforms a resolved value before substitution, used to
	// percent-encode values going into a URL.
	Escape func(kind PlaceholderKind, value string) string
}

// Resolve substitutes every placeholder in s.
//
// Resolution is single-pass: a value that itself contains {{...}} is not
// expanded again. That rules out recursion and expansion bombs, and means a
// value containing braces is data rather than a template.
//
// An unresolved placeholder is an error naming what was missing and which
// environment was selected. It is never substituted with an empty string --
// sending a URL with a literal {{baseUrl}} in it, or silently hitting the
// wrong host, is worse than failing.
func (r *Resolver) Resolve(s string) (string, error) {
	var b strings.Builder
	var missing []string

	for i := 0; i < len(s); {
		var (
			kind    PlaceholderKind
			name    string
			consume int
			matched bool
		)

		switch {
		case strings.HasPrefix(s[i:], "{{"):
			if end := strings.Index(s[i+2:], "}}"); end >= 0 {
				kind, name, consume, matched = FromEnvironment, strings.TrimSpace(s[i+2:i+2+end]), 2+end+2, true
			}
		case strings.HasPrefix(s[i:], "${"):
			if end := strings.Index(s[i+2:], "}"); end >= 0 {
				kind, name, consume, matched = FromProcessEnv, strings.TrimSpace(s[i+2:i+2+end]), 2+end+1, true
			}
		case s[i] == '{':
			if end := strings.Index(s[i+1:], "}"); end >= 0 {
				kind, name, consume, matched = FromParam, strings.TrimSpace(s[i+1:i+1+end]), 1+end+1, true
			}
		}

		if !matched || !isPlaceholderName(name) {
			b.WriteByte(s[i])
			i++
			continue
		}

		value, ok := r.lookup(kind, name)
		if !ok {
			missing = append(missing, describeMissing(kind, name, r.EnvName))
			i += consume
			continue
		}
		if r.Escape != nil {
			value = r.Escape(kind, value)
		}
		b.WriteString(value)
		i += consume
	}

	if len(missing) > 0 {
		return "", fmt.Errorf("unresolved %s", strings.Join(dedupe(missing), ", "))
	}
	return b.String(), nil
}

func (r *Resolver) lookup(kind PlaceholderKind, name string) (string, bool) {
	switch kind {
	case FromEnvironment:
		if r.Env == nil {
			return "", false
		}
		return r.Env(name)
	case FromProcessEnv:
		if r.Process == nil {
			return "", false
		}
		return r.Process(name)
	case FromParam:
		if r.Param == nil {
			return "", false
		}
		return r.Param(name)
	}
	return "", false
}

func describeMissing(kind PlaceholderKind, name, envName string) string {
	switch kind {
	case FromEnvironment:
		if envName != "" {
			return fmt.Sprintf("{{%s}} (not set in environment %q)", name, envName)
		}
		return fmt.Sprintf("{{%s}} (no environment selected)", name)
	case FromProcessEnv:
		return fmt.Sprintf("${%s} (not set in the engine's environment)", name)
	default:
		return fmt.Sprintf("{%s} (no value given)", name)
	}
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
