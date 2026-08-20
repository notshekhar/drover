package object

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// MinRefreshInterval is the shortest refresh a Repository may ask for. Below
// this we are hammering a git host to no purpose, so it is rejected at parse
// time rather than at the first tick.
const MinRefreshInterval = 30 * time.Second

// Interval is a refresh cadence: unset, never, or a duration.
//
// Three states, not two, because "not written down" and "written down as
// never" mean genuinely different things. Unset inherits the server default;
// never opts out of the ticker entirely.
type Interval struct {
	Set      bool
	Never    bool
	Duration time.Duration
}

// Resolve returns the effective interval given the server-wide default, and
// whether this object should be ticked at all.
func (i Interval) Resolve(def time.Duration) (time.Duration, bool) {
	switch {
	case !i.Set:
		return def, def > 0
	case i.Never:
		return 0, false
	default:
		return i.Duration, true
	}
}

func (i Interval) String() string {
	switch {
	case !i.Set:
		return "(default)"
	case i.Never:
		return "never"
	default:
		return i.Duration.String()
	}
}

// UnmarshalYAML accepts a Go duration string ("15m", "6h"), the word "never",
// or 0. A bare number without a unit is refused: "refreshInterval: 30" is
// ambiguous enough that guessing seconds would eventually surprise someone.
func (i *Interval) UnmarshalYAML(node *yaml.Node) error {
	// Read the scalar text rather than decoding into a string, so that an
	// unquoted 0 (which YAML types as an int) reaches the same code path as
	// the quoted forms instead of failing as a type error.
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("must be a duration like 15m, or never")
	}
	s := node.Value
	i.Set = true

	switch lower(s) {
	case "never", "off", "0":
		i.Never = true
		return nil
	}

	d, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("%q is not a duration; write it with a unit, like 30s, 15m or 6h (or never to disable)", s)
	}
	if d < 0 {
		return fmt.Errorf("%q is negative", s)
	}
	if d == 0 {
		i.Never = true
		return nil
	}
	if d < MinRefreshInterval {
		return fmt.Errorf("%s is below the minimum of %s", d, MinRefreshInterval)
	}
	i.Duration = d
	return nil
}

// MarshalYAML writes the interval back in the form it was given.
func (i Interval) MarshalYAML() (any, error) {
	switch {
	case !i.Set:
		return nil, nil
	case i.Never:
		return "never", nil
	default:
		return i.Duration.String(), nil
	}
}
