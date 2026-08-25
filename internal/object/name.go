package object

import (
	"errors"
	"fmt"
)

// MaxNameLen is the longest a metadata.name may be, matching an RFC 1123
// label so a name is always a legal DNS label and a legal directory name.
const MaxNameLen = 63

// ValidateName enforces the naming rule for metadata.name.
//
// The pattern is ^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$ -- lowercase alphanumerics,
// dashes and at most one dot, not starting or ending with either, at most 63
// characters.
//
// The dot is the namespace separator, and it exists for one case: an object
// declared inside a repository is stored as <repository>.<name>, so two
// repositories cannot fight over one name and the origin of an object is
// visible in the object.
//
// Lowercase-only is load-bearing rather than cosmetic. A Repository name is
// also a directory under $DROVER_DATA/repos, and macOS filesystems are
// case-insensitive by default, so "API" and "api" would be two objects
// fighting over one checkout. Rejecting uppercase up front is cheaper than
// discovering that during a clone. The same pattern rules out ".", "..", "/"
// and leading dashes, so a name can never escape the data directory or be
// mistaken for a flag.
func ValidateName(name string) error {
	if name == "" {
		return errors.New("is required")
	}
	if len(name) > MaxNameLen {
		return fmt.Errorf("is %d characters, the maximum is %d", len(name), MaxNameLen)
	}
	dots := 0
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			continue
		case c == '.':
			dots++
			if dots > 1 {
				return fmt.Errorf("%q has more than one dot; the dot is the namespace separator and there is only one level", name)
			}
			if i == 0 || i == len(name)-1 {
				return fmt.Errorf("%q may not start or end with a dot", name)
			}
			continue
		case c == '-':
			if i == 0 || i == len(name)-1 {
				return fmt.Errorf("%q may not start or end with a dash", name)
			}
			continue
		case c >= 'A' && c <= 'Z':
			return fmt.Errorf("%q must be lowercase (names become directory names, and a case-insensitive filesystem would treat %q and its lowercase form as one)", name, name)
		default:
			return fmt.Errorf("%q contains %q; names may only use lowercase letters, digits, dashes and one dot", name, string(c))
		}
	}
	return nil
}

// ReservedNames are top-level names the file tools use for roots that are not
// checkouts: mirrored issues and pull requests, document stores, and (once
// PLAN-OBSERVABILITY lands) hydrated log windows.
//
// A Repository may not take one, because a repository name is also a
// top-level path segment and the collision would be resolved silently and
// wrongly. They are checked at apply and again when the store loads, so an
// object written before this rule existed fails loudly on upgrade rather than
// shadowing a root.
//
// This list is duplicated as files.ExtraRootNames, which is where the roots
// themselves are declared; a test asserts the two never drift. The duplication
// buys the file tools their independence from the object model.
var ReservedNames = []string{"mirrors", "docs", "documents", "logs"}

// Reserved reports whether a name is one the file tools have taken.
func Reserved(name string) bool {
	for _, r := range ReservedNames {
		if name == r {
			return true
		}
	}
	return false
}
