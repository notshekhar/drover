// Package store keeps applied objects on disk under the data directory.
//
// This is the point of apply: a document is written to
// $DROVER_DATA/objects/<Kind>/<name>.yaml before the apply reports success,
// and serve reads that tree on boot. The client's yaml files are how state
// got here, not where it lives -- delete them and the engine still knows what
// it holds.
package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/notshekhar/drover/internal/atomicfile"
	"github.com/notshekhar/drover/internal/object"
)

// ErrNotFound is returned when an object is not in the store.
var ErrNotFound = errors.New("not found")

// Store is the objects tree under the data directory.
type Store struct {
	dir string // <data>/objects
}

// New opens the store rooted at the data directory.
func New(dataDir string) (*Store, error) {
	dir := filepath.Join(dataDir, "objects")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}
	return &Store{dir: dir}, nil
}

// Dir is the objects directory.
func (s *Store) Dir() string { return s.dir }

func (s *Store) path(kind object.Kind, name string) string {
	return filepath.Join(s.dir, string(kind), name+".yaml")
}

// Put writes one object, creating or replacing it.
//
// The name is validated again here even though parsing already did it. This
// is the layer that turns a name into a filesystem path, so it refuses to
// trust its caller.
func (s *Store) Put(o *object.Object) error {
	if err := object.ValidateName(o.Metadata.Name); err != nil {
		return fmt.Errorf("metadata.name: %w", err)
	}
	if o.Kind.Plural() == "" {
		return fmt.Errorf("unknown kind %q", o.Kind)
	}

	stored := *o
	stored.Metadata.Source = o.Source
	stored.Metadata.AppliedAt = time.Now().UTC().Format(time.RFC3339)

	data, err := yaml.Marshal(&stored)
	if err != nil {
		return fmt.Errorf("marshal %s: %w", o.Ref(), err)
	}
	header := fmt.Sprintf("# written by drover on apply -- edit the source and re-apply, not this file\n# source: %s\n", sourceOrStdin(o.Source))

	if err := atomicfile.Write(s.path(o.Kind, o.Metadata.Name), append([]byte(header), data...), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", o.Ref(), err)
	}
	return nil
}

func sourceOrStdin(src string) string {
	if src == "" {
		return "stdin"
	}
	return src
}

// Get reads one object.
func (s *Store) Get(kind object.Kind, name string) (*object.Object, error) {
	if err := object.ValidateName(name); err != nil {
		return nil, fmt.Errorf("metadata.name: %w", err)
	}
	path := s.path(kind, name)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%s: %w", object.Ref{Kind: kind, Name: name}, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	return decodeStored(path, data)
}

// List reads every object of one kind, sorted by name.
func (s *Store) List(kind object.Kind) ([]*object.Object, error) {
	dir := filepath.Join(s.dir, string(kind))
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var out []*object.Object
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		o, err := decodeStored(path, data)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Metadata.Name < out[j].Metadata.Name })
	return out, nil
}

// ListAll reads every object of every kind.
func (s *Store) ListAll() ([]*object.Object, error) {
	var out []*object.Object
	for _, k := range object.Kinds {
		objs, err := s.List(k)
		if err != nil {
			return nil, err
		}
		out = append(out, objs...)
	}
	return out, nil
}

// Delete removes one object. Deleting something that is not there is an
// error, so a typo in a name does not look like it worked.
func (s *Store) Delete(kind object.Kind, name string) error {
	if err := object.ValidateName(name); err != nil {
		return fmt.Errorf("metadata.name: %w", err)
	}
	err := os.Remove(s.path(kind, name))
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s: %w", object.Ref{Kind: kind, Name: name}, ErrNotFound)
	}
	return err
}

// decodeStored parses a file this store wrote.
//
// A file that does not parse is a hard error rather than a skip. Skipping
// would mean an engine that quietly holds less than the user applied, and the
// clone on disk would outlive the object that explains it.
func decodeStored(path string, data []byte) (*object.Object, error) {
	objs, err := object.Parse(path, data)
	if err != nil {
		return nil, fmt.Errorf("stored object is unreadable: %w", err)
	}
	if len(objs) != 1 {
		return nil, fmt.Errorf("%s: holds %d documents, want exactly 1", path, len(objs))
	}
	o := objs[0]

	// Parse clears provenance because it does not trust incoming documents.
	// Here the document is ours, so read it back out of the file.
	var stored struct {
		Metadata struct {
			Source    string `yaml:"source"`
			AppliedAt string `yaml:"appliedAt"`
		} `yaml:"metadata"`
	}
	if err := yaml.Unmarshal(data, &stored); err == nil {
		o.Metadata.Source = stored.Metadata.Source
		o.Metadata.AppliedAt = stored.Metadata.AppliedAt
		o.Source = stored.Metadata.Source
	}

	// The name in the file wins over the filename, but they must agree --
	// disagreement means someone renamed a file by hand and the store would
	// otherwise answer to a name it cannot find again.
	want := strings.TrimSuffix(filepath.Base(path), ".yaml")
	if o.Metadata.Name != want {
		return nil, fmt.Errorf("%s: holds %s, but the filename says %q; rename the file or fix metadata.name", path, o.Ref(), want)
	}
	return o, nil
}

// SourcesInUse returns the set of source paths that stored objects still
// point at. Delete uses it to decide whether an apply: entry is now dead.
func (s *Store) SourcesInUse() (map[string]bool, error) {
	objs, err := s.ListAll()
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, o := range objs {
		if o.Metadata.Source != "" {
			out[o.Metadata.Source] = true
		}
	}
	return out, nil
}
