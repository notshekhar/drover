package mcp

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/notshekhar/drover/internal/api"
	"github.com/notshekhar/drover/internal/object"
)

// The inventory is what a static instructions string cannot say: which
// repositories this engine actually holds, which environments exist, how many
// requests are callable, which databases are reachable.
//
// Without it an agent's first move is always `ls` with no path, to learn three
// names it could have been told. Worse, a model that skips that call guesses
// paths instead, and a guessed repository name fails in a way that looks like
// the file is missing.
const (
	// Caps, because orientation is the point and a listing is not. Fifty
	// repositories must not produce a fifty-line preamble that pushes the
	// how-to-use-the-tools half out of the model's attention.
	maxInventoryRepos = 12
	maxInventoryEnvs  = 16
	maxInventorySQL   = 12

	// How long a rendered inventory is reused. Initialize happens once per
	// connection, but a stdio bridge reconnects freely and the HTTP endpoint
	// serves the resource form on demand.
	inventoryTTL = 10 * time.Second
)

// inventory renders what this engine holds, or "" when it holds nothing and
// when the engine cannot be reached.
//
// Returning "" on failure is deliberate. `drover mcp` bridges to an engine
// over HTTP and may well be started before `drover serve`; an initialize that
// failed because the inventory could not be fetched would be a far worse
// outcome than one with a vague preamble.
func (s *Server) inventory() string {
	s.invMu.Lock()
	defer s.invMu.Unlock()

	if s.invCache != "" && time.Since(s.invAt) < inventoryTTL {
		return s.invCache
	}

	rendered := s.renderInventory()
	// Only a successful render refreshes the clock. An engine that was down
	// returns "" without pinning that emptiness for the next ten seconds.
	if rendered != "" {
		s.invCache = rendered
		s.invAt = time.Now()
	}
	return rendered
}

func (s *Server) renderInventory() string {
	var sections []string

	if sec := s.repositorySection(); sec != "" {
		sections = append(sections, sec)
	}
	if sec := s.environmentSection(); sec != "" {
		sections = append(sections, sec)
	}
	if sec := s.requestSection(); sec != "" {
		sections = append(sections, sec)
	}
	if sec := s.databaseSection(); sec != "" {
		sections = append(sections, sec)
	}

	if len(sections) == 0 {
		return ""
	}
	return "This engine currently holds:\n\n" + strings.Join(sections, "\n")
}

