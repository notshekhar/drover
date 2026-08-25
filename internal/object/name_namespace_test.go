package object

import (
	"strings"
	"testing"
)

func repositoryDoc(name string) string {
	return strings.ReplaceAll(`apiVersion: drover/v1
kind: Repository
metadata:
  name: NAME
spec:
  url: https://github.com/acme/api
  branch: main
`, "NAME", name)
}

// A repository's own name is a directory and a top-level path segment, so it
// keeps the stricter rule even though other kinds may carry a namespace dot.
func TestRepositoryNameMayNotBeNamespaced(t *testing.T) {
	if _, err := Parse("r.yaml", []byte(repositoryDoc("api.v2"))); err == nil {
		t.Fatal("a repository took a namespaced name")
	}
	if _, err := Parse("r.yaml", []byte(repositoryDoc("api-v2"))); err != nil {
		t.Fatalf("an ordinary repository name was refused: %v", err)
	}
}

// A reserved root name would shadow a top-level directory the file tools use.
func TestRepositoryNameMayNotShadowARoot(t *testing.T) {
	for _, name := range ReservedNames {
		if _, err := Parse("r.yaml", []byte(repositoryDoc(name))); err == nil {
			t.Errorf("a repository took the reserved name %q", name)
		}
	}
}
