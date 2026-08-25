// Package docstore is the only place in drover that writes on an agent's
// behalf.
//
// Everything else drover offers is read-only by construction: GET requests,
// read-only SQL, file tools with no write. That is the right default and it
// leaves a real gap -- the warehouse has nowhere to put what an agent worked
// out, so every session starts from nothing. A document store is that place,
// and it is deliberately the narrowest possible exception: one tool, markdown
// only, jailed to a directory somebody declared, and every write recorded.
package docstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/notshekhar/drover/internal/atomicfile"
	"github.com/notshekhar/drover/internal/object"
)

// RootPrefix is the path prefix every store sits under, as an agent sees it.
const RootPrefix = "documents"

// MaxDocumentBytes caps one document. Generous for prose, and small enough
// that a runaway write cannot fill a disk.
const MaxDocumentBytes = 1 << 20

// Manager owns the directories behind the document stores.
type Manager struct {
	DataDir string

	// NoHistory turns off the local git repository. Tests set it; so does
	// anyone whose data directory is itself inside a repository they would
	// rather not nest.
	NoHistory bool
}

// New builds a Manager over a data directory.
func New(dataDir string) *Manager { return &Manager{DataDir: dataDir} }

// Root is the default parent of every store.
func (m *Manager) Root() string { return filepath.Join(m.DataDir, RootPrefix) }

// Dir resolves where one store's content lives.
func (m *Manager) Dir(name string, spec *object.DocumentStoreSpec) string {
	if p := strings.TrimSpace(spec.Path); p != "" {
		return filepath.Clean(p)
	}
	return filepath.Join(m.Root(), name)
}

// PathPrefix is how an agent names this store.
func PathPrefix(name string) string { return RootPrefix + "/" + name }

// Ensure creates a store's directory and, when history is on, its local git
// repository.
//
// A store is created eagerly at apply rather than on first write, because a
// store an agent cannot see is a store it will not use: ls has to list it.
func (m *Manager) Ensure(ctx context.Context, name string, spec *object.DocumentStoreSpec) error {
	dir := m.Dir(name, spec)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if m.NoHistory || !spec.KeepsHistory() {
		return nil
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		return nil
	}
	// A local repository with no remote. drover never pushes; this is undo,
	// and the answer to "who wrote this and why".
	if out, err := git(ctx, dir, "init", "--quiet"); err != nil {
		return fmt.Errorf("git init in the %s store: %v: %s", name, err, out)
	}
	return nil
}

// Remove deletes a store's content, for `drover delete`.
//
// A store with its own spec.path is left alone. drover did not create that
// directory and it is somebody's own docs folder; deleting the object should
// stop drover offering it, not destroy the documents.
func (m *Manager) Remove(name string, spec *object.DocumentStoreSpec) error {
	if err := object.ValidateName(name); err != nil {
		return err
	}
	if strings.TrimSpace(spec.Path) != "" {
		return nil
	}
	return os.RemoveAll(filepath.Join(m.Root(), name))
}

// Count reports how many documents a store holds, for the listings.
func (m *Manager) Count(name string, spec *object.DocumentStoreSpec) int {
	dir := m.Dir(name, spec)
	n := 0
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(strings.ToLower(d.Name()), ".md") {
			n++
		}
		return nil
	})
	return n
}

// WriteRequest is one document write.
type WriteRequest struct {
	Store   string
	Rel     string // path inside the store
	Content string
	Reason  string // what the agent says it is doing; recorded, never trusted
	Author  string // resolved attribution, e.g. "claude-code 2.1.4 over stdio"
}

// WriteResult is what a write did.
type WriteResult struct {
	Path      string `json:"path"`
	Bytes     int    `json:"bytes"`
	Created   bool   `json:"created"`
	Commit    string `json:"commit,omitempty"`
	Unchanged bool   `json:"unchanged,omitempty"`
}

