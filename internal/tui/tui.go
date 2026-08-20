// Package tui draws the dashboard `drover serve` shows in a terminal.
//
// It is hand-rolled ANSI, no dependency. The screen is small and static in
// shape -- three tables and a footer -- so a full-screen framework would be a
// lot of surface for a picture that is redrawn once a second.
//
// Everything here reads state and paints it. The one thing it can change is
// asking the engine to reload, and that goes through a callback rather than
// happening in here, so the dashboard stays a view.
package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/notshekhar/drover/internal/api"
)

// ANSI. Kept in one place so the palette is easy to see and change.
const (
	reset     = "\x1b[0m"
	bold      = "\x1b[1m"
	dim       = "\x1b[2m"
	green     = "\x1b[32m"
	yellow    = "\x1b[33m"
	red       = "\x1b[31m"
	blue      = "\x1b[34m"
	cyan      = "\x1b[36m"
	magenta   = "\x1b[35m"
	altScreen = "\x1b[?1049h"
	mainScr   = "\x1b[?1049l"
	hideCur   = "\x1b[?25l"
	showCur   = "\x1b[?25h"
	clear     = "\x1b[2J\x1b[H"
	clearLine = "\x1b[K"
)

// Repo is one repository row.
type Repo struct {
	Name     string
	URL      string
	Branch   string
	Refresh  string
	Status   string
	Commit   string
	LastSync time.Time
	Error    string
}

// Request is one HTTPRequest row.
type Request struct {
	Name        string
	Method      string
	URL         string
	Environment string
	Offered     bool
}

// SQL is one SQLConnection row.
type SQL struct {
	Name     string
	Provider string
	ReadOnly bool
	Status   string
	Error    string
}

// Env is one Environment row.
type Env struct {
	Name      string
	Variables int
	Secrets   int
	Unset     int
}

// Model is everything the dashboard draws.
type Model struct {
	Version  string
	Listen   string
	MCPURL   string
	DataDir  string
	Started  time.Time
	Repos    []Repo
	Requests []Request
	SQLs     []SQL
	Envs     []Env

	// Notice is a transient line under the footer -- the result of a reload,
	// mostly. It is the only feedback a keypress gets.
	Notice     string
	NoticeKind string // "ok", "err", ""
	Reloading  bool
}

// Render paints the whole screen into a string.
//
// One string, written in a single syscall, because painting piecewise makes
// the redraw visibly tear on a slow terminal.
func Render(m Model, width int) string {
	if width < 60 {
		width = 60
	}
	var b strings.Builder

	b.WriteString(clear)
	header(&b, m, width)

	b.WriteString("\n")
	repoTable(&b, m.Repos, width)
	b.WriteString("\n")
	requestTable(&b, m.Requests, width)
	b.WriteString("\n")
	sqlTable(&b, m.SQLs, width)
	if len(m.Envs) > 0 {
		b.WriteString("\n")
		envLine(&b, m.Envs, width)
	}

	b.WriteString("\n")
	footer(&b, m, width)
	return b.String()
}

func header(b *strings.Builder, m Model, width int) {
	title := fmt.Sprintf(" drover %s ", m.Version)
	fmt.Fprintf(b, "%s%s%s%s\n", bold, cyan, title, reset)
	fmt.Fprintf(b, "%s%s%s\n", dim, strings.Repeat("─", width), reset)
	fmt.Fprintf(b, " %sengine%s  http://%s   %sMCP%s  http://%s/mcp\n",
		dim, reset, m.Listen, dim, reset, m.Listen)
	fmt.Fprintf(b, " %sdata%s    %s   %suptime%s  %s\n",
		dim, reset, m.DataDir, dim, reset, humanDuration(time.Since(m.Started)))
}