func (s *Server) repositorySection() string {
	items, err := s.Backend.List(object.KindRepository)
	if err != nil || len(items) == 0 {
		return ""
	}
	sortByName(items)

	shown := items
	if len(shown) > maxInventoryRepos {
		shown = shown[:maxInventoryRepos]
	}

	width := 0
	for _, v := range shown {
		if len(v.Name) > width {
			width = len(v.Name)
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "REPOSITORIES (%d)\n", len(items))
	for _, v := range shown {
		fmt.Fprintf(&b, "  %-*s  %s\n", width, v.Name, repositoryDetail(v))
	}
	if rest := len(items) - len(shown); rest > 0 {
		fmt.Fprintf(&b, "  ... and %d more -- call ls with no path for all of them\n", rest)
	}
	return b.String()
}

// repositoryDetail is the one line after a repository's name.
//
// A failed repository says so, with the reason clipped to one line. An agent
// told that a checkout failed stops trying to read from it; an agent told only
// that the name exists reads a missing-file error instead and concludes the
// code is not there.
func repositoryDetail(v api.ObjectView) string {
	parts := []string{}
	if v.Branch != "" {
		parts = append(parts, v.Branch)
	}
	if len(v.Commit) >= 8 {
		parts = append(parts, v.Commit[:8])
	}

	switch v.Status {
	case "ready":
		parts = append(parts, "synced "+relativeTime(v.LastSync))
	case "":
		parts = append(parts, "not synced yet")
	default:
		detail := v.Status
		if v.Error != "" {
			detail += ": " + firstLine(v.Error)
		}
		parts = append(parts, clip(detail, 100))
	}
	return strings.Join(parts, "  ")
}

func (s *Server) environmentSection() string {
	items, err := s.Backend.List(object.KindEnvironment)
	if err != nil || len(items) == 0 {
		return ""
	}
	sortByName(items)

	names := make([]string, 0, len(items))
	for _, v := range items {
		names = append(names, v.Name)
	}
	suffix := ""
	if len(names) > maxInventoryEnvs {
		suffix = fmt.Sprintf(", and %d more", len(names)-maxInventoryEnvs)
		names = names[:maxInventoryEnvs]
	}
	// Names only. An environment's variables and secrets are never rendered
	// here: the standing rule is that an agent cannot reach a secret by
	// asking, and a preamble that printed one would be a way of asking.
	return fmt.Sprintf("ENVIRONMENTS (%d)\n  %s%s\n", len(items), strings.Join(names, ", "), suffix)
}

func (s *Server) requestSection() string {
	items, err := s.Backend.List(object.KindHTTPRequest)
	if err != nil || len(items) == 0 {
		return ""
	}
	callable := 0
	for _, v := range items {
		if v.Safe {
			callable++
		}
	}

	// Individual requests are not listed: api_list is the catalogue, and
	// duplicating it here would be the mistake that made twenty requests into
	// twenty tools, in a new place.
	switch {
	case callable == 0:
		return fmt.Sprintf("HTTP REQUESTS (%d)\n  none are callable -- every one is a non-GET request, which is never offered.\n", len(items))
	case callable == len(items):
		return fmt.Sprintf("HTTP REQUESTS (%d)\n  all callable with api_call. Use api_list to find one.\n", len(items))
	default:
		return fmt.Sprintf("HTTP REQUESTS (%d)\n  %d callable with api_call; the rest are non-GET and are not offered. Use api_list to find one.\n", len(items), callable)
	}
}

func (s *Server) databaseSection() string {
	items, err := s.Backend.List(object.KindSQLConnection)
	if err != nil || len(items) == 0 {
		return ""
	}
	sortByName(items)

	var ready []string
	unhealthy := 0
	for _, v := range items {
		// Same health gate the sql_query tool applies, so the preamble cannot
		// advertise a connection the tool refuses to serve.
		if v.Status == "ready" {
			ready = append(ready, fmt.Sprintf("%s (%s)", v.Name, v.Provider))
			continue
		}
		unhealthy++
	}

	var b strings.Builder
	fmt.Fprintf(&b, "DATABASES (%d)\n", len(items))
	switch {
	case len(ready) == 0:
		b.WriteString("  none are queryable -- no health check has passed, so sql_query is not offered.\n")
	default:
		shown := ready
		if len(shown) > maxInventorySQL {
			shown = shown[:maxInventorySQL]
		}
		fmt.Fprintf(&b, "  %s", strings.Join(shown, ", "))
		if rest := len(ready) - len(shown); rest > 0 {
			fmt.Fprintf(&b, ", and %d more", rest)
		}
		b.WriteString("\n")
		if unhealthy > 0 {
			fmt.Fprintf(&b, "  %d more stored but not queryable: no health check has passed.\n", unhealthy)
		}
	}
	return b.String()
}

func sortByName(items []api.ObjectView) {
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
}

// relativeTime turns the RFC3339 stamp an ObjectView carries into something a
// sentence can hold. The dashboard has its own copy of this idea; sharing one
// would mean this package importing the terminal UI, which is the wrong way
// round for a dependency.
func relativeTime(stamp string) string {
	if stamp == "" {
		return "never"
	}
	t, err := time.Parse(time.RFC3339, stamp)
	if err != nil || t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func clip(s string, max int) string {
	// Runes, not bytes: a clone error can carry a non-ASCII path, and cutting
	// one mid-character puts a replacement char in the model's context.
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-3]) + "..."
}