// Write stores one document and commits it.
//
// abs must already have been produced by the file jail, so this function does
// no containment checking of its own -- there is exactly one jail and it is
// not here.
func (m *Manager) Write(ctx context.Context, abs string, req WriteRequest, spec *object.DocumentStoreSpec) (*WriteResult, error) {
	if err := ValidateDocumentPath(req.Rel); err != nil {
		return nil, err
	}
	if len(req.Content) > MaxDocumentBytes {
		return nil, fmt.Errorf("the document is %d bytes; the limit is %d", len(req.Content), MaxDocumentBytes)
	}
	if !spec.IsWritable() {
		return nil, fmt.Errorf("the %s store is not writable (spec.writable: false)", req.Store)
	}

	body := normalise(req.Content)
	res := &WriteResult{Path: req.Rel, Bytes: len(body)}

	existing, err := os.ReadFile(abs)
	switch {
	case err == nil:
		if bytes.Equal(existing, body) {
			// Committing an identical write would fill the history with
			// nothing, and history is the reason it is worth keeping.
			res.Unchanged = true
			return res, nil
		}
	case errors.Is(err, os.ErrNotExist):
		res.Created = true
	default:
		return nil, err
	}

	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return nil, err
	}
	if err := atomicfile.Write(abs, body, 0o644); err != nil {
		return nil, err
	}

	if !m.NoHistory && spec.KeepsHistory() {
		commit, err := m.commit(ctx, m.Dir(req.Store, spec), req)
		if err != nil {
			// The document is on disk. Failing the whole write because git
			// could not record it would throw away the thing that was asked
			// for in order to protect the audit trail of it.
			return res, nil
		}
		res.Commit = commit
	}
	return res, nil
}

// commit records one write in the store's local history.
func (m *Manager) commit(ctx context.Context, dir string, req WriteRequest) (string, error) {
	if _, err := git(ctx, dir, "add", "--", req.Rel); err != nil {
		return "", err
	}
	message := strings.TrimSpace(req.Reason)
	if message == "" {
		message = "write " + req.Rel
	}
	message = firstLine(message)

	author := strings.TrimSpace(req.Author)
	if author == "" {
		author = "an agent"
	}
	args := []string{
		"-c", "user.name=" + sanitiseIdentity(author),
		"-c", "user.email=agent@drover.invalid",
		"commit", "--quiet", "--allow-empty-message",
		"-m", message,
		"--", req.Rel,
	}
	if out, err := git(ctx, dir, args...); err != nil {
		return "", fmt.Errorf("%v: %s", err, out)
	}
	sha, err := git(ctx, dir, "rev-parse", "--short", "HEAD")
	return strings.TrimSpace(sha), err
}

// ValidateDocumentPath enforces what a document may be called.
//
// Markdown only, and not because of taste: the store's whole value is that
// grep over it returns readable prose. A binary blob or a minified bundle in
// there is a result nobody can read and a file nobody can review.
func ValidateDocumentPath(rel string) error {
	rel = strings.TrimSpace(rel)
	switch {
	case rel == "":
		return errors.New("give a path inside the store, like prd-billing.md or decisions/0001-why.md")
	case strings.HasPrefix(rel, "/"):
		return errors.New("give a path relative to the store")
	case strings.Contains(rel, ".."):
		return errors.New("a path may not walk upwards")
	}
	for _, seg := range strings.Split(filepath.ToSlash(rel), "/") {
		if seg == "" || seg == "." {
			return fmt.Errorf("%q has an empty path segment", rel)
		}
		if strings.HasPrefix(seg, ".") {
			return fmt.Errorf("%q has a hidden segment; the store's own .git is not yours to write in", rel)
		}
	}
	if ext := strings.ToLower(filepath.Ext(rel)); ext != ".md" && ext != ".markdown" {
		return fmt.Errorf("%q is not markdown; a document store holds prose that grep can read", rel)
	}
	return nil
}

// normalise gives every document the same line endings and a trailing
// newline, so a document written twice by two agents does not diff on
// whitespace alone.
func normalise(s string) []byte {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	return []byte(s)
}

// Document is one entry in a store's listing.
type Document struct {
	Path     string    `json:"path"`
	Bytes    int64     `json:"bytes"`
	Modified time.Time `json:"modified"`
}

// List returns the documents in a store, newest first.
func (m *Manager) List(name string, spec *object.DocumentStoreSpec) []Document {
	dir := m.Dir(name, spec)
	var out []Document
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return nil
		}
		out = append(out, Document{Path: filepath.ToSlash(rel), Bytes: info.Size(), Modified: info.ModTime()})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Modified.After(out[j].Modified) })
	return out
}

// git runs one read or write against a store's local repository.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// sanitiseIdentity keeps an attribution string inside what git will accept as
// an author name: no newlines, no angle brackets to close the address early.
func sanitiseIdentity(s string) string {
	s = strings.NewReplacer("\n", " ", "\r", " ", "<", "(", ">", ")").Replace(s)
	if len(s) > 120 {
		s = s[:120]
	}
	return strings.TrimSpace(s)
}

func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:197] + "..."
	}
	return strings.TrimSpace(s)
}
