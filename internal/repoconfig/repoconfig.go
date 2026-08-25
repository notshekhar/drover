// Package repoconfig reads the objects a repository declares about itself.
//
// A service knows which API it calls and where its docs are, and transcribing
// that into ~/.drover by hand is work nobody wants to do twice. So a
// repository may carry a .drover.yaml at its root and describe itself.
//
// And then it is quarantined, because a repository is remote content. A yaml
// file inside a clone is written by whoever can push to that repository,
// which is not necessarily the person running this engine. Applying it
// automatically would mean a pull request against a vendored dependency can
// reach into the engine. Nothing here is applied until the Repository says
// trustConfig: true.
package repoconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/notshekhar/drover/internal/object"
)

// FileName is what a repository calls its self-description.
const FileName = ".drover.yaml"

// MaxBytes caps the file before it is parsed. A repository can contain a
// 200 MB yaml file, and a parser is not the place to find that out.
const MaxBytes = 256 << 10

// allowedKinds are the kinds a repository may declare about itself.
//
// Repository and SQLConnection are absent, and permanently: a clone target
// and a database url are the two things that reach the network on drover's
// own credentials. Everything a repository can declare here is either inert
// on its own (Environment) or GET-only and jailed (HTTPRequest).
var allowedKinds = map[object.Kind]bool{
	object.KindEnvironment: true,
	object.KindHTTPRequest: true,
}

// Result is what one repository's self-description contained.
type Result struct {
	// Objects are ready to apply, with their names already namespaced. Empty
	// when the repository is not trusted -- see Pending.
	Objects []*object.Object

	// Pending is what would be applied if the repository were trusted. It is
	// always populated, so `drover review` can show the same list either way.
	Pending []*object.Object

	Trusted bool
	Skipped []string
	Path    string
}

// Summary is the line `drover get repository` prints.
func (r *Result) Summary() string {
	if r == nil || len(r.Pending) == 0 {
		return ""
	}
	if !r.Trusted {
		return fmt.Sprintf("%d object(s) in %s, not applied (set spec.trustConfig: true)", len(r.Pending), FileName)
	}
	return fmt.Sprintf("%d object(s) from %s", len(r.Objects), FileName)
}

// Read parses a checkout's self-description.
//
// A missing file is not an error and returns nil: most repositories will
// never have one.
func Read(checkout, repository string, trusted bool) (*Result, error) {
	path := filepath.Join(checkout, FileName)
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%s is a directory", FileName)
	}
	if info.Size() > MaxBytes {
		return nil, fmt.Errorf("%s is %d bytes; the limit is %d", FileName, info.Size(), MaxBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	objs, err := object.Parse(repository+"/"+FileName, data)
	if err != nil {
		return nil, err
	}

	res := &Result{Trusted: trusted, Path: path}
	for _, o := range objs {
		if !allowedKinds[o.Kind] {
			res.Skipped = append(res.Skipped, fmt.Sprintf("%s/%s: a repository may not declare a %s", o.Kind, o.Metadata.Name, o.Kind))
			continue
		}
		if strings.Contains(o.Metadata.Name, ".") {
			res.Skipped = append(res.Skipped, fmt.Sprintf("%s/%s: the dot is drover's namespace separator and this file does not get to set it", o.Kind, o.Metadata.Name))
			continue
		}
		if err := namespace(o, repository); err != nil {
			res.Skipped = append(res.Skipped, fmt.Sprintf("%s/%s: %v", o.Kind, o.Metadata.Name, err))
			continue
		}
		res.Pending = append(res.Pending, o)
	}
	sort.Slice(res.Pending, func(i, j int) bool {
		if res.Pending[i].Kind != res.Pending[j].Kind {
			return res.Pending[i].Kind < res.Pending[j].Kind
		}
		return res.Pending[i].Metadata.Name < res.Pending[j].Metadata.Name
	})

	if trusted {
		res.Objects = res.Pending
	}
	return res, nil
}

// namespace rewrites an object's name and the environments it references, so
// two repositories declaring "prod" do not collide and an object's origin is
// visible in the object.
//
// The label is generated rather than declared, under the reserved prefix, so
// `drover get httprequest -l drover.io/source=repository/api` works and
// nothing in the file could have forged it.
func namespace(o *object.Object, repository string) error {
	o.Metadata.Name = repository + "." + o.Metadata.Name
	if err := object.ValidateName(o.Metadata.Name); err != nil {
		return err
	}
	if o.Kind == object.KindHTTPRequest {
		if err := namespaceEnvironments(o, repository); err != nil {
			return err
		}
	}
	// Re-validated under its new name and references before the generated
	// label goes on. The label lives under the reserved prefix, which a
	// document may not write and Validate therefore refuses -- so it has to
	// be added on the far side of the check that proves the document did not
	// write one itself.
	if err := o.Validate(); err != nil {
		return err
	}
	if o.Metadata.Labels == nil {
		o.Metadata.Labels = map[string]string{}
	}
	o.Metadata.Labels[object.LabelPrefix+"source"] = "repository/" + repository
	return nil
}

// namespaceEnvironments rewrites a request's environment references to the
// namespaced names, since the Environments it means are the ones declared
// beside it in the same file.
func namespaceEnvironments(o *object.Object, repository string) error {
	spec, err := o.HTTPRequest()
	if err != nil {
		return err
	}
	changed := false
	for i, e := range spec.Environments {
		if !strings.Contains(e, ".") {
			spec.Environments[i] = repository + "." + e
			changed = true
		}
	}
	if spec.DefaultEnvironment != "" && !strings.Contains(spec.DefaultEnvironment, ".") {
		spec.DefaultEnvironment = repository + "." + spec.DefaultEnvironment
		changed = true
	}
	if !changed {
		return nil
	}
	return o.ReplaceSpec(spec)
}
