package docs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureWritesOnce(t *testing.T) {
	dir := t.TempDir()

	written, err := Ensure(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !written {
		t.Fatal("the first call did not write the reference")
	}
	body, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != Markdown {
		t.Error("the written file does not match the embedded reference")
	}

	// Someone may have annotated it with notes about their own setup, so a
	// later start must leave it alone.
	edited := string(body) + "\n\n## our notes\nthe warehouse is behind the VPN.\n"
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	written, err = Ensure(dir)
	if err != nil {
		t.Fatal(err)
	}
	if written {
		t.Error("the second call reported writing again")
	}
	after, _ := os.ReadFile(filepath.Join(dir, FileName))
	if string(after) != edited {
		t.Error("an existing file was overwritten; local notes would be lost")
	}
}

func TestEnsureCreatesTheDataDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does", "not", "exist")
	if _, err := Ensure(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, FileName)); err != nil {
		t.Errorf("the reference was not written into a fresh data dir: %v", err)
	}
}

// The file is what an agent reads to write correct documents, so the rules it
// would otherwise get wrong have to actually be in there.
func TestReferenceCoversEveryKindAndItsTraps(t *testing.T) {
	for _, want := range []string{
		"kind: Repository", "kind: Environment", "kind: HTTPRequest", "kind: SQLConnection",
		"refreshInterval",
		"postgres", "mysql", "redshift",
		"{{name}}", "${NAME}", "{name}",
		"readOnly",
		"never be advertised", // GET-only for agents
		"${ENV_VAR}",          // the secrets rule
		"cannot share a name", // uniqueness
		"lowercase",           // the name rule
		"drover apply -f",     // how to check a file
		"/mcp",                // how to hand it to an agent
	} {
		if !strings.Contains(Markdown, want) {
			t.Errorf("the reference never mentions %q", want)
		}
	}
}

// It is docs.md on purpose: AGENTS.md and CLAUDE.md are names other tools load
// automatically as instructions, and this is a reference someone points at.
func TestFileNameIsDocsMarkdown(t *testing.T) {
	if FileName != "docs.md" {
		t.Errorf("FileName = %q, want docs.md", FileName)
	}
}
