// Package object parses and validates drover documents.
//
// It is pure: no git, no HTTP, no filesystem beyond what the caller hands it.
// Everything in here can be tested offline.
package object

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"gopkg.in/yaml.v3"
)

// APIVersion is the only apiVersion drover accepts.
const APIVersion = "drover/v1"

// Kind is the type of a document. Kinds are spelled out in full: there are no
// short forms and no aliases, so a document says Repository, never Repo.
type Kind string

const (
	KindRepository    Kind = "Repository"
	KindEnvironment   Kind = "Environment"
	KindHTTPRequest   Kind = "HTTPRequest"
	KindSQLConnection Kind = "SQLConnection"
)

// Kinds is every kind drover knows, in the order they should be listed.
var Kinds = []Kind{KindRepository, KindEnvironment, KindHTTPRequest, KindSQLConnection}

// Implemented reports whether this kind can be applied. Every kind is now
// built; the method stays because it is where a future kind lands as
// parse-only before its reconcile exists.
func (k Kind) Implemented() bool { return true }

// Plural is the path segment used in routes: /apis/drover/v1/repositories.
func (k Kind) Plural() string {
	switch k {
	case KindRepository:
		return "repositories"
	case KindEnvironment:
		return "environments"
	case KindHTTPRequest:
		return "httprequests"
	case KindSQLConnection:
		return "sqlconnections"
	}
	return ""
}

// ParseKind resolves a user-typed kind. It accepts any case and the plural,
// because "drover get repositories" reads better than the singular. It does
// not accept abbreviations -- "repo" is an error, on purpose.
func ParseKind(s string) (Kind, error) {
	norm := lower(s)
	for _, k := range Kinds {
		if norm == lower(string(k)) || norm == k.Plural() {
			return k, nil
		}
	}
	return "", fmt.Errorf("unknown kind %q (kinds are spelled in full: repository, environment, httprequest, sqlconnection)", s)
}

func lower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// Meta is the metadata block. Identity is (Kind, Name) and nothing else --
// there are no namespaces.
type Meta struct {
	Name string `yaml:"name"`

	// Source and AppliedAt are written by the store, never by the user. They
	// are what lets `drover get` say where an object came from, and what lets
	// delete work out whether an apply: path still accounts for anything. A
	// document that arrives with them set has them cleared, so provenance
	// always reflects the apply that actually happened.
	Source    string `yaml:"source,omitempty"`
	AppliedAt string `yaml:"appliedAt,omitempty"`
}

// Object is one parsed document.
type Object struct {
	APIVersion string    `yaml:"apiVersion"`
	Kind       Kind      `yaml:"kind"`
	Metadata   Meta      `yaml:"metadata"`
	Spec       yaml.Node `yaml:"spec"`

	// Source is where this document came from: an absolute file path, or
	// "stdin". It is provenance only and is never part of identity.
	Source string `yaml:"-"`
	// Index is the 0-based position of this document within its source, so a
	// multi-doc file can point at the offending document.
	Index int `yaml:"-"`

	// allowInlinePasswords is set by the Parse option of the same name. It is
	// validation policy, never part of the document.
	allowInlinePasswords bool
}

// ParseOption tweaks how Parse validates. The zero options are the secure
// defaults.
type ParseOption func(*parseOptions)

type parseOptions struct {
	allowInlinePasswords bool
}

// AllowInlinePasswords accepts a password inside a SQLConnection url.
//
// The default rejects one, because a credential sitting in a file people
// commit is a leak waiting to happen. Passing this says the caller knows the
// file it is applying is not a shared repository.
func AllowInlinePasswords() ParseOption {
	return func(o *parseOptions) { o.allowInlinePasswords = true }
}

// Ref is the identity of an object, and how it is named in errors.
type Ref struct {
	Kind Kind
	Name string
}

func (r Ref) String() string { return string(r.Kind) + "/" + r.Name }

// Ref returns this object's identity.
func (o *Object) Ref() Ref { return Ref{Kind: o.Kind, Name: o.Metadata.Name} }

