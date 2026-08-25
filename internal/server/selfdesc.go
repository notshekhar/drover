package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/notshekhar/drover/internal/atomicfile"
	"github.com/notshekhar/drover/internal/object"
	"github.com/notshekhar/drover/internal/repoconfig"
)

// selfDescriber applies what a checkout says about itself, and quarantines it
// when the repository is not trusted.
type selfDescriber struct{ s *Server }

// pendingDir holds what an untrusted repository would have applied.
//
// Deliberately outside the file jail. It is for a person to read before
// deciding to trust a repository, and putting attacker-supplied documents
// where an agent will grep them would defeat half the point of quarantining
// them in the first place.
func (s *Server) pendingDir() string { return filepath.Join(s.opts.DataDir, "pending") }

func (d *selfDescriber) Apply(ctx context.Context, name string, spec *object.RepositorySpec) (string, error) {
	res, err := repoconfig.Read(d.s.repo.Path(name), name, spec.TrustConfig)
	if err != nil {
		return "", err
	}
	pending := filepath.Join(d.s.pendingDir(), name+".yaml")
	if res == nil || len(res.Pending) == 0 {
		// The file went away, or declared nothing usable. Anything previously
		// quarantined from it is stale and misleading.
		_ = os.Remove(pending)
		if res != nil && len(res.Skipped) > 0 {
			return "", fmt.Errorf("%s", strings.Join(res.Skipped, "; "))
		}
		return "", nil
	}

	for _, s := range res.Skipped {
		d.s.logf("repository %s: %s: skipped %s", name, repoconfig.FileName, s)
	}

	if !res.Trusted {
		if err := d.writePending(pending, name, res); err != nil {
			return "", err
		}
		return res.Summary(), nil
	}
	_ = os.Remove(pending)

	// Applied one at a time rather than as a batch: an Environment declared
	// beside a request has to land before the request that references it, and
	// a single bad document in somebody else's repository should not stop the
	// rest from being useful.
	applied := 0
	var failures []string
	for _, o := range sortEnvironmentsFirst(res.Objects) {
		if err := d.s.store.Put(o); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", o.Ref(), err))
			continue
		}
		d.s.objectChanged(o)
		applied++
	}
	if len(failures) > 0 {
		return fmt.Sprintf("%d of %d object(s) from %s", applied, len(res.Objects), repoconfig.FileName),
			fmt.Errorf("%s", strings.Join(failures, "; "))
	}
	return res.Summary(), nil
}

// sortEnvironmentsFirst puts Environments ahead of the requests that name
// them, since apply checks that a referenced environment exists.
func sortEnvironmentsFirst(objs []*object.Object) []*object.Object {
	out := make([]*object.Object, 0, len(objs))
	for _, o := range objs {
		if o.Kind == object.KindEnvironment {
			out = append(out, o)
		}
	}
	for _, o := range objs {
		if o.Kind != object.KindEnvironment {
			out = append(out, o)
		}
	}
	return out
}

// writePending stores what would be applied, for a person to read.
func (d *selfDescriber) writePending(path, name string, res *repoconfig.Result) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s from the %s repository, NOT applied.\n", repoconfig.FileName, name)
	fmt.Fprintf(&b, "# A repository's yaml is written by whoever can push to it, so drover\n")
	fmt.Fprintf(&b, "# will not apply it until the Repository says trustConfig: true.\n")
	fmt.Fprintf(&b, "# Read it here first: drover review %s\n", name)
	for _, o := range res.Pending {
		data, err := yaml.Marshal(o)
		if err != nil {
			return err
		}
		b.WriteString("---\n")
		b.Write(data)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return atomicfile.Write(path, []byte(b.String()), 0o600)
}
