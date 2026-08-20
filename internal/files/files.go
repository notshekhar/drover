// Package files implements the four filesystem tools drover hands to agents:
// ls, read, grep and find, all scoped to the repository checkouts.
//
// Everything here is jailed to $DROVER_DATA/repos. The jail is enforced by
// resolving symlinks and re-checking the result, not by rejecting ".." --
// a symlink inside a checked-out repository pointing at /etc/passwd contains
// no dots at all, and a repository is by definition content drover did not
// write.
package files

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Limits keep a tool result inside a context window and keep one careless
// call from walking a million files.
const (
	MaxReadBytes   = 256 << 10 // per read
	MaxLineLen     = 2000      // per line, before truncation
	MaxGrepResults = 200
	MaxFindResults = 500
	MaxListEntries = 1000
	binarySniff    = 8 << 10
)

// Root is the directory holding every checkout.
type Root struct {
	Dir string
}

// New returns a Root over the data directory's repos folder.
//
// The root is canonicalised once here. Containment is checked against the
// resolved form, and on macOS the data directory frequently sits under /var,
// which is itself a symlink to /private/var -- comparing a resolved path
// against an unresolved root would then reject every legitimate file.
func New(dataDir string) *Root {
	return &Root{Dir: canonicalDir(filepath.Join(dataDir, "repos"))}
}

// canonicalDir resolves a path even when it does not exist yet, by resolving
// the nearest ancestor that does and re-appending the rest.
//
// EvalSymlinks fails outright on a missing path, and the repos directory is
// routinely missing before the first clone. Silently leaving the root
// unresolved would then make every containment check fail later, once the
// directory exists and resolves to a different prefix.
func canonicalDir(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	parent := filepath.Dir(path)
	if parent == path {
		return path
	}
	return filepath.Join(canonicalDir(parent), filepath.Base(path))
}

// ErrOutsideRoot is returned when a path would escape the checkouts.
var ErrOutsideRoot = errors.New("path is outside the repository root")

// resolve turns a caller-supplied relative path into an absolute one that is
// provably inside the root.
//
// Two checks, because they catch different things. The lexical check rejects
// "../.." before touching the disk. The symlink check catches a link inside a
// repository whose target is elsewhere -- the case that matters, since
// repository contents are written by whoever wrote the repository.
func (r *Root) resolve(rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("%w: give a path relative to the repository root, like api/src/main.go", ErrOutsideRoot)
	}
	// Reject ".." outright rather than relying on Clean to neutralise it.
	// Clean would fold "../../etc/passwd" into "etc/passwd", which is safely
	// inside the root but then fails as "does not exist" -- an answer that
	// tells the caller nothing about why.
	for _, seg := range strings.Split(filepath.ToSlash(rel), "/") {
		if seg == ".." {
			return "", fmt.Errorf("%w: %q walks upwards", ErrOutsideRoot, rel)
		}
	}
	clean := filepath.Clean("/" + strings.TrimPrefix(rel, "/"))
	abs := filepath.Join(r.Dir, clean)

	if !within(abs, r.Dir) {
		return "", ErrOutsideRoot
	}

	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		// The path may simply not exist; let the caller's own operation
		// report that, but make sure its parent is still inside the root.
		if os.IsNotExist(err) {
			parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
			if err != nil || within(parent, r.Dir) {
				return abs, nil
			}
			return "", ErrOutsideRoot
		}
		return "", err
	}
	if !within(resolved, r.Dir) {
		return "", fmt.Errorf("%w: %s is a link pointing out of the checkouts", ErrOutsideRoot, rel)
	}
	return resolved, nil
}

// resolveDir resolves a search scope and insists it exists. Returning an
// empty result for a path that is not there reads as "no matches", which is a
// different and much more misleading answer than "no such directory".
func (r *Root) resolveDir(rel string) (string, error) {
	abs, err := r.resolve(rel)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", notFound(orRoot(rel), err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is a file, not a directory to search in", r.display(abs))
	}
	return abs, nil
}

func orRoot(rel string) string {
	if strings.TrimSpace(rel) == "" {
		return "the repository root"
	}
	return rel
}

// within reports whether path is dir or sits inside it.
func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel))
}

// display turns an absolute path back into the repo-relative form callers use.
func (r *Root) display(abs string) string {
	rel, err := filepath.Rel(r.Dir, abs)
	if err != nil {
		return abs
	}
	return filepath.ToSlash(rel)
}

// --- ls ---

