// Package docs carries the reference drover writes into its data directory.
//
// The file exists so a person can say "read ~/.drover/docs.md and add our
// repos" to an agent and have that work. It is embedded rather than generated,
// so it is a real markdown file in the repo that can be read and edited
// directly.
package docs

import (
	_ "embed"
	"os"
	"path/filepath"
)

//go:embed docs.md
var Markdown string

// FileName is what the reference is called in the data directory.
//
// docs.md, deliberately not AGENTS.md or CLAUDE.md: those names are claimed by
// tools that load them automatically as instructions, and this is a reference
// someone points at on purpose.
const FileName = "docs.md"

// Ensure writes the reference into the data directory when it is not there.
//
// An existing file is left alone. Someone may have annotated it with notes
// about their own setup, and silently overwriting that on every start would
// make it a bad place to keep anything.
//
// It returns whether a file was written, so a first run can say so.
func Ensure(dataDir string) (bool, error) {
	path := filepath.Join(dataDir, FileName)
	if _, err := os.Stat(path); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, err
	}

	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return false, err
	}
	if err := os.WriteFile(path, []byte(Markdown), 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// Path is where the reference lives for a given data directory.
func Path(dataDir string) string { return filepath.Join(dataDir, FileName) }
