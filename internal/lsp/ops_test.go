package lsp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/notshekhar/drover/internal/files"
)

// goFixture builds a data directory holding one checkout: a small Go module
// where a function is defined in one file and called from another, which is
// the shape every navigation question has.
func goFixture(t *testing.T) *Manager {
	t.Helper()
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".drover", "servers", "bin", "gopls")); err != nil {
		t.Skip("gopls is not installed")
	}

	data := t.TempDir()
	repo := filepath.Join(data, "repos", "api")
	if err := os.MkdirAll(filepath.Join(repo, "internal"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/api\n\ngo 1.22\n")
	write(filepath.Join("internal", "db.go"), `package internal

// Connect opens the database.
func Connect(dsn string) error {
	return nil
}

// Close shuts it again.
func Close() {}
`)
	write("main.go", `package main

import "example.com/api/internal"

func main() {
	boot()
}

func boot() {
	_ = internal.Connect("dsn")
	internal.Close()
}
`)

	m := NewManager(files.New(data), NewAcquirer(filepath.Join(os.Getenv("HOME"), ".drover", "servers")))
	t.Cleanup(m.Close)
	return m
}

func ask(t *testing.T, m *Manager, req Request) *Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := m.Run(ctx, req)
	if err != nil {
		t.Fatalf("%s: %v", req.Operation, err)
	}
	return res
}

// TestSymbolInsteadOfColumn is the argument that matters most: an agent
// arrives from grep with a name, never a column.
func TestSymbolInsteadOfColumn(t *testing.T) {
	m := goFixture(t)
	res := ask(t, m, Request{Operation: "definition", Path: "api/main.go", Symbol: "Connect"})
	if len(res.Refs) != 1 {
		t.Fatalf("refs = %+v (note %q)", res.Refs, res.Note)
	}
	got := res.Refs[0]
	if got.Path != "api/internal/db.go" || got.Line != 4 {
		t.Errorf("definition -> %s:%d, want api/internal/db.go:4", got.Path, got.Line)
	}
	if !strings.Contains(got.Text, "func Connect") {
		t.Errorf("the source line should come back with it, got %q", got.Text)
	}
	t.Logf("definition -> %s:%d  %s", got.Path, got.Line, got.Text)
}

func TestDefinitionByLineAndCharacter(t *testing.T) {
	m := goFixture(t)
	// main.go line 10 is `_ = internal.Connect("dsn")`; column 15 is inside
	// Connect. Both coordinates are 1-based here and 0-based on the wire.
	res := ask(t, m, Request{Operation: "definition", Path: "api/main.go", Line: 10, Character: 15})
	if len(res.Refs) != 1 || res.Refs[0].Line != 4 {
		t.Fatalf("refs = %+v", res.Refs)
	}
	if res.Position != "api/main.go:10:15" {
		t.Errorf("position echoed as %q", res.Position)
	}
}

func TestReferencesFindsTheCallSite(t *testing.T) {
	m := goFixture(t)
	res := ask(t, m, Request{Operation: "references", Path: "api/internal/db.go", Symbol: "Connect"})
	if len(res.Refs) == 0 {
		t.Fatalf("no references (note %q)", res.Note)
	}
	var found bool
	for _, r := range res.Refs {
		if r.Path == "api/main.go" {
			found = true
		}
	}
	if !found {
		t.Errorf("the call in main.go is missing: %+v", res.Refs)
	}
}

func TestHover(t *testing.T) {
	m := goFixture(t)
	res := ask(t, m, Request{Operation: "hover", Path: "api/main.go", Symbol: "Connect"})
	if !strings.Contains(res.Hover, "Connect") {
		t.Errorf("hover = %q", res.Hover)
	}
	if !strings.Contains(res.Hover, "opens the database") {
		t.Errorf("the doc comment should be in there: %q", res.Hover)
	}
}

func TestDocumentSymbols(t *testing.T) {
	m := goFixture(t)
	res := ask(t, m, Request{Operation: "document_symbols", Path: "api/internal/db.go"})
	names := map[string]string{}
	for _, s := range res.Symbols {
		names[s.Name] = s.Kind
	}
	if names["Connect"] != "function" || names["Close"] != "function" {
		t.Errorf("outline = %+v", res.Symbols)
	}
}

func TestWorkspaceSymbols(t *testing.T) {
	m := goFixture(t)
	res := ask(t, m, Request{Operation: "workspace_symbols", Path: "api/main.go", Query: "Connect"})
	if len(res.Symbols) == 0 {
		t.Fatal("nothing found")
	}
	t.Logf("workspace_symbols -> %s (%s) at %s:%d", res.Symbols[0].Name, res.Symbols[0].Kind, res.Symbols[0].Path, res.Symbols[0].Line)
}

