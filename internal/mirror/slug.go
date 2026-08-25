// Package mirror keeps the discussion around a checkout -- issues and pull
// requests -- on disk beside it, as markdown.
//
// git says what changed and who changed it. It cannot say why: why was
// argued in a pull request and filed in an issue, and neither is in the
// clone. This package closes that gap without adding a tool, because
// everything it writes is a file the existing ls/read/grep/find already reach.
package mirror

import (
	"fmt"
	"net/url"
	"strings"
)

// Slug is an owner/repository pair on a GitHub host.
type Slug struct {
	Host  string
	Owner string
	Name  string
}

func (s Slug) String() string { return s.Owner + "/" + s.Name }

// ParseSlug reads owner and repository out of a git remote URL.
//
// Every form git itself accepts has to work here, because the url in the
// document was written for git, not for this: https, ssh, and scp-style
// git@host:owner/name.
func ParseSlug(raw string) (Slug, error) {
	raw = strings.TrimSpace(raw)

	// scp-style, which is not a URL and never parses as one.
	if !strings.Contains(raw, "://") {
		if at := strings.Index(raw, "@"); at >= 0 {
			rest := raw[at+1:]
			host, path, ok := strings.Cut(rest, ":")
			if ok {
				return slugFrom(host, path)
			}
		}
		return Slug{}, fmt.Errorf("%q is not a URL drover can mirror from", raw)
	}

	u, err := url.Parse(raw)
	if err != nil {
		return Slug{}, fmt.Errorf("%q: %w", raw, err)
	}
	return slugFrom(u.Host, u.Path)
}

func slugFrom(host, path string) (Slug, error) {
	if at := strings.Index(host, "@"); at >= 0 {
		host = host[at+1:]
	}
	host = strings.TrimSuffix(host, ":443")
	path = strings.Trim(path, "/")
	path = strings.TrimSuffix(path, ".git")

	parts := strings.Split(path, "/")
	if len(parts) < 2 || parts[0] == "" || parts[len(parts)-1] == "" {
		return Slug{}, fmt.Errorf("%q does not name an owner and a repository", path)
	}
	// A GitHub path is exactly owner/name; anything deeper is not one.
	if len(parts) != 2 {
		return Slug{}, fmt.Errorf("%q is not an owner/repository path", path)
	}
	if host == "" {
		return Slug{}, fmt.Errorf("no host in the repository url")
	}
	return Slug{Host: host, Owner: parts[0], Name: parts[1]}, nil
}

// IsGitHub reports whether this host is one the GitHub API speaks for.
//
// GitHub Enterprise lives on its own hostname and its API sits under
// /api/v3, which the fetcher handles; what this rules out is GitLab and
// Bitbucket, whose APIs are a different shape entirely and which get a clear
// error rather than a confusing 404.
func (s Slug) IsGitHub() bool {
	return s.Host == "github.com" || strings.HasPrefix(s.Host, "github.")
}