// Entry is one directory entry.
type Entry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Type  string `json:"type"` // file, dir, symlink
	Size  int64  `json:"size,omitempty"`
	Lines int    `json:"-"`
}

// ListResult is what ls returns.
type ListResult struct {
	Path      string  `json:"path"`
	Entries   []Entry `json:"entries"`
	Truncated bool    `json:"truncated,omitempty"`
}

// List reads one directory. An empty path lists the repositories themselves,
// which is how an agent discovers what drover holds.
func (r *Root) List(rel string) (*ListResult, error) {
	abs, err := r.resolve(rel)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, notFound(rel, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is a file, not a directory; use read", r.display(abs))
	}

	entries, err := os.ReadDir(abs)
	if err != nil {
		return nil, err
	}

	out := &ListResult{Path: r.display(abs)}
	if out.Path == "." {
		out.Path = ""
	}
	for _, e := range entries {
		// .git is drover's bookkeeping and pure noise to an agent reading
		// source. Everything else, including .github and .gitignore, stays.
		if e.Name() == ".git" {
			continue
		}
		if len(out.Entries) >= MaxListEntries {
			out.Truncated = true
			break
		}

		entry := Entry{Name: e.Name(), Path: pathJoin(out.Path, e.Name())}
		switch {
		case e.Type()&os.ModeSymlink != 0:
			entry.Type = "symlink"
		case e.IsDir():
			entry.Type = "dir"
		default:
			entry.Type = "file"
			if info, err := e.Info(); err == nil {
				entry.Size = info.Size()
			}
		}
		out.Entries = append(out.Entries, entry)
	}

	sort.Slice(out.Entries, func(i, j int) bool {
		a, b := out.Entries[i], out.Entries[j]
		if (a.Type == "dir") != (b.Type == "dir") {
			return a.Type == "dir"
		}
		return a.Name < b.Name
	})
	return out, nil
}

func pathJoin(base, name string) string {
	if base == "" || base == "." {
		return name
	}
	return base + "/" + name
}

// --- read ---

// ReadResult is what read returns.
type ReadResult struct {
	Path       string `json:"path"`
	Content    string `json:"content"`
	StartLine  int    `json:"startLine"`
	EndLine    int    `json:"endLine"`
	TotalLines int    `json:"totalLines"`
	Truncated  bool   `json:"truncated,omitempty"`
}

// Read returns part of a file. offset is a 1-based line number; limit is a
// line count. Both zero means the whole file, up to the byte cap.
func (r *Root) Read(rel string, offset, limit int) (*ReadResult, error) {
	abs, err := r.resolve(rel)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, notFound(rel, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%s is a directory; use ls", r.display(abs))
	}

	f, err := os.Open(abs)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	if binary, err := looksBinary(f); err != nil {
		return nil, err
	} else if binary {
		return nil, fmt.Errorf("%s looks like a binary file", r.display(abs))
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}

	if offset < 1 {
		offset = 1
	}

	var b strings.Builder
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 10<<20)

	line, start, end, taken := 0, 0, 0, 0
	truncated := false
	for sc.Scan() {
		line++
		if line < offset {
			continue
		}
		if limit > 0 && taken >= limit {
			truncated = true
			break
		}
		if b.Len() >= MaxReadBytes {
			truncated = true
			break
		}
		if start == 0 {
			start = line
		}
		end = line
		taken++
		b.WriteString(clipLine(sc.Text()))
		b.WriteByte('\n')
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", r.display(abs), err)
	}

	// Keep counting past the window, so the caller is told how much file it
	// did not get rather than being left to guess from the line numbers.
	total := line
	for sc.Scan() {
		total++
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", r.display(abs), err)
	}
	if total > end {
		truncated = true
	}

	return &ReadResult{
		Path:       r.display(abs),
		Content:    b.String(),
		StartLine:  start,
		EndLine:    end,
		TotalLines: total,
		Truncated:  truncated,
	}, nil
}

func clipLine(s string) string {
	if len(s) <= MaxLineLen {
		return s
	}
	return s[:MaxLineLen] + "… (line truncated)"
}

// looksBinary sniffs for a NUL byte, which no text file has.
func looksBinary(f *os.File) (bool, error) {
	buf := make([]byte, binarySniff)
	n, err := f.Read(buf)
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	return bytes.IndexByte(buf[:n], 0) >= 0, nil
}

// --- grep ---

// Match is one grep hit.
type Match struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

// GrepResult is what grep returns.
type GrepResult struct {
	Matches   []Match `json:"matches"`
	Files     int     `json:"filesSearched"`
	Truncated bool    `json:"truncated,omitempty"`
}

