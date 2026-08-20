// Package atomicfile writes a file in a way that a killed process cannot
// leave half-written.
//
// Both the object store and the config depend on this. A truncated object
// under ~/.drover/objects is not a cosmetic problem: serve reads that tree on
// boot and refuses to start if a document does not parse, so a crash during a
// save would otherwise wedge the engine until someone deleted the file by
// hand.
package atomicfile

import (
	"os"
	"path/filepath"
)

// Write creates a temp file in the target's own directory, syncs it, and
// renames it into place. Same directory matters: rename is only atomic within
// a filesystem, and a temp dir can be on a different one.
func Write(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	// Removing after a successful rename is a no-op that fails silently, so
	// this is safe on both paths.
	defer os.Remove(tmp)

	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
