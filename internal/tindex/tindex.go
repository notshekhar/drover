// Package tindex is drover's optional trigram index.
//
// It narrows a search to the files that could match, so the walk-and-read that
// dominates `grep` on a large warehouse only touches those. It does not do the
// matching: that stays in internal/files, which owns drover's line semantics
// (CRLF handling, the whole-file probe, stable ordering, the result cap). The
// index is allowed to be over-broad and is never allowed to be authoritative.
//
// # Why it is off by default
//
// Measured on this machine, drover's plain search costs:
//
//	560 files      6-8ms      today's warehouse
//	5,000 files    52-57ms
//	20,000 files   293-527ms
//	100,000 files  3.5-4.9s
//
// An index has to be built, kept fresh, and reasoned about when it is stale.
// None of that is worth 8ms. It becomes worth it somewhere around twenty
// thousand files, which is why Threshold exists and why a warehouse below it
// silently keeps the simple path.
//
// # Why it cannot return a wrong answer
//
// The only failure an index can cause is being too NARROW — omitting a file
// that really matches, which no later stage can recover because the file is
// never opened. Three things guard that:
//
//   - A pattern the planner cannot narrow reports exhaustive=false, and the
//     caller searches everything.
//   - A path whose bytes no longer match what was indexed is added to the
//     candidate set rather than dropped. That covers both a file the index has
//     never seen and one it has, whose content has since changed — the second
//     is the dangerous case, because the index holds real but obsolete
//     postings for it and would otherwise confidently leave it out.
//   - Every candidate is still matched by drover's own matcher against the
//     file's real bytes. The index decides what to open, never what matched.
package tindex

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	tgindex "github.com/notshekhar/tgrep-go/index"
	"github.com/notshekhar/tgrep-go/matching"
	"github.com/notshekhar/tgrep-go/meta"
	"github.com/notshekhar/tgrep-go/reader"
	tgsearch "github.com/notshekhar/tgrep-go/search"
)

// Threshold is the file count below which an index is not worth having.
//
// Chosen from the measurements in the package comment: at 5,000 files a search
// is already under 60ms, which nobody waits on, while at 20,000 it is half a
// second. The exact number matters less than being on the right side of it —
// the cost of indexing a small warehouse is certain and the benefit is not.
const Threshold = 10_000

// Index is a built index over one warehouse root.
//
// The zero value is unusable; call Open or Build.
type Index struct {
	mu   sync.RWMutex
	rd   *reader.Reader
	dir  string
	root string
	// stamps is what each indexed path looked like when it was indexed. It is
	// how a changed file is recognised as no longer described by the index;
	// see Candidates.
	stamps map[string]meta.FileStamp
	closed bool
}

// Dir is where an index for this data directory lives.
//
// Beside the objects rather than inside the checkouts, because a checkout is a
// git worktree that drover syncs by fetching — anything written into it risks
// showing up in a diff or being blown away by a reset.
func Dir(dataDir string) string { return filepath.Join(dataDir, "index") }

// Build indexes exactly these paths, replacing any existing index.
//
// paths are relative to root and use forward slashes, and their ORDER is the
// contract: file ids are positional. The caller passes the same enumeration it
// would have searched, which is what keeps the index and the walk agreeing
// about what exists.
func Build(ctx context.Context, dataDir, root string, paths []string) (*Index, error) {
	dir := Dir(dataDir)
	// A build writes into a fresh directory and swaps, so a crash midway
	// leaves the previous index intact rather than a half-written one that
	// would read as authoritative.
	staging := dir + ".building"
	if err := os.RemoveAll(staging); err != nil {
		return nil, err
	}
	if _, err := tgindex.BuildFromPaths(staging, root, paths, tgindex.Options{Complete: true}); err != nil {
		os.RemoveAll(staging)
		return nil, fmt.Errorf("building the index: %w", err)
	}
	if err := ctx.Err(); err != nil {
		os.RemoveAll(staging)
		return nil, err
	}

	if err := os.RemoveAll(dir); err != nil {
		os.RemoveAll(staging)
		return nil, err
	}
	if err := os.Rename(staging, dir); err != nil {
		os.RemoveAll(staging)
		return nil, err
	}
	return Open(dataDir, root)
}

