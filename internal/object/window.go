package object

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultMirrorWindow is how far back a mirror reaches when the document does
// not say. A year covers the discussion anyone is still arguing about; the
// rest is available by asking for it.
const DefaultMirrorWindow = 365 * 24 * time.Hour

// Window is a span reaching backwards from now: "90d", "6h", or "all".
//
// It is deliberately not an Interval. An Interval's "never" means do not do
// this on a schedule; a Window's "all" means do it without a bound. Reusing
// one type for both would make `since: never` read as "mirror everything",
// which is the opposite of what anyone would expect it to say.
type Window struct {
	Set bool
	All bool
	Dur time.Duration
}

// UnmarshalYAML accepts a Go duration, a day count like 90d, or "all".
func (w *Window) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("must be a span like 90d, 6h, or all")
	}
	w.Set = true
	s := strings.TrimSpace(node.Value)
	if lower(s) == "all" {
		w.All = true
		return nil
	}
	d, err := parseSpan(s)
	if err != nil {
		return err
	}
	w.Dur = d
	return nil
}

// MarshalYAML writes the window back in the form it was given.
func (w Window) MarshalYAML() (any, error) {
	switch {
	case !w.Set:
		return nil, nil
	case w.All:
		return "all", nil
	default:
		return w.String(), nil
	}
}

func (w Window) String() string {
	switch {
	case !w.Set:
		return "(default)"
	case w.All:
		return "all"
	case w.Dur%(24*time.Hour) == 0:
		return strconv.FormatInt(int64(w.Dur/(24*time.Hour)), 10) + "d"
	default:
		return w.Dur.String()
	}
}

// Since returns the timestamp this window starts at, and whether it has a
// start at all.
func (w Window) Since(now time.Time) (time.Time, bool) {
	switch {
	case w.Set && w.All:
		return time.Time{}, false
	case !w.Set:
		return now.Add(-DefaultMirrorWindow), true
	default:
		return now.Add(-w.Dur), true
	}
}

// parseSpan reads a Go duration, plus the day suffix Go does not have.
//
// Days matter here in a way they do not for a refresh interval: nobody
// mirrors "the last 2160 hours" of discussion, they mirror the last 90 days,
// and making them convert it by hand is a small cruelty.
func parseSpan(s string) (time.Duration, error) {
	if rest, ok := strings.CutSuffix(lower(s), "d"); ok {
		days, err := strconv.Atoi(rest)
		if err != nil || days < 0 {
			return 0, fmt.Errorf("%q is not a span; write it like 90d, 6h, or all", s)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%q is not a span; write it like 90d, 6h, or all", s)
	}
	if d < 0 {
		return 0, fmt.Errorf("%q is negative", s)
	}
	return d, nil
}