// Where names the document for an error message: the source, plus the
// document index when the source held more than one.
func (o *Object) Where() string {
	if o.Index > 0 {
		return fmt.Sprintf("%s (document %d)", o.Source, o.Index+1)
	}
	return o.Source
}

// Parse reads every YAML document in data. Documents are separated by "---";
// empty documents are skipped so a trailing separator is not an error.
//
// Each document is validated on its own. Parse stops at the first invalid
// document, because apply is all-or-nothing anyway and a wall of errors from
// one bad indent helps nobody.
func Parse(source string, data []byte, opts ...ParseOption) ([]*Object, error) {
	var po parseOptions
	for _, opt := range opts {
		opt(&po)
	}

	dec := yaml.NewDecoder(bytes.NewReader(data))
	var out []*Object
	for i := 0; ; i++ {
		var node yaml.Node
		err := dec.Decode(&node)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s: document %d: %w", source, i+1, err)
		}
		if isEmptyDoc(&node) {
			continue // empty document, e.g. a trailing "---"
		}

		obj := &Object{}
		if err := node.Decode(obj); err != nil {
			return nil, fmt.Errorf("%s: document %d: %w", source, i+1, err)
		}
		obj.Source, obj.Index = source, i
		obj.allowInlinePasswords = po.allowInlinePasswords
		// Provenance is the server's to write, not the document's.
		obj.Metadata.Source, obj.Metadata.AppliedAt = "", ""
		if err := obj.Validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", obj.Where(), err)
		}
		out = append(out, obj)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: no documents", source)
	}
	return out, nil
}

// Validate checks the envelope and then hands off to the kind's own spec.
func (o *Object) Validate() error {
	if o.APIVersion == "" {
		return errors.New("apiVersion is required")
	}
	if o.APIVersion != APIVersion {
		return fmt.Errorf("unsupported apiVersion %q (want %q)", o.APIVersion, APIVersion)
	}
	if o.Kind == "" {
		return errors.New("kind is required")
	}
	known := false
	for _, k := range Kinds {
		if o.Kind == k {
			known = true
			break
		}
	}
	if !known {
		return fmt.Errorf("unknown kind %q (kinds are spelled in full: Repository, Environment, HTTPRequest, SQLConnection)", o.Kind)
	}
	if err := ValidateName(o.Metadata.Name); err != nil {
		return fmt.Errorf("metadata.name: %w", err)
	}

	switch o.Kind {
	case KindRepository:
		spec, err := o.Repository()
		if err != nil {
			return err
		}
		return spec.Validate()
	case KindEnvironment:
		spec, err := o.Environment()
		if err != nil {
			return err
		}
		return spec.Validate()
	case KindHTTPRequest:
		spec, err := o.HTTPRequest()
		if err != nil {
			return err
		}
		return spec.Validate()
	case KindSQLConnection:
		spec, err := o.SQLConnection()
		if err != nil {
			return err
		}
		spec.allowInlinePasswords = o.allowInlinePasswords
		return spec.Validate()
	}
	return nil
}

// decodeSpec pulls the spec block into v, refusing fields the schema does not
// know so a typo is an error instead of a silently ignored setting.
func (o *Object) decodeSpec(v any) error {
	if o.Spec.Kind == 0 {
		return errors.New("spec is required")
	}
	// yaml.Node.Decode has no strict mode, so round-trip through a decoder
	// that does. An unknown field is nearly always a typo, and silently
	// ignoring it means the setting the user wrote never takes effect.
	raw, err := yaml.Marshal(&o.Spec)
	if err != nil {
		return fmt.Errorf("spec: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("spec: %w", err)
	}
	return nil
}

// isEmptyDoc reports whether a decoded document carries nothing. A trailing
// "---" is the common case: YAML reads it as a document holding null, which is
// a separator the writer left behind, not a document that forgot its fields.
func isEmptyDoc(node *yaml.Node) bool {
	switch {
	case node.Kind == 0:
		return true
	case node.Kind == yaml.DocumentNode:
		return len(node.Content) == 0 || isEmptyDoc(node.Content[0])
	case node.Kind == yaml.ScalarNode:
		return node.Tag == "!!null"
	}
	return false
}
