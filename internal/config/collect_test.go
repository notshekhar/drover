package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCollectFileIsItself(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "repo.yaml")
	write(t, f, repoYAML)

	got, err := CollectFiles(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != f {
		t.Errorf("got %v, want [%s]", got, f)
	}
}

// An explicitly named file is applied whatever it is called. Only directory
// scanning filters by extension.
func TestCollectNamedFileIgnoresExtension(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "repo.txt")
	write(t, f, repoYAML)

	got, err := CollectFiles(f)
	if err != nil {
		t.Fatalf("an explicitly named file must be applied whatever its extension: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("got %v", got)
	}
}

func TestCollectDirIsOneLevelAndSorted(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "b.yaml"), repoYAML)
	write(t, filepath.Join(dir, "a.yml"), repoYAML)
	write(t, filepath.Join(dir, "notes.md"), "ignore me")
	write(t, filepath.Join(dir, ".hidden.yaml"), repoYAML)
	write(t, filepath.Join(dir, "nested", "deep.yaml"), repoYAML)

	got, err := CollectFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(dir, "a.yml"), filepath.Join(dir, "b.yaml")}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %s, want %s", i, got[i], want[i])
		}
	}
}

func TestCollectDirWithNoYAML(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "readme.md"), "hi")
	_, err := CollectFiles(dir)
	if err == nil {
		t.Fatal("a directory with no yaml must be an error, not a silent no-op")
	}
	if !strings.Contains(err.Error(), "no .yaml") {
		t.Errorf("error = %q", err)
	}
}

func TestCollectMissingPath(t *testing.T) {
	if _, err := CollectFiles(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("want an error for a missing path")
	}
}

// The same file named directly and reached through its directory is one file,
// or it would be applied twice and hit its own duplicate-name check.
func TestCollectAllDedupes(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "api.yaml")
	write(t, f, repoYAML)

	got, err := CollectAll([]string{dir, f})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("got %v, want one file", got)
	}
}

func TestCollectAllKeepsOrder(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.yaml")
	b := filepath.Join(dir, "b.yaml")
	write(t, a, repoYAML)
	write(t, b, repoYAML)

	got, err := CollectAll([]string{b, a})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != b || got[1] != a {
		t.Errorf("got %v, want the order the paths were given", got)
	}
}

func TestIsYAML(t *testing.T) {
	for _, n := range []string{"a.yaml", "a.yml", "A.YAML", "x/y.yaml"} {
		if !IsYAML(n) {
			t.Errorf("IsYAML(%q) = false", n)
		}
	}
	for _, n := range []string{"a.md", "a", "a.yaml.bak", "a.json"} {
		if IsYAML(n) {
			t.Errorf("IsYAML(%q) = true", n)
		}
	}
}

func TestCollectAllReportsMissingPath(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.yaml"), repoYAML)
	missing := filepath.Join(dir, "gone")

	_, err := CollectAll([]string{dir, missing})
	if err == nil {
		t.Fatal("a missing path must fail the whole collection")
	}
	if !strings.Contains(err.Error(), "gone") {
		t.Errorf("error = %q, want it to name the missing path", err)
	}
	if _, statErr := os.Stat(missing); statErr == nil {
		t.Fatal("test setup is wrong: the path exists")
	}
}

// Dropping a yaml file in ~/.drover is the whole workflow the reference
// describes, so the scan has to find it -- and has to ignore drover's own
// config and internal directories.
func TestDropInFiles(t *testing.T) {
	dir := t.TempDir()

	write(t, filepath.Join(dir, "repos.yaml"), repoYAML)
	write(t, filepath.Join(dir, "github-api.yml"), repoYAML)
	write(t, filepath.Join(dir, "config.yaml"), "listen: 127.0.0.1:7432\n")
	write(t, filepath.Join(dir, "docs.md"), "# docs\n")
	write(t, filepath.Join(dir, ".hidden.yaml"), repoYAML)
	write(t, filepath.Join(dir, "objects", "Repository", "api.yaml"), repoYAML)
	write(t, filepath.Join(dir, "repos", "api", "thing.yaml"), repoYAML)

	got, err := DropInFiles(dir)
	if err != nil {
		t.Fatal(err)
	}

	want := []string{filepath.Join(dir, "github-api.yml"), filepath.Join(dir, "repos.yaml")}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got %v, want %v", got, want)
		}
	}
}

// config.yaml is drover's own settings. Scanning it in as an object would
// fail every single boot with "kind is required".
func TestDropInFilesSkipsConfig(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "config.yaml"), "listen: 127.0.0.1:7432\n")

	got, err := DropInFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want nothing -- config.yaml is not an object file", got)
	}
}

func TestDropInFilesOnMissingDir(t *testing.T) {
	got, err := DropInFiles(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatalf("a missing data dir must not be an error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v", got)
	}
}