func section(b *strings.Builder, label string, n int, width int) {
	head := fmt.Sprintf(" %s%s%s %s(%d)%s ", bold, label, reset, dim, n, reset)
	// The visible length excludes the escape sequences.
	visible := len(label) + len(fmt.Sprintf(" (%d) ", n)) + 1
	rule := width - visible
	if rule < 0 {
		rule = 0
	}
	fmt.Fprintf(b, "%s%s%s%s\n", head, dim, strings.Repeat("─", rule), reset)
}

func repoTable(b *strings.Builder, repos []Repo, width int) {
	section(b, "REPOSITORIES", len(repos), width)
	if len(repos) == 0 {
		fmt.Fprintf(b, "   %snone yet — drover apply -f repo.yaml%s\n", dim, reset)
		return
	}

	fmt.Fprintf(b, "   %s%-16s %-9s %-10s %-9s %-9s %s%s\n",
		dim, "NAME", "BRANCH", "STATUS", "COMMIT", "REFRESH", "LAST SYNC", reset)
	for _, r := range repos {
		fmt.Fprintf(b, "   %-16s %-9s %s%s%s %-9s %-9s %s\n",
			truncate(r.Name, 16),
			truncate(r.Branch, 9),
			statusColor(r.Status), pad(truncate(r.Status, 10), 10), reset,
			shortCommit(r.Commit),
			truncate(r.Refresh, 9),
			relativeTime(r.LastSync),
		)
		// A failure is the reason someone opened this screen, so it gets its
		// own line rather than being clipped into a column.
		if r.Error != "" {
			fmt.Fprintf(b, "      %s%s%s\n", red, truncate(firstLine(r.Error), width-8), reset)
		}
	}
}

func requestTable(b *strings.Builder, reqs []Request, width int) {
	section(b, "HTTP REQUESTS", len(reqs), width)
	if len(reqs) == 0 {
		fmt.Fprintf(b, "   %snone%s\n", dim, reset)
		return
	}
	fmt.Fprintf(b, "   %s%-18s %-7s %-12s %s%s\n", dim, "NAME", "METHOD", "ENVIRONMENT", "URL", reset)
	for _, r := range reqs {
		offered := green + "●" + reset
		if !r.Offered {
			// Stored but never handed to an agent, which is worth seeing.
			offered = dim + "○" + reset
		}
		fmt.Fprintf(b, " %s %-18s %-7s %-12s %s%s%s\n",
			offered,
			truncate(r.Name, 18),
			truncate(r.Method, 7),
			truncate(r.Environment, 12),
			dim, truncate(r.URL, maxInt(10, width-48)), reset)
	}
}

func sqlTable(b *strings.Builder, sqls []SQL, width int) {
	section(b, "SQL CONNECTIONS", len(sqls), width)
	if len(sqls) == 0 {
		fmt.Fprintf(b, "   %snone%s\n", dim, reset)
		return
	}
	fmt.Fprintf(b, "   %s%-18s %-10s %-15s %s%s\n", dim, "NAME", "PROVIDER", "ACCESS", "STATUS", reset)
	for _, s := range sqls {
		offered := green + "●" + reset
		if s.Status != "ready" {
			offered = dim + "○" + reset
		}
		// Pad the plain text, then colour it: %-15s counts escape bytes, so
		// colouring first silently eats the padding and shifts the column.
		access := pad("read-only", 15)
		if !s.ReadOnly {
			access = yellow + pad("writes allowed", 15) + reset
		}
		fmt.Fprintf(b, " %s %-18s %-10s %s %s%s%s\n",
			offered,
			truncate(s.Name, 18),
			truncate(s.Provider, 10),
			access,
			statusColor(s.Status), s.Status, reset)
		if s.Error != "" {
			fmt.Fprintf(b, "      %s%s%s\n", red, truncate(firstLine(s.Error), width-8), reset)
		}
	}
}

