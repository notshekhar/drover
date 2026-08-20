package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/notshekhar/drover/internal/object"
)

const repoDoc = `apiVersion: drover/v1
kind: Repository
metadata:
  name: api
spec:
  url: https://github.com/acme/api
  branch: main
  refreshInterval: 15m
`

func parseOne(t *testing.T, src, doc string) *object.Object {
	t.Helper()
	objs, err := object.Parse(src, []byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return objs[0]
}

func newStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s, dir
}

// The core promise: what apply writes, a later read gets back.
func TestPutGetRoundTrip(t *testing.T) {
	s, dataDir := newStore(t)
	in := parseOne(t, "/work/repo.yaml", repoDoc)

	if err := s.Put(in); err != nil {
		t.Fatal(err)
	}

	want := filepath.Join(dataDir, "objects", "Repository", "api.yaml")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("object was not written to %s: %v", want, err)
	}

	got, err := s.Get(object.KindRepository, "api")
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata.Name != "api" || got.Kind != object.KindRepository {
		t.Errorf("got %s", got.Ref())
	}

	spec, err := got.Repository()
	if err != nil {
		t.Fatal(err)
	}
	if spec.URL != "https://github.com/acme/api" || spec.Branch != "main" {
		t.Errorf("spec = %+v", spec)
	}
	// The per-repository refresh interval must survive the round trip, or a
	// restart silently moves every repo back to the server default.
	if !spec.RefreshInterval.Set || spec.RefreshInterval.Duration.Minutes() != 15 {
		t.Errorf("refreshInterval = %v, want 15m", spec.RefreshInterval)
	}
}

func TestPutRecordsProvenance(t *testing.T) {
	s, _ := newStore(t)
	if err := s.Put(parseOne(t, "/work/repo.yaml", repoDoc)); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(object.KindRepository, "api")
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata.Source != "/work/repo.yaml" {
		t.Errorf("source = %q, want /work/repo.yaml", got.Metadata.Source)
	}
	if got.Metadata.AppliedAt == "" {
		t.Error("appliedAt was not recorded")
	}
}

// A document cannot claim its own provenance -- that is the server's to write.
func TestUserSuppliedProvenanceIsIgnored(t *testing.T) {
	doc := `apiVersion: drover/v1
kind: Repository
metadata:
  name: api
  source: /somewhere/i/made/up.yaml
  appliedAt: "1999-01-01T00:00:00Z"
spec:
  url: https://github.com/acme/api
  branch: main
`
	s, _ := newStore(t)
	if err := s.Put(parseOne(t, "/real/path.yaml", doc)); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(object.KindRepository, "api")
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata.Source != "/real/path.yaml" {
		t.Errorf("source = %q, want the real apply path", got.Metadata.Source)
	}
	if strings.HasPrefix(got.Metadata.AppliedAt, "1999") {
		t.Error("appliedAt was taken from the document")
	}
}

// Re-applying a name updates the one object rather than making a second.
func TestPutIsUpsert(t *testing.T) {
	s, _ := newStore(t)
	if err := s.Put(parseOne(t, "/work/a.yaml", repoDoc)); err != nil {
		t.Fatal(err)
	}
	updated := strings.Replace(repoDoc, "branch: main", "branch: develop", 1)
	if err := s.Put(parseOne(t, "/work/b.yaml", updated)); err != nil {
		t.Fatal(err)
	}

	objs, err := s.List(object.KindRepository)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 1 {
		t.Fatalf("got %d objects, want 1 -- re-apply is an update", len(objs))
	}
	spec, err := objs[0].Repository()
	if err != nil {
		t.Fatal(err)
	}
	if spec.Branch != "develop" {
		t.Errorf("branch = %q, want the re-applied value", spec.Branch)
	}
	// Provenance follows the object to its new file.
	if objs[0].Metadata.Source != "/work/b.yaml" {
		t.Errorf("source = %q, want /work/b.yaml", objs[0].Metadata.Source)
	}
}

func TestGetMissing(t *testing.T) {
	s, _ := newStore(t)
	_, err := s.Get(object.KindRepository, "nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestListEmptyAndSorted(t *testing.T) {
	s, _ := newStore(t)
	objs, err := s.List(object.KindRepository)
	if err != nil {
		t.Fatalf("listing an empty store must not error: %v", err)
	}
	if len(objs) != 0 {
		t.Fatalf("got %d, want 0", len(objs))
	}

	for _, name := range []string{"web", "api", "worker"} {
		doc := strings.Replace(repoDoc, "name: api", "name: "+name, 1)
		if err := s.Put(parseOne(t, "f.yaml", doc)); err != nil {
			t.Fatal(err)
		}
	}
	objs, err = s.List(object.KindRepository)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{objs[0].Metadata.Name, objs[1].Metadata.Name, objs[2].Metadata.Name}
	want := []string{"api", "web", "worker"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("list = %v, want %v", got, want)
		}
	}
}

