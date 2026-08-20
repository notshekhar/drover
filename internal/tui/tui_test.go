package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/notshekhar/drover/internal/api"
)

// stripANSI removes escape sequences, so a test can assert on what a reader
// actually sees rather than on the bytes.
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			for i < len(s) && !isFinalByte(s[i]) {
				i++
			}
			i++
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func isFinalByte(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func sample() Model {
	return Model{
		Version: "1.2.3", Listen: "127.0.0.1:7432", DataDir: "/home/x/.drover",
		Started: time.Now().Add(-90 * time.Minute),
		Repos: []Repo{
			{Name: "api", Branch: "main", Refresh: "15m", Status: "ready", Commit: "abcdef1234", LastSync: time.Now().Add(-2 * time.Minute)},
			{Name: "broken", Branch: "main", Refresh: "1h", Status: "failed", Error: "remote: Repository not found."},
		},
		Requests: []Request{
			{Name: "get-user", Method: "GET", URL: "{{baseUrl}}/u/{id}", Environment: "prod", Offered: true},
			{Name: "make-thing", Method: "POST", URL: "{{baseUrl}}/things", Environment: "prod", Offered: false},
		},
		SQLs: []SQL{
			{Name: "analytics", Provider: "postgres", ReadOnly: true, Status: "ready"},
			{Name: "scratch", Provider: "mysql", ReadOnly: false, Status: "ready"},
		},
		Envs: []Env{{Name: "prod", Variables: 2, Secrets: 1}, {Name: "local", Variables: 1, Secrets: 1, Unset: 1}},
	}
}

func TestRenderShowsEverything(t *testing.T) {
	out := stripANSI(Render(sample(), 100))

	for _, want := range []string{
		"drover 1.2.3",
		"127.0.0.1:7432/mcp", // the address someone needs to point an agent at
		"REPOSITORIES 2",
		"HTTP REQUESTS 2",
		"SQL CONNECTIONS 2",
		"ENVIRONMENTS 2",
		"api", "broken", "get-user", "analytics", "prod",
		"d summary", "r reload", "s sync", "q quit",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the screen is missing %q:\n%s", want, out)
		}
	}
}

// A failure is the reason someone looks at this screen, so it gets its own
// line rather than being clipped into a column.
func TestRenderShowsErrorsInFull(t *testing.T) {
	out := stripANSI(Render(sample(), 100))
	if !strings.Contains(out, "remote: Repository not found.") {
		t.Errorf("the repository error is not shown:\n%s", out)
	}
}

// The dot says whether an agent is actually offered the thing. A stored POST
// is not, and that difference is the whole point of the column.
func TestRenderMarksWhatIsOffered(t *testing.T) {
	out := stripANSI(Render(sample(), 100))
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "get-user") && !strings.HasPrefix(line, " ●") {
			t.Errorf("an offered GET is not marked: %q", line)
		}
		if strings.Contains(line, "make-thing") && !strings.HasPrefix(line, " ○") {
			t.Errorf("a stored POST is marked as offered: %q", line)
		}
	}
}

// Columns must line up even though some cells carry colour, which is exactly
// what a %-Ns verb gets wrong once escape bytes are in the string.
func TestColumnsAlignWithColour(t *testing.T) {
	out := stripANSI(Render(sample(), 100))

	var statusCol []int
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "read-only") || strings.Contains(line, "writes allowed") {
			statusCol = append(statusCol, strings.Index(line, "ready"))
		}
	}
	if len(statusCol) != 2 {
		t.Fatalf("expected two sql rows, got %d", len(statusCol))
	}
	if statusCol[0] != statusCol[1] {
		t.Errorf("the STATUS column shifts between rows: %d vs %d", statusCol[0], statusCol[1])
	}
}

func TestRelativeTime(t *testing.T) {
	cases := map[time.Duration]string{
		2 * time.Second:  "just now",
		30 * time.Second: "30s ago",
		5 * time.Minute:  "5m ago",
		3 * time.Hour:    "3h ago",
		50 * time.Hour:   "2d ago",
	}
	for d, want := range cases {
		if got := relativeTime(time.Now().Add(-d)); got != want {
			t.Errorf("relativeTime(-%v) = %q, want %q", d, got, want)
		}
	}
	if got := relativeTime(time.Time{}); got != "never" {
		t.Errorf("a zero time renders as %q, want never", got)
	}
}

func TestEmptyStateGuidesTheReader(t *testing.T) {
	out := stripANSI(Render(Model{Version: "1.0.0", Listen: "x:1", Started: time.Now()}, 100))
	if !strings.Contains(out, "drover apply -f repo.yaml") {
		t.Errorf("an empty dashboard should say what to do next:\n%s", out)
	}
}

