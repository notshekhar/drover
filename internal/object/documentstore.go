package object

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// DocumentStoreSpec is a place an agent may write.
//
// Every other kind drover has is desired state it converges something onto: a
// checkout onto a ref, a pool onto a url. A document is content -- there is
// nothing to reconcile. So the store is an object and the documents are
// files, and there is deliberately no way to apply a document as yaml: a PRD
// wrapped in a block scalar comes back reflowed, and grepping it returns the
// wrapper instead of the prose.
type DocumentStoreSpec struct {
	// Description goes in the connection inventory and in the write tool's
	// catalogue, so a model is told "product is where PRDs live" rather than
	// inferring it from the name.
	Description string `yaml:"description,omitempty"`

	// Path points the store at a directory that already exists. Unset means
	// $DROVER_DATA/documents/<name>, which is the zero-config case.
	Path string `yaml:"path,omitempty"`

	// Writable defaults to true: a store exists to be written to. Setting it
	// false makes a store an agent can read and cannot change, which is what
	// you want for a directory of documents that is somebody else's.
	Writable *bool `yaml:"writable,omitempty"`

	// History keeps a local git repository inside the store, committing every
	// agent write with its attribution and its stated reason. Default true.
	// It is not a remote and drover never pushes; it is undo, and an answer
	// to "who wrote this and why".
	History *bool `yaml:"history,omitempty"`
}

// DocumentStore decodes this object's spec as a DocumentStoreSpec.
func (o *Object) DocumentStore() (*DocumentStoreSpec, error) {
	if o.Kind != KindDocumentStore {
		return nil, fmt.Errorf("object is %s, not %s", o.Kind, KindDocumentStore)
	}
	var spec DocumentStoreSpec
	if err := o.decodeSpec(&spec); err != nil {
		return nil, err
	}
	return &spec, nil
}

// IsWritable reports whether an agent may write here.
func (s *DocumentStoreSpec) IsWritable() bool { return s.Writable == nil || *s.Writable }

// KeepsHistory reports whether writes are committed locally.
func (s *DocumentStoreSpec) KeepsHistory() bool { return s.History == nil || *s.History }

// Validate checks what can be known without touching the disk.
func (s *DocumentStoreSpec) Validate() error {
	if p := strings.TrimSpace(s.Path); p != "" {
		if strings.HasPrefix(p, "~") {
			return errors.New("spec.path: write the path out in full; ~ is the shell's, and drover is not a shell")
		}
		if !filepath.IsAbs(p) {
			return fmt.Errorf("spec.path: %q is relative, and drover has no working directory to resolve it against", p)
		}
	}
	return nil
}
