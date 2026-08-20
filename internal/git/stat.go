package git

import (
	"os"
	"path/filepath"
)

// statPath stats a repository-relative path. It is its own function so the
// join happens in exactly one place: every caller has already had its path
// checked for "..", and this is where that check would stop mattering if it
// were done inline.
func statPath(dir, rel string) (os.FileInfo, error) {
	return os.Stat(filepath.Join(dir, filepath.FromSlash(rel)))
}
