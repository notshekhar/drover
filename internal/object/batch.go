package object

import (
	"fmt"
	"sort"
	"strings"
)

// Batch is everything one `drover apply` invocation is about to write.
//
// Apply is all-or-nothing: the batch is assembled and checked in full before
// anything is written or cloned, so a bad document in the third file cannot
// leave the first two applied.
type Batch struct {
	Objects []*Object

	// seen maps identity to the document that claimed it first, which is what
	// lets a duplicate error name both sides.
	seen map[Ref]*Object
}

// NewBatch returns an empty batch.
func NewBatch() *Batch { return &Batch{seen: map[Ref]*Object{}} }

// Add appends one already-validated object, refusing a name this batch has
// already used for that kind.
//
// Two objects of the same kind can never share a name -- the name is the whole
// identity, and for a Repository it is also the checkout directory. Within one
// apply there is no defensible way to pick a winner, so this is an error
// rather than last-one-wins. Re-applying a name that exists in the *store* is
// a different thing entirely: that is an update, and it is fine.
func (b *Batch) Add(o *Object) error {
	if b.seen == nil {
		b.seen = map[Ref]*Object{}
	}
	ref := o.Ref()
	if prev, ok := b.seen[ref]; ok {
		return fmt.Errorf("duplicate %s: defined in %s and again in %s; two objects of the same kind cannot share a name",
			ref, prev.Where(), o.Where())
	}
	b.seen[ref] = o
	b.Objects = append(b.Objects, o)
	return nil
}

// AddAll adds each object in order, stopping at the first duplicate.
func (b *Batch) AddAll(objs []*Object) error {
	for _, o := range objs {
		if err := b.Add(o); err != nil {
			return err
		}
	}
	return nil
}

// Len is the number of objects in the batch.
func (b *Batch) Len() int { return len(b.Objects) }

// Unsupported returns the objects whose kind has no reconcile yet. The caller
// refuses these before writing anything, so an apply that would half-work
// fails cleanly instead.
func (b *Batch) Unsupported() []*Object {
	var out []*Object
	for _, o := range b.Objects {
		if !o.Kind.Implemented() {
			out = append(out, o)
		}
	}
	return out
}

// CheckLocal runs the whole-batch rules that need nothing beyond the batch
// itself. The client runs these so a typo fails without a round trip.
func (b *Batch) CheckLocal() error {
	if unsupported := b.Unsupported(); len(unsupported) > 0 {
		var parts []string
		for _, o := range unsupported {
			parts = append(parts, fmt.Sprintf("%s in %s", o.Ref(), o.Where()))
		}
		return fmt.Errorf("kind not supported yet: %s", strings.Join(parts, ", "))
	}
	return nil
}

// Check runs every rule, including the ones that need to know what the store
// already holds.
//
// Only the server can call this usefully: a request may reference an
// environment applied last week, which is invisible from the client. Passing
// an empty map here would reject exactly that case, which is why the client
// runs CheckLocal instead.
func (b *Batch) Check(existing map[Ref]bool) error {
	if err := b.CheckLocal(); err != nil {
		return err
	}
	return b.checkEnvironmentRefs(existing)
}

// checkEnvironmentRefs verifies every environment an HTTPRequest names.
//
// A request pointing at a stage that does not exist is a typo, not a feature:
// left alone it would surface as an unresolved {{baseUrl}} at call time, long
// after the person who wrote it moved on.
func (b *Batch) checkEnvironmentRefs(existing map[Ref]bool) error {
	known := map[string]bool{}
	for _, o := range b.Objects {
		if o.Kind == KindEnvironment {
			known[o.Metadata.Name] = true
		}
	}
	for ref := range existing {
		if ref.Kind == KindEnvironment {
			known[ref.Name] = true
		}
	}

	for _, o := range b.Objects {
		if o.Kind != KindHTTPRequest {
			continue
		}
		spec, err := o.HTTPRequest()
		if err != nil {
			return err
		}
		var missing []string
		for _, name := range spec.EnvironmentRefs() {
			if !known[name] {
				missing = append(missing, name)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			return fmt.Errorf("%s in %s names environment(s) that do not exist: %s (apply the Environment first, or in the same batch)",
				o.Ref(), o.Where(), strings.Join(missing, ", "))
		}
	}
	return nil
}

// Warnings are things worth saying out loud that are not errors.
//
// Two Repository objects pointing at the same url and branch is legal -- that
// is two checkouts, deliberately -- but it is also exactly what a copy-pasted
// document looks like, so it is worth a line on stderr. Likewise a SQLConnection
// url carrying a password works, but a credential in a file people commit is
// a leak waiting to happen, so it is called out.
func (b *Batch) Warnings() []string {
	type target struct{ url, branch string }
	byTarget := map[target][]string{}
	for _, o := range b.Objects {
		if o.Kind != KindRepository {
			continue
		}
		spec, err := o.Repository()
		if err != nil {
			continue
		}
		t := target{spec.URL, spec.Branch}
		byTarget[t] = append(byTarget[t], o.Metadata.Name)
	}

	var out []string
	for t, names := range byTarget {
		if len(names) < 2 {
			continue
		}
		sort.Strings(names)
		out = append(out, fmt.Sprintf("%d repositories clone %s branch %s: %s",
			len(names), t.url, t.branch, strings.Join(names, ", ")))
	}

	// An inline password is legal now, but it is still a credential sitting in
	// a file people commit, so it warns. ${ENV} references hide the password
	// from the file and never warn.
	for _, o := range b.Objects {
		if o.Kind != KindSQLConnection {
			continue
		}
		spec, err := o.SQLConnection()
		if err != nil {
			continue
		}
		if isSingleProcessEnvRef(strings.TrimSpace(spec.URL)) {
			continue
		}
		if err := checkNoInlinePassword(spec.URL); err != nil {
			out = append(out, fmt.Sprintf("%s: spec.url has a password in it; prefer a ${ENV_VAR} reference so the credential is not in a file people commit", o.Ref()))
		}
	}
	sort.Strings(out)
	return out
}