// Open maps an existing index. It returns an error when there is none, which
// callers treat as "search without one" rather than as a failure.
func Open(dataDir, root string) (*Index, error) {
	dir := Dir(dataDir)
	rd, err := reader.Open(dir)
	if err != nil {
		return nil, err
	}
	// Without the stamps there is no way to tell a current file from an
	// edited one, and the index would answer confidently about content it no
	// longer describes. An index that cannot be checked is not usable.
	evidence, err := meta.ReadEvidence(dir)
	if err != nil {
		rd.Close()
		return nil, fmt.Errorf("reading index stamps: %w", err)
	}
	return &Index{rd: rd, dir: dir, root: root, stamps: evidence.Stamps}, nil
}

// Close releases the index.
func (ix *Index) Close() error {
	if ix == nil {
		return nil
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if ix.closed || ix.rd == nil {
		return nil
	}
	ix.closed = true
	return ix.rd.Close()
}

// NumFiles is how many files the index holds.
func (ix *Index) NumFiles() int {
	if ix == nil {
		return 0
	}
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return ix.rd.NumFiles()
}

// ErrNoNarrowing reports that the pattern gave the index nothing to work with,
// so the caller must search every file.
var ErrNoNarrowing = errors.New("pattern cannot be narrowed by the index")

// Candidates returns the paths worth opening for this pattern.
//
// known is the set of paths the caller is about to search. Any of them the
// index has never heard of is returned as a candidate regardless: the index
// may predate a file, and a stale index must cost extra reads rather than
// missing results.
//
// ErrNoNarrowing means every path should be searched.
func (ix *Index) Candidates(pattern string, caseSensitive bool, known []string) ([]string, error) {
	if ix == nil {
		return nil, ErrNoNarrowing
	}
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	if ix.closed {
		return nil, ErrNoNarrowing
	}

	cfg := matching.Config{CaseInsensitive: !caseSensitive}
	paths, exhaustive, err := tgsearch.Candidates(ix.rd, []string{pattern}, cfg)
	if err != nil {
		// An unplannable pattern is not a failure; it just means no narrowing.
		return nil, ErrNoNarrowing
	}
	if !exhaustive {
		return nil, ErrNoNarrowing
	}

	nominated := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		nominated[p] = struct{}{}
	}

	// Keep a path when the index nominated it, OR when the index's evidence
	// no longer describes it.
	//
	// The second half is what makes a stale index safe. A stat per path in
	// scope is not free — at a hundred thousand files it is a tenth of a
	// second — but it is an order of magnitude cheaper than the open-and-read
	// it replaces, and the alternative is a search that silently misses every
	// edit made since the last index build.
	out := make([]string, 0, len(paths))
	for _, p := range known {
		if _, isCandidate := nominated[p]; isCandidate {
			out = append(out, p)
			continue
		}
		if !ix.describes(p) {
			out = append(out, p)
		}
	}
	return out, nil
}

// describes reports whether the index's postings still reflect what is on disk
// for this path.
//
// False for a file the index never saw, one whose size or modified time has
// moved, and one that cannot be stat'd — every uncertain case resolves towards
// reading the file, because the cost of being wrong in that direction is time
// and the cost of being wrong in the other is a missing result.
func (ix *Index) describes(rel string) bool {
	recorded, ok := ix.stamps[rel]
	if !ok {
		return false
	}
	info, err := os.Stat(filepath.Join(ix.root, filepath.FromSlash(rel)))
	if err != nil {
		return false
	}
	return meta.StampOf(info) == recorded
}
