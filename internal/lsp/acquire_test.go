package lsp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestJavaGateFailsClosed is the macOS stub: /usr/bin/java exists, runs, and
// reports no runtime. Present on PATH is not the same as usable, and this must
// decline before downloading 50MB to find out.
//
// NoInstall, so that a machine which does have a working JVM -- a CI runner,
// say -- checks the gate rather than spending four minutes pulling jdtls from
// Eclipse to prove it.
func TestJavaGateFailsClosed(t *testing.T) {
	a := NewAcquirer(t.TempDir())
	a.NoInstall = true

	start := time.Now()
	err := a.CheckRequirements(context.Background(), DefinitionByKey("java"))
	elapsed := time.Since(start)
	if elapsed > 5*time.Second {
		t.Errorf("the gate should decline before any download, took %s", elapsed)
	}
	if err == nil {
		t.Logf("this machine has a usable JVM; the gate passed in %s", elapsed)
		return
	}
	if !strings.Contains(err.Error(), "java") {
		t.Errorf("the message should name the toolchain: %v", err)
	}
	t.Logf("declined in %s: %v", elapsed, err)
}

// TestFindsInstalledGopls proves drover's own bin directory is preferred and
// reported as such.
func TestFindsInstalledGopls(t *testing.T) {
	dir := filepath.Join(os.Getenv("HOME"), ".drover", "servers")
	if _, err := os.Stat(filepath.Join(dir, "bin", "gopls")); err != nil {
		t.Skip("gopls is not installed")
	}
	a := NewAcquirer(dir)
	res, err := a.Ensure(context.Background(), DefinitionByKey("go"))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(res.Bin) != "gopls" {
		t.Errorf("bin = %s", res.Bin)
	}
	t.Logf("go: %s (%s)", res.Bin, res.Source)
}

// TestInstallsTypeScriptFromNPM exercises the whole npm route for real: no
// npm, no node, just the registry over HTTPS and a tar in the standard
// library.
//
// Opt-in, because it reaches the network and pulls 9MB. Making every CI run
// download TypeScript to prove the tar reader still works is a poor trade;
// run it deliberately when the acquisition code changes.
func TestInstallsTypeScriptFromNPM(t *testing.T) {
	if os.Getenv("DROVER_NETWORK_TESTS") == "" {
		t.Skip("set DROVER_NETWORK_TESTS=1 to run; downloads 9MB from npm")
	}
	a := NewAcquirer(t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	res, err := a.Ensure(ctx, DefinitionByKey("typescript"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("typescript: %s (%s, version %s) args %v", res.Bin, res.Source, res.Version, res.Args)
	if res.Version != "7" {
		t.Errorf("the version gate should have confirmed 7, got %q", res.Version)
	}

	// It has to be a language server, not just a binary that exists.
	c, err := Start(ctx, "typescript", t.TempDir(), res.Bin, res.Args, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Initialize(ctx, nil); err != nil {
		t.Fatalf("handshake: %v\nstderr: %s", err, c.Stderr())
	}
	for _, capability := range []string{"definitionProvider", "referencesProvider", "hoverProvider", "documentSymbolProvider", "workspaceSymbolProvider", "diagnosticProvider"} {
		t.Logf("  %-24s %v", capability, c.Supports(capability))
	}
	if !c.Supports("definitionProvider") {
		t.Error("tsc --lsp should advertise definitionProvider")
	}
}

func TestArchiveEntriesCannotEscape(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"../escape", "/etc/passwd", "a/../../escape"} {
		if _, err := safeJoin(dir, name); err == nil {
			t.Errorf("%q should have been refused", name)
		}
	}
	if _, err := safeJoin(dir, "package/lib/tsc"); err != nil {
		t.Errorf("a normal entry was refused: %v", err)
	}
}
