package object

import (
	"errors"
	"fmt"
	"strings"
)

// MaxLabelLen is the longest a label key or value may be.
const MaxLabelLen = 63

// LabelPrefix is the namespace drover writes its own labels under. A user
// label may not use it, so a generated label can always be trusted to be
// generated.
const LabelPrefix = "drover.io/"

// ValidateLabels checks a metadata.labels block.
//
// Keys and values are the same shape as a name, plus dots and underscores,
// which is what makes `team=billing` and `app.kubernetes.io/part-of=api` both
// legal. Unlike names there is no filesystem reason to forbid uppercase, but
// forbidding it anyway means a selector never has to think about case.
func ValidateLabels(labels map[string]string) error {
	for k, v := range labels {
		if err := validateLabelKey(k); err != nil {
			return fmt.Errorf("label key %q: %w", k, err)
		}
		if err := validateLabelToken(v, true); err != nil {
			return fmt.Errorf("label %q: value %q: %w", k, v, err)
		}
	}
	return nil
}

func validateLabelKey(k string) error {
	if strings.HasPrefix(k, LabelPrefix) {
		return fmt.Errorf("the %s prefix is reserved for labels drover writes itself", LabelPrefix)
	}
	name := k
	if prefix, rest, ok := strings.Cut(k, "/"); ok {
		if err := validateLabelToken(prefix, true); err != nil {
			return fmt.Errorf("prefix: %w", err)
		}
		name = rest
	}
	return validateLabelToken(name, false)
}

// validateLabelToken checks one half of a label. Dots are allowed in a prefix
// and in a value (versions, domains) but not in a bare key, where a dot would
// read as a prefix separator that is not there.
func validateLabelToken(s string, allowDots bool) error {
	if s == "" {
		return errors.New("is empty")
	}
	if len(s) > MaxLabelLen {
		return fmt.Errorf("is %d characters, the maximum is %d", len(s), MaxLabelLen)
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			continue
		case c == '-', c == '_':
		case c == '.' && allowDots:
		case c >= 'A' && c <= 'Z':
			return fmt.Errorf("must be lowercase, so a selector never has to consider case")
		default:
			return fmt.Errorf("contains %q; use lowercase letters, digits, dashes and underscores", string(c))
		}
		if i == 0 || i == len(s)-1 {
			return fmt.Errorf("may not start or end with %q", string(c))
		}
	}
	return nil
}

// --- selectors ---

// Requirement is one clause of a selector.
type Requirement struct {
	Key    string
	Op     string // "=", "!=", "exists", "!exists"
	Value  string
	source string
}

// Selector is a conjunction of requirements: every one has to hold.
//
// The grammar is kubectl's, minus the parts nobody uses. `k=v`, `k!=v`, `k`
// for exists and `!k` for absent, comma-separated and ANDed. There are no set
// operators and no OR, because a selector that needs either of those is
// really asking for a label that does not exist yet.
type Selector []Requirement

// ParseSelector reads a selector expression. An empty expression matches
// everything, which is what makes it safe to pass one through unconditionally.
func ParseSelector(expr string) (Selector, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return nil, nil
	}
	var sel Selector
	for _, clause := range strings.Split(expr, ",") {
		clause = strings.TrimSpace(clause)
		if clause == "" {
			return nil, fmt.Errorf("selector %q has an empty clause", expr)
		}
		req, err := parseRequirement(clause)
		if err != nil {
			return nil, err
		}
		sel = append(sel, req)
	}
	return sel, nil
}

func parseRequirement(clause string) (Requirement, error) {
	req := Requirement{source: clause}
	switch {
	case strings.Contains(clause, "!="):
		k, v, _ := strings.Cut(clause, "!=")
		req.Key, req.Op, req.Value = strings.TrimSpace(k), "!=", strings.TrimSpace(v)
	case strings.Contains(clause, "="):
		k, v, _ := strings.Cut(clause, "=")
		// "k==v" is the same as "k=v"; accept it so a kubectl habit works.
		v = strings.TrimPrefix(strings.TrimSpace(v), "=")
		req.Key, req.Op, req.Value = strings.TrimSpace(k), "=", strings.TrimSpace(v)
	case strings.HasPrefix(clause, "!"):
		req.Key, req.Op = strings.TrimSpace(clause[1:]), "!exists"
	default:
		req.Key, req.Op = clause, "exists"
	}

	if req.Key == "" {
		return req, fmt.Errorf("selector clause %q has no key", clause)
	}
	// A key here may be one drover generated, so the reserved-prefix rule does
	// not apply -- selecting on drover.io/source is exactly the point.
	name := req.Key
	if prefix, rest, ok := strings.Cut(req.Key, "/"); ok {
		if err := validateLabelToken(prefix, true); err != nil {
			return req, fmt.Errorf("selector clause %q: prefix: %w", clause, err)
		}
		name = rest
	}
	if err := validateLabelToken(name, false); err != nil {
		return req, fmt.Errorf("selector clause %q: key: %w", clause, err)
	}
	if req.Op == "=" || req.Op == "!=" {
		if err := validateLabelToken(req.Value, true); err != nil {
			return req, fmt.Errorf("selector clause %q: value: %w", clause, err)
		}
	}
	return req, nil
}

// Matches reports whether a label set satisfies every requirement.
func (s Selector) Matches(labels map[string]string) bool {
	for _, r := range s {
		v, ok := labels[r.Key]
		switch r.Op {
		case "=":
			if !ok || v != r.Value {
				return false
			}
		case "!=":
			// An absent label satisfies "!=", the same way it does in
			// kubectl: "not on the billing team" includes "on no team".
			if ok && v == r.Value {
				return false
			}
		case "exists":
			if !ok {
				return false
			}
		case "!exists":
			if ok {
				return false
			}
		}
	}
	return true
}

// String renders the selector back, for an error message that quotes what the
// caller actually typed.
func (s Selector) String() string {
	parts := make([]string, 0, len(s))
	for _, r := range s {
		parts = append(parts, r.source)
	}
	return strings.Join(parts, ",")
}