// GrepOptions narrow a search.
type GrepOptions struct {
	Path          string // subtree to search, relative to the root
	Include       string // glob on the file name, e.g. *.go
	CaseSensitive bool
	MaxResults    int
}

// Grep searches file contents for a regular expression.
func (r *Root) Grep(pattern string, opts GrepOptions) (*GrepResult, error) {
	if strings.TrimSpace(pattern) == "" {
		return nil, errors.New("the pattern is empty")
	}
	expr := pattern
	if !opts.CaseSensitive {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return nil, fmt.Errorf("pattern is not a valid regular expression: %w", err)
	}

	base, err := r.resolveDir(opts.Path)
	if err != nil {
		return nil, err
	}
	max := opts.MaxResults
	if max <= 0 || max > MaxGrepResults {
		max = MaxGrepResults
	}

	out := &GrepResult{Matches: []Match{}}
	err = r.walk(base, func(abs string, d os.DirEntry) error {
		if opts.Include != "" {
			ok, err := filepath.Match(opts.Include, d.Name())
			if err != nil {
				return fmt.Errorf("include %q is not a valid glob: %w", opts.Include, err)
			}
			if !ok {
				return nil
			}
		}
		out.Files++
		return r.grepFile(abs, re, out, max)
	})
	if err != nil && !errors.Is(err, errStop) {
		return nil, err
	}
	if errors.Is(err, errStop) {
		out.Truncated = true
	}
	return out, nil
}

func (r *Root) grepFile(abs string, re *regexp.Regexp, out *GrepResult, max int) error {
	f, err := os.Open(abs)
	if err != nil {
		return nil // unreadable file is not a search failure
	}
	defer f.Close()

	if binary, err := looksBinary(f); err != nil || binary {
		return nil
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil
	}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 10<<20)
	line := 0
	for sc.Scan() {
		line++
		text := sc.Text()
		if !re.MatchString(text) {
			continue
		}
		if len(out.Matches) >= max {
			return errStop
		}
		out.Matches = append(out.Matches, Match{
			Path: r.display(abs),
			Line: line,
			Text: clipLine(strings.TrimRight(text, "\r")),
		})
	}
	return nil
}

// --- find ---

// FindResult is what find returns.
type FindResult struct {
	Paths     []string `json:"paths"`
	Truncated bool     `json:"truncated,omitempty"`
}

// Find matches paths by glob. A pattern with no slash matches the file name;
// one with a slash matches the whole repo-relative path, so both `*.go` and
// `api/internal/*.go` do what they look like.
func (r *Root) Find(pattern, path string, maxResults int) (*FindResult, error) {
	if strings.TrimSpace(pattern) == "" {
		return nil, errors.New("the pattern is empty")
	}
	if _, err := filepath.Match(pattern, "probe"); err != nil {
		return nil, fmt.Errorf("pattern %q is not a valid glob: %w", pattern, err)
	}

	base, err := r.resolveDir(path)
	if err != nil {
		return nil, err
	}
	max := maxResults
	if max <= 0 || max > MaxFindResults {
		max = MaxFindResults
	}
	matchWholePath := strings.Contains(pattern, "/")

	out := &FindResult{Paths: []string{}}
	err = r.walk(base, func(abs string, d os.DirEntry) error {
		subject := d.Name()
		if matchWholePath {
			subject = r.display(abs)
		}
		ok, err := filepath.Match(pattern, subject)
		if err != nil || !ok {
			return nil
		}
		if len(out.Paths) >= max {
			return errStop
		}
		out.Paths = append(out.Paths, r.display(abs))
		return nil
	})
	if err != nil && !errors.Is(err, errStop) {
		return nil, err
	}
	if errors.Is(err, errStop) {
		out.Truncated = true
	}
	sort.Strings(out.Paths)
	return out, nil
}

// --- walking ---

var errStop = errors.New("enough")

// walk visits every regular file under base, skipping .git and never
// following a symlink out of the root.
func (r *Root) walk(base string, fn func(abs string, d os.DirEntry) error) error {
	return filepath.WalkDir(base, func(abs string, d os.DirEntry, err error) error {
		if err != nil {
			// An unreadable directory should not abort the whole search.
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		// Do not follow symlinks while walking. Reading one deliberately is
		// still possible through read, where resolve checks the target.
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		return fn(abs, d)
	})
}

func notFound(rel string, err error) error {
	if os.IsNotExist(err) {
		return fmt.Errorf("%s does not exist", rel)
	}
	return err
}