func TestDelete(t *testing.T) {
	s, _ := newStore(t)
	if err := s.Put(parseOne(t, "f.yaml", repoDoc)); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(object.KindRepository, "api"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(object.KindRepository, "api"); !errors.Is(err, ErrNotFound) {
		t.Errorf("object survived delete: %v", err)
	}
	// Deleting a name that is not there must not look like success.
	if err := s.Delete(object.KindRepository, "api"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete = %v, want ErrNotFound", err)
	}
}

// A name is a path segment here, so the store re-checks it rather than
// trusting the caller.
func TestStoreRefusesUnsafeNames(t *testing.T) {
	s, dataDir := newStore(t)
	for _, name := range []string{"../escape", "..", "a/b", "API"} {
		o := parseOne(t, "f.yaml", repoDoc)
		o.Metadata.Name = name
		if err := s.Put(o); err == nil {
			t.Errorf("Put accepted name %q", name)
		}
		if _, err := s.Get(object.KindRepository, name); err == nil {
			t.Errorf("Get accepted name %q", name)
		}
	}
	// Nothing escaped the objects tree.
	escaped := filepath.Join(filepath.Dir(dataDir), "escape.yaml")
	if _, err := os.Stat(escaped); err == nil {
		t.Fatalf("a file was written outside the data dir: %s", escaped)
	}
}

// Serve reads this tree at boot. A corrupt file must be a loud error, not a
// silent skip that leaves a clone with no object explaining it.
func TestCorruptStoredObjectIsAnError(t *testing.T) {
	s, dataDir := newStore(t)
	if err := s.Put(parseOne(t, "f.yaml", repoDoc)); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dataDir, "objects", "Repository", "api.yaml")
	if err := os.WriteFile(path, []byte("this: is: not: valid: yaml:\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Get(object.KindRepository, "api"); err == nil {
		t.Error("Get returned a corrupt object without error")
	}
	if _, err := s.List(object.KindRepository); err == nil {
		t.Error("List skipped a corrupt object instead of failing")
	}
}

// A hand-renamed file would otherwise give the store a name it cannot find
// again.
func TestFilenameMustMatchName(t *testing.T) {
	s, dataDir := newStore(t)
	if err := s.Put(parseOne(t, "f.yaml", repoDoc)); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(dataDir, "objects", "Repository")
	if err := os.Rename(filepath.Join(dir, "api.yaml"), filepath.Join(dir, "renamed.yaml")); err != nil {
		t.Fatal(err)
	}
	_, err := s.Get(object.KindRepository, "renamed")
	if err == nil {
		t.Fatal("a mismatched filename was accepted")
	}
	if !strings.Contains(err.Error(), "metadata.name") {
		t.Errorf("error = %q, want it to explain the mismatch", err)
	}
}

func TestSourcesInUse(t *testing.T) {
	s, _ := newStore(t)
	if err := s.Put(parseOne(t, "/work/a.yaml", repoDoc)); err != nil {
		t.Fatal(err)
	}
	web := strings.Replace(repoDoc, "name: api", "name: web", 1)
	if err := s.Put(parseOne(t, "/work/b.yaml", web)); err != nil {
		t.Fatal(err)
	}

	sources, err := s.SourcesInUse()
	if err != nil {
		t.Fatal(err)
	}
	if !sources["/work/a.yaml"] || !sources["/work/b.yaml"] {
		t.Errorf("sources = %v, want both files", sources)
	}

	if err := s.Delete(object.KindRepository, "api"); err != nil {
		t.Fatal(err)
	}
	sources, err = s.SourcesInUse()
	if err != nil {
		t.Fatal(err)
	}
	if sources["/work/a.yaml"] {
		t.Error("a.yaml is still listed after its only object was deleted")
	}
	if !sources["/work/b.yaml"] {
		t.Error("b.yaml should still be in use")
	}
}

func TestListAllSpansKinds(t *testing.T) {
	s, _ := newStore(t)
	env := `apiVersion: drover/v1
kind: Environment
metadata:
  name: stage
spec:
  variables:
    baseUrl: https://stage.example.com
`
	if err := s.Put(parseOne(t, "a.yaml", repoDoc)); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(parseOne(t, "b.yaml", env)); err != nil {
		t.Fatal(err)
	}
	objs, err := s.ListAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 2 {
		t.Fatalf("got %d objects, want 2", len(objs))
	}
}