// The remote dashboard renders from the same wire type as the in-process one,
// so a field dropped here would silently blank a column in `drover dash`.
func TestFromState(t *testing.T) {
	synced := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	m := FromState(api.DashboardResponse{
		Version: "9.9.9", Listen: "1.2.3.4:9", DataDir: "/d", UptimeSec: 120,
		Repos:    []api.DashRepo{{Name: "api", Status: "ready", LastSync: synced, Commit: "deadbeef"}},
		Requests: []api.DashRequest{{Name: "r", Method: "GET", Offered: true}},
		SQLs:     []api.DashSQL{{Name: "db", Provider: "postgres", ReadOnly: true, Status: "ready"}},
		Envs:     []api.DashEnv{{Name: "prod", Variables: 1, Secrets: 2, Unset: 1}},
	}, time.Now())

	if m.Version != "9.9.9" || m.Listen != "1.2.3.4:9" || m.DataDir != "/d" {
		t.Errorf("header fields lost: %+v", m)
	}
	if len(m.Repos) != 1 || m.Repos[0].Commit != "deadbeef" || m.Repos[0].LastSync.IsZero() {
		t.Errorf("repo row lost: %+v", m.Repos)
	}
	if len(m.Requests) != 1 || !m.Requests[0].Offered {
		t.Errorf("request row lost: %+v", m.Requests)
	}
	if len(m.SQLs) != 1 || !m.SQLs[0].ReadOnly {
		t.Errorf("sql row lost: %+v", m.SQLs)
	}
	if len(m.Envs) != 1 || m.Envs[0].Unset != 1 {
		t.Errorf("env row lost: %+v", m.Envs)
	}
	// A remote engine's uptime is its own, not this process's.
	if d := time.Since(m.Started); d < 100*time.Second || d > 140*time.Second {
		t.Errorf("uptime came out as %v, want about 120s", d)
	}
}

// A narrow terminal must not produce a broken picture.
func TestNarrowTerminal(t *testing.T) {
	for _, w := range []int{20, 40, 60, 80} {
		out := Render(sample(), w)
		if !strings.Contains(stripANSI(out), "REPOSITORIES") {
			t.Errorf("width %d lost the sections", w)
		}
	}
}

// serve shows a summary, not tables: the question it answers at a glance is
// "is it up and is anything broken".
func TestSummaryIsCountsNotTables(t *testing.T) {
	out := stripANSI(RenderSummary(sample(), 78))

	for _, want := range []string{
		"drover 1.2.3",
		"127.0.0.1:7432/mcp",
		"repositories", "http requests", "databases", "environments",
		"d details", "s sync", "q quit",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the summary is missing %q:\n%s", want, out)
		}
	}

	// The tables belong to the detail view.
	for _, unwanted := range []string{"BRANCH", "COMMIT", "LAST SYNC", "PROVIDER"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("the summary printed a table column %q:\n%s", unwanted, out)
		}
	}
}

// A failure is the one detail worth interrupting a summary for.
func TestSummaryNamesFailures(t *testing.T) {
	out := stripANSI(RenderSummary(sample(), 78))
	if !strings.Contains(out, "broken") {
		t.Errorf("the summary does not name the failing repository:\n%s", out)
	}
	if !strings.Contains(out, "1 failing") {
		t.Errorf("the summary does not count the failure:\n%s", out)
	}
}

// A stored POST is not offered to agents, and the gap between "configured"
// and "offered" is worth showing.
func TestSummaryShowsWhatIsOffered(t *testing.T) {
	out := stripANSI(RenderSummary(sample(), 78))
	if !strings.Contains(out, "1 offered") {
		t.Errorf("the summary does not distinguish stored from offered:\n%s", out)
	}
}

func TestSummaryWithNothingApplied(t *testing.T) {
	out := stripANSI(RenderSummary(Model{Version: "1.0.0", Listen: "x:1", Started: time.Now()}, 78))
	for _, want := range []string{"repositories", "—"} {
		if !strings.Contains(out, want) {
			t.Errorf("an empty summary is missing %q:\n%s", want, out)
		}
	}
}

// When the engine watches its own files there is nothing to press, and the
// screen should not offer a key that does nothing.
func TestAutoReloadHidesTheReloadKey(t *testing.T) {
	m := sample()
	m.AutoReload = true

	for name, out := range map[string]string{
		"summary": stripANSI(RenderSummary(m, 78)),
		"detail":  stripANSI(Render(m, 100)),
	} {
		if strings.Contains(out, "r reload") {
			t.Errorf("the %s screen still offers a reload key:\n%s", name, out)
		}
		if !strings.Contains(out, "automatically") {
			t.Errorf("the %s screen does not say edits apply themselves:\n%s", name, out)
		}
	}
}