func envLine(b *strings.Builder, envs []Env, width int) {
	section(b, "ENVIRONMENTS", len(envs), width)
	for _, e := range envs {
		secrets := fmt.Sprintf("%d secret(s)", e.Secrets)
		if e.Unset > 0 {
			// An unset secret is a call that will fail later; say so now.
			secrets = fmt.Sprintf("%s%d secret(s), %d unset%s", yellow, e.Secrets, e.Unset, reset)
		}
		fmt.Fprintf(b, "   %-18s %s%d variable(s)%s  %s\n",
			truncate(e.Name, 18), dim, e.Variables, reset, secrets)
	}
}

func footer(b *strings.Builder, m Model, width int) {
	fmt.Fprintf(b, "%s%s%s\n", dim, strings.Repeat("─", width), reset)

	if m.Reloading {
		fmt.Fprintf(b, " %sreloading…%s%s\n", yellow, reset, clearLine)
	} else if m.Notice != "" {
		color := green
		if m.NoticeKind == "err" {
			color = red
		}
		fmt.Fprintf(b, " %s%s%s%s\n", color, m.Notice, reset, clearLine)
	} else {
		fmt.Fprintf(b, "%s\n", clearLine)
	}

	fmt.Fprintf(b, " %s%sr%s reload configs   %s%ss%s sync repos   %s%sq%s quit%s\n",
		bold, magenta, reset, bold, magenta, reset, bold, magenta, reset, clearLine)
}

// --- helpers ---

func statusColor(status string) string {
	switch status {
	case "ready":
		return green
	case "failed":
		return red
	case "syncing":
		return yellow
	case "pending":
		return dim
	}
	return ""
}

func shortCommit(c string) string {
	if c == "" {
		return "-"
	}
	if len(c) > 8 {
		return c[:8]
	}
	return c
}

// relativeTime is "how long ago", because on this screen that is the question
// -- an absolute timestamp makes the reader do the subtraction.
func relativeTime(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < 5*time.Second:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd%dh", int(d.Hours()/24), int(d.Hours())%24)
	}
}

// pad right-pads to n visible columns. Every cell that carries colour must go
// through here rather than a %-Ns verb.
func pad(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(s))
}

func truncate(s string, n int) string {
	if n <= 1 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// FromState turns the engine's wire state into the model the screen draws.
//
// Both dashboards go through here: `drover serve` builds the state in
// process, `drover dash` fetches it over HTTP, and neither can drift from the
// other because the rendering only ever sees this one shape.
func FromState(s api.DashboardResponse, started time.Time) Model {
	m := Model{
		Version: s.Version,
		Listen:  s.Listen,
		DataDir: s.DataDir,
		Started: started,
	}
	if s.UptimeSec > 0 {
		// A remote engine's uptime is its own, not this process's.
		m.Started = time.Now().Add(-time.Duration(s.UptimeSec) * time.Second)
	}

	for _, r := range s.Repos {
		row := Repo{
			Name:    r.Name,
			URL:     r.URL,
			Branch:  r.Branch,
			Refresh: r.Refresh,
			Status:  r.Status,
			Commit:  r.Commit,
			Error:   r.Error,
		}
		if r.LastSync != "" {
			if t, err := time.Parse(time.RFC3339, r.LastSync); err == nil {
				row.LastSync = t
			}
		}
		m.Repos = append(m.Repos, row)
	}
	for _, r := range s.Requests {
		m.Requests = append(m.Requests, Request{
			Name: r.Name, Method: r.Method, URL: r.URL,
			Environment: r.Environment, Offered: r.Offered,
		})
	}
	for _, q := range s.SQLs {
		m.SQLs = append(m.SQLs, SQL{
			Name: q.Name, Provider: q.Provider, ReadOnly: q.ReadOnly,
			Status: q.Status, Error: q.Error,
		})
	}
	for _, e := range s.Envs {
		m.Envs = append(m.Envs, Env{
			Name: e.Name, Variables: e.Variables, Secrets: e.Secrets, Unset: e.Unset,
		})
	}
	return m
}
