package lsp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSpikeGopls is the phase-0 proof: a real server, a real handshake, and a
// definition that lands where it should.
func TestSpikeGopls(t *testing.T) {
	bin := filepath.Join(os.Getenv("HOME"), ".drover", "servers", "bin", "gopls")
	if _, err := os.Stat(bin); err != nil {
		t.Skip("no gopls")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	c, err := Start(ctx, "go", root, bin, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	start := time.Now()
	if err := c.Initialize(ctx, nil); err != nil {
		t.Fatalf("initialize: %v\nstderr: %s", err, c.Stderr())
	}
	t.Logf("handshake in %s", time.Since(start))
	for _, capability := range []string{
		"definitionProvider", "referencesProvider", "hoverProvider",
		"documentSymbolProvider", "workspaceSymbolProvider", "implementationProvider",
		"callHierarchyProvider", "diagnosticProvider",
	} {
		t.Logf("  %-24s %v", capability, c.Supports(capability))
	}

	file := filepath.Join(root, "internal", "git", "ops.go")
	if err := c.OpenDocument(file); err != nil {
		t.Fatal(err)
	}

	// Find `parseCommits(` where it is CALLED, and ask where it is defined.
	line, col := find(t, file, "res.Commits = parseCommits(out)", "parseCommits")

	var raw json.RawMessage
	if err := c.Request(ctx, "textDocument/definition", map[string]any{
		"textDocument": map[string]any{"uri": PathToURI(file)},
		"position":     Position{Line: line, Character: col},
	}, &raw); err != nil {
		t.Fatalf("definition: %v\nstderr: %s", err, c.Stderr())
	}
	locations := ToLocations(raw)
	if len(locations) == 0 {
		t.Fatalf("no definition found (raw %s)\nstderr: %s", raw, c.Stderr())
	}
	got := URIToPath(locations[0].URI)
	t.Logf("definition -> %s:%d", got, locations[0].Range.Start.Line+1)
	if filepath.Base(got) != "ops.go" {
		t.Errorf("want ops.go, got %s", got)
	}

	// workspace/symbol is the operation that most needs the project loaded.
	var symbols []SymbolInformation
	if err := c.Request(ctx, "workspace/symbol", map[string]any{"query": "parseNameStatus"}, &symbols); err != nil {
		t.Fatalf("workspace/symbol: %v", err)
	}
	if len(symbols) == 0 {
		t.Fatal("workspace/symbol found nothing")
	}
	t.Logf("workspace/symbol -> %s (%s) in %s", symbols[0].Name, KindName(symbols[0].Kind), filepath.Base(URIToPath(symbols[0].Location.URI)))

	// And references, which is the operation an agent actually wants.
	var refs []Location
	if err := c.Request(ctx, "textDocument/references", map[string]any{
		"textDocument": map[string]any{"uri": PathToURI(file)},
		"position":     Position{Line: line, Character: col},
		"context":      map[string]any{"includeDeclaration": false},
	}, &refs); err != nil {
		t.Fatalf("references: %v", err)
	}
	t.Logf("references -> %d call sites", len(refs))
	if len(refs) < 2 {
		t.Errorf("parseCommits is called more than once; got %d", len(refs))
	}
}

// find returns the 0-based line and character of needle within the first line
// containing anchor.
func find(t *testing.T, path, anchor, needle string) (int, int) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for i, line := range strings.Split(string(data), "\n") {
		if !strings.Contains(line, anchor) {
			continue
		}
		col := strings.Index(line, needle)
		if col < 0 {
			continue
		}
		return i, col
	}
	t.Fatalf("%q not found in %s", anchor, path)
	return 0, 0
}