func TestIncomingCalls(t *testing.T) {
	m := goFixture(t)
	res := ask(t, m, Request{Operation: "incoming_calls", Path: "api/internal/db.go", Symbol: "Connect"})
	if len(res.Calls) == 0 {
		t.Fatalf("no callers (note %q)", res.Note)
	}
	if res.Calls[0].Name != "boot" {
		t.Errorf("Connect is called by boot, got %+v", res.Calls)
	}
	t.Logf("incoming -> %s at %s:%d, call sites %v", res.Calls[0].Name, res.Calls[0].Path, res.Calls[0].Line, res.Calls[0].Sites)
}

func TestOutgoingCalls(t *testing.T) {
	m := goFixture(t)
	res := ask(t, m, Request{Operation: "outgoing_calls", Path: "api/main.go", Symbol: "boot"})
	var names []string
	for _, c := range res.Calls {
		names = append(names, c.Name)
	}
	if len(names) == 0 {
		t.Fatalf("boot calls Connect and Close, got nothing (note %q)", res.Note)
	}
	t.Logf("outgoing -> %v", names)
}

func TestDiagnosticsReportsARealError(t *testing.T) {
	m := goFixture(t)
	broken := filepath.Join(m.Files.Dir, "api", "broken.go")
	if err := os.WriteFile(broken, []byte("package main\n\nfunc bad() int { return \"not an int\" }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := ask(t, m, Request{Operation: "diagnostics", Path: "api/broken.go"})
	if len(res.Problems) == 0 {
		t.Fatalf("a type error should be reported (note %q, server %q)", res.Note, res.ServerState)
	}
	t.Logf("diagnostics -> %s at line %d: %s", res.Problems[0].Severity, res.Problems[0].Line, res.Problems[0].Message)
	if res.Problems[0].Severity != "error" {
		t.Errorf("severity = %s", res.Problems[0].Severity)
	}
}

func TestServersOperationExplainsWhatIsMissing(t *testing.T) {
	m := goFixture(t)
	res := ask(t, m, Request{Operation: "servers"})
	if len(res.Servers) < 3 {
		t.Fatalf("all three languages should be accounted for: %+v", res.Servers)
	}
	for _, s := range res.Servers {
		t.Logf("%-10s %-12s %s", s.Key, s.State, s.Detail)
		if s.State == "" {
			t.Errorf("%s has no state", s.Key)
		}
	}
}

func TestRefusals(t *testing.T) {
	m := goFixture(t)
	for _, tc := range []struct {
		name, want string
		req        Request
	}{
		{"unknown operation", "unknown operation", Request{Operation: "rename", Path: "api/main.go"}},
		{"no position", "needs a position", Request{Operation: "definition", Path: "api/main.go"}},
		{"symbol not there", "does not appear", Request{Operation: "definition", Path: "api/main.go", Symbol: "Nonexistent"}},
		{"path escapes", "walks upwards", Request{Operation: "definition", Path: "../../etc/passwd", Symbol: "x"}},
		{"unknown language", "no language server", Request{Operation: "definition", Path: "api/go.mod", Symbol: "module"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := m.Run(context.Background(), tc.req)
			if err == nil {
				t.Fatal("want an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestRestartDropsServersForARepository is the drover-specific one: reconcile
// rewrites the working tree with `git reset --hard`, and a server that already
// parsed the old tree would go on answering about a tree that is gone.
func TestRestartDropsServersForARepository(t *testing.T) {
	m := goFixture(t)
	ctx := context.Background()

	target, err := m.Resolve("api/main.go")
	if err != nil {
		t.Fatal(err)
	}
	first, _, err := m.ClientFor(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Alive() {
		t.Fatal("the server should be running")
	}

	m.Restart("api")

	deadline := time.Now().Add(10 * time.Second)
	for first.Alive() && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if first.Alive() {
		t.Fatal("the old server is still running after a restart")
	}

	second, _, err := m.ClientFor(ctx, target)
	if err != nil {
		t.Fatalf("a fresh server should start on the next question: %v", err)
	}
	if second == first {
		t.Error("the same client came back")
	}
	if !second.Alive() {
		t.Error("the replacement is not running")
	}
}

// TestIdleServersAreReaped: a warm server is the point of a daemon, but one
// nobody has asked anything is a gigabyte with no owner.
func TestIdleServersAreReaped(t *testing.T) {
	m := goFixture(t)
	m.IdleTTL = time.Nanosecond

	target, err := m.Resolve("api/main.go")
	if err != nil {
		t.Fatal(err)
	}
	client, _, err := m.ClientFor(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}

	m.reapIdle()
	if client.Alive() {
		t.Error("an idle server should have been reaped")
	}
}
