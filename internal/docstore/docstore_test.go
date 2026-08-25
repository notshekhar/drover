package docstore

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/notshekhar/drover/internal/object"
)

func newStore(t *testing.T, spec *object.DocumentStoreSpec) (*Manager, string) {
	t.Helper()
	m := New(t.TempDir())
	if err := m.Ensure(context.Background(), "product", spec); err != nil {
		t.Fatal(err)
	}
	return m, m.Dir("product", spec)
}

func write(t *testing.T, m *Manager, spec *object.DocumentStoreSpec, rel, body, reason string) *WriteResult {
	t.Helper()
	abs := filepath.Join(m.Dir("product", spec), rel)
	res, err := m.Write(context.Background(), abs, WriteRequest{
		Store: "product", Rel: rel, Content: body, Reason: reason, Author: "claude-code via mcp-stdio",
	}, spec)
	if err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
	return res
}

func TestWriteCreatesAndReplaces(t *testing.T) {
	spec := &object.DocumentStoreSpec{}
	m, dir := newStore(t, spec)

	res := write(t, m, spec, "prd-billing.md", "# Billing\n\nfirst draft", "draft the billing PRD")
	if !res.Created {
		t.Error("the first write did not report a creation")
	}
	body, err := os.ReadFile(filepath.Join(dir, "prd-billing.md"))
	if err != nil {
		t.Fatal(err)
	}
	// A document written twice by two agents should not diff on whitespace.
	if !strings.HasSuffix(string(body), "\n") {
		t.Error("the document has no trailing newline")
	}

	res = write(t, m, spec, "prd-billing.md", "# Billing\n\nsecond draft", "revise")
	if res.Created {
		t.Error("a replacement reported a creation")
	}
}

// Committing an identical write would fill the history with nothing, and
// history is the reason it is worth keeping.
func TestIdenticalWriteIsNotACommit(t *testing.T) {
	spec := &object.DocumentStoreSpec{}
	m, _ := newStore(t, spec)
	write(t, m, spec, "a.md", "same", "first")
	res := write(t, m, spec, "a.md", "same", "again")
	if !res.Unchanged || res.Commit != "" {
		t.Fatalf("res was %+v", res)
	}
}

// Every write is a commit with the agent's attribution and its stated reason,
// so "who wrote this and why" is answerable with the git tool that exists.
func TestWritesAreCommittedWithAttribution(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	spec := &object.DocumentStoreSpec{}
	m, dir := newStore(t, spec)
	res := write(t, m, spec, "decisions/0001-why-postgres.md", "# Why postgres\n", "record the postgres decision")
	if res.Commit == "" {
		t.Fatal("the write was not committed")
	}

	out, err := exec.Command("git", "-C", dir, "log", "-1", "--format=%an%x1f%s").Output()
	if err != nil {
		t.Fatal(err)
	}
	author, subject, _ := strings.Cut(strings.TrimSpace(string(out)), "\x1f")
	if !strings.Contains(author, "claude-code") {
		t.Errorf("the commit author is %q", author)
	}
	if subject != "record the postgres decision" {
		t.Errorf("the commit message is %q", subject)
	}
}

// A store exists to hold prose that grep can read. A binary blob in there is
// a result nobody can read and a file nobody can review.
func TestOnlyMarkdown(t *testing.T) {
	for _, bad := range []string{"notes.txt", "bundle.js", "image.png", "x"} {
		if err := ValidateDocumentPath(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
	for _, ok := range []string{"a.md", "decisions/0001.md", "deep/nested/note.markdown"} {
		if err := ValidateDocumentPath(ok); err != nil {
			t.Errorf("%q was refused: %v", ok, err)
		}
	}
}

// The store's own .git is not the agent's to write in.
func TestHiddenSegmentsAreRefused(t *testing.T) {
	for _, bad := range []string{".git/config.md", "a/../../b.md", "../escape.md", ".hidden.md"} {
		if err := ValidateDocumentPath(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

func TestReadOnlyStoreRefusesWrites(t *testing.T) {
	no := false
	spec := &object.DocumentStoreSpec{Writable: &no}
	m, _ := newStore(t, spec)
	abs := filepath.Join(m.Dir("product", spec), "a.md")
	if _, err := m.Write(context.Background(), abs, WriteRequest{Store: "product", Rel: "a.md", Content: "x"}, spec); err == nil {
		t.Fatal("a read-only store accepted a write")
	}
}

// A store pointed at somebody's own docs folder is theirs. Deleting the
// object should stop drover offering it, not destroy the documents.
func TestRemoveLeavesAnExternalPathAlone(t *testing.T) {
	external := t.TempDir()
	spec := &object.DocumentStoreSpec{Path: external}
	m := New(t.TempDir())
	if err := m.Ensure(context.Background(), "product", spec); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(external, "keep.md"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.Remove("product", spec); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(external, "keep.md")); err != nil {
		t.Fatalf("drover deleted a directory it did not create: %v", err)
	}
}

func TestOversizedDocumentIsRefused(t *testing.T) {
	spec := &object.DocumentStoreSpec{}
	m, _ := newStore(t, spec)
	abs := filepath.Join(m.Dir("product", spec), "big.md")
	_, err := m.Write(context.Background(), abs, WriteRequest{
		Store: "product", Rel: "big.md", Content: strings.Repeat("a", MaxDocumentBytes+1),
	}, spec)
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("err was %v", err)
	}
}
