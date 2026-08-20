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
// The pattern is ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ -- lowercase alphanumerics
// and dashes, not starting or ending with a dash, at most 63 characters.
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
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			continue
		case c == '-':
			if i == 0 || i == len(name)-1 {
				return fmt.Errorf("%q may not start or end with a dash", name)
			}
			continue
		case c >= 'A' && c <= 'Z':
			return fmt.Errorf("%q must be lowercase (names become directory names, and a case-insensitive filesystem would treat %q and its lowercase form as one)", name, name)
		default:
			return fmt.Errorf("%q contains %q; names may only use lowercase letters, digits and dashes", name, string(c))
		}
	}
	return nil
}
