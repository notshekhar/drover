package object

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// RepositorySpec is one git repository drover keeps checked out.
type RepositorySpec struct {
	URL    string `yaml:"url"`
	Branch string `yaml:"branch"`

	// RefreshInterval is per repository, so a monorepo someone is actively
	// working in can pull every few minutes while a vendored reference repo
	// sits at a day. Unset means the server default.
	RefreshInterval Interval `yaml:"refreshInterval"`
}

// Repository decodes this object's spec as a RepositorySpec.
func (o *Object) Repository() (*RepositorySpec, error) {
	if o.Kind != KindRepository {
		return nil, fmt.Errorf("object is %s, not %s", o.Kind, KindRepository)
	}
	var spec RepositorySpec
	if err := o.decodeSpec(&spec); err != nil {
		return nil, err
	}
	return &spec, nil
}

// Validate checks the fields a clone actually needs.
func (s *RepositorySpec) Validate() error {
	if strings.TrimSpace(s.URL) == "" {
		return errors.New("spec.url is required")
	}
	if err := validateRepoURL(s.URL); err != nil {
		return fmt.Errorf("spec.url: %w", err)
	}
	if strings.TrimSpace(s.Branch) == "" {
		return errors.New("spec.branch is required (drover clones one branch, so there is no sensible default)")
	}
	if err := validateBranch(s.Branch); err != nil {
		return fmt.Errorf("spec.branch: %w", err)
	}
	return nil
}

// validateRepoURL accepts the forms git itself takes: a URL with a scheme, or
// scp-style user@host:path. It is a sanity check, not an existence check --
// whether the remote is reachable is reconcile's problem, not parsing's.
func validateRepoURL(raw string) error {
	if strings.HasPrefix(raw, "-") {
		return fmt.Errorf("%q starts with a dash, which git would read as a flag", raw)
	}
	if strings.ContainsAny(raw, " \t\n") {
		return fmt.Errorf("%q contains whitespace", raw)
	}

	if u, err := url.Parse(raw); err == nil && u.Scheme != "" {
		switch u.Scheme {
		case "http", "https", "ssh", "git", "file":
			if u.Scheme != "file" && u.Host == "" {
				return fmt.Errorf("%q has no host", raw)
			}
			return nil
		default:
			return fmt.Errorf("unsupported scheme %q (use https, ssh, git or file)", u.Scheme)
		}
	}

	// scp-style: git@github.com:acme/api.git
	if at := strings.Index(raw, "@"); at > 0 {
		if colon := strings.Index(raw[at:], ":"); colon > 0 {
			return nil
		}
	}

	// An absolute local path is a real git remote -- `git clone /srv/git/api`
	// works, and a local mirror is a reasonable thing to point drover at.
	// Whether it exists is reconcile's problem, not parsing's.
	if strings.HasPrefix(raw, "/") {
		return nil
	}

	return fmt.Errorf("%q is not a git URL (want https://host/path, ssh://…, git@host:path, or an absolute local path)", raw)
}

// validateBranch rejects the shapes git refuses, so a bad branch surfaces at
// apply rather than as a clone failure an hour later.
func validateBranch(b string) error {
	switch {
	case strings.HasPrefix(b, "-"):
		return fmt.Errorf("%q starts with a dash", b)
	case strings.HasPrefix(b, "/"), strings.HasSuffix(b, "/"):
		return fmt.Errorf("%q may not start or end with a slash", b)
	case strings.HasSuffix(b, ".lock"):
		return fmt.Errorf("%q may not end with .lock", b)
	case strings.Contains(b, ".."), strings.Contains(b, "//"):
		return fmt.Errorf("%q contains .. or //", b)
	case strings.ContainsAny(b, " \t\n~^:?*[\\"):
		return fmt.Errorf("%q contains a character git does not allow in a ref name", b)
	}
	return nil
}
