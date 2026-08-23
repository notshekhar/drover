package tui

import (
	"fmt"
	"strings"
	"time"
)

// RenderSummary is what `drover serve` shows: counts and health, not tables.
//
// serve is a thing you start and leave running in a corner of the screen. The
// question it has to answer at a glance is "is it up and is anything broken",
// not "what is in it" -- that is what `drover dash` is for.
func RenderSummary(m Model, width int) string {
	if width < 46 {
		width = 46
	}
	if width > 78 {
		width = 78
	}
	var b strings.Builder

	b.WriteString(clear)

	fmt.Fprintf(&b, "\n  %s%sdrover%s %s%s%s\n\n", bold, cyan, reset, dim, m.Version, reset)

	fmt.Fprintf(&b, "  %sengine%s  http://%s\n", dim, reset, m.Listen)
	fmt.Fprintf(&b, "  %sMCP%s     http://%s/mcp\n", dim, reset, m.Listen)
	fmt.Fprintf(&b, "  %sdash%s    http://%s/dashboard\n", dim, reset, m.Listen)
	fmt.Fprintf(&b, "  %sdata%s    %s\n", dim, reset, truncate(homeShort(m.DataDir), width-12))
	fmt.Fprintf(&b, "  %suptime%s  %s\n\n", dim, reset, humanDuration(time.Since(m.Started)))

	fmt.Fprintf(&b, "  %s%s%s\n", dim, strings.Repeat("─", width-4), reset)

	summaryRow(&b, "repositories", countRepos(m))
	summaryRow(&b, "http requests", countRequests(m))
	summaryRow(&b, "databases", countSQL(m))
	summaryRow(&b, "environments", countEnvs(m))

	fmt.Fprintf(&b, "  %s%s%s\n\n", dim, strings.Repeat("─", width-4), reset)

	// Failures are the only detail worth interrupting a summary for.
	if failing := failures(m); len(failing) > 0 {
		for _, f := range failing {
			fmt.Fprintf(&b, "  %s✗ %s%s\n", red, f, reset)
		}
		b.WriteString("\n")
	}

	if m.Notice != "" {
		color := green
		if m.NoticeKind == "err" {
			color = red
		}
		fmt.Fprintf(&b, "  %s%s%s%s\n\n", color, truncate(m.Notice, width-4), reset, clearLine)
	}

	if m.AutoReload {
		// Only claim this when the engine really is watching its files.
		fmt.Fprintf(&b, "  %s%s%s\n", dim,
			truncate("edits in "+homeShort(m.DataDir)+" apply automatically", width-4), reset)
	}
	if m.AutoReload {
		fmt.Fprintf(&b, "  %s%sd%s details   %s%ss%s sync   %s%sq%s quit%s\n",
			bold, magenta, reset, bold, magenta, reset, bold, magenta, reset, clearLine)
		return b.String()
	}
	fmt.Fprintf(&b, "  %s%sd%s details   %s%sr%s reload   %s%ss%s sync   %s%sq%s quit%s\n",
		bold, magenta, reset, bold, magenta, reset, bold, magenta, reset, bold, magenta, reset, clearLine)
	return b.String()
}

// count is a tally with a health breakdown.
type count struct {
	total  int
	ok     int
	failed int
	// note is an extra clause, like "3 offered to agents".
	note string
}

func summaryRow(b *strings.Builder, label string, c count) {
	if c.total == 0 {
		fmt.Fprintf(b, "  %-15s %s—%s\n", label, dim, reset)
		return
	}

	value := fmt.Sprintf("%d", c.total)
	switch {
	case c.failed > 0:
		value += fmt.Sprintf("   %s%d failing%s", red, c.failed, reset)
	case c.ok > 0 && c.ok == c.total:
		value += fmt.Sprintf("   %s✓%s", green, reset)
	}
	if c.note != "" {
		value += fmt.Sprintf("   %s%s%s", dim, c.note, reset)
	}
	fmt.Fprintf(b, "  %-15s %s\n", label, value)
}

func countRepos(m Model) count {
	c := count{total: len(m.Repos)}
	for _, r := range m.Repos {
		switch r.Status {
		case "ready":
			c.ok++
		case "failed":
			c.failed++
		}
	}
	return c
}

func countRequests(m Model) count {
	c := count{total: len(m.Requests)}
	offered := 0
	for _, r := range m.Requests {
		if r.Offered {
			offered++
		}
	}
	c.ok = c.total
	if offered != c.total {
		// The gap matters: a stored POST is never handed to an agent.
		c.note = fmt.Sprintf("%d offered", offered)
	}
	return c
}

func countSQL(m Model) count {
	c := count{total: len(m.SQLs)}
	for _, s := range m.SQLs {
		if s.Status == "ready" {
			c.ok++
		} else {
			c.failed++
		}
	}
	return c
}

func countEnvs(m Model) count {
	c := count{total: len(m.Envs), ok: len(m.Envs)}
	unset := 0
	for _, e := range m.Envs {
		unset += e.Unset
	}
	if unset > 0 {
		// An unset secret is a call that will fail later.
		c.note = fmt.Sprintf("%d secret(s) unset", unset)
	}
	return c
}

// failures names what is broken, one short line each.
func failures(m Model) []string {
	var out []string
	for _, r := range m.Repos {
		if r.Status == "failed" {
			out = append(out, fmt.Sprintf("%s: %s", r.Name, firstLine(r.Error)))
		}
	}
	for _, s := range m.SQLs {
		if s.Status != "ready" {
			reason := firstLine(s.Error)
			if reason == "" {
				reason = s.Status
			}
			out = append(out, fmt.Sprintf("%s: %s", s.Name, reason))
		}
	}
	for i, f := range out {
		out[i] = truncate(f, 72)
	}
	return out
}
