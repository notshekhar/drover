package importer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/notshekhar/drover/internal/object"
)

func writeCollection(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, body string) {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("collection.bru", `
vars {
  tenant: acme
}
`)
	write("environments/prod.bru", `
vars {
  baseUrl: https://api.acme.com
}
vars:secret [
  token
]
`)
	write("users/get user.bru", `
meta {
  name: Get User
  type: http
  seq: 1
}

get {
  url: {{baseUrl}}/v1/users/:userId?verbose=true
  body: none
  auth: none
}

params:path {
  ~disabled: nope
  userId: usr_1a2b3c
}

params:query {
  verbose: true
}

headers {
  Accept: application/json
  ~X-Debug: 1
}

docs {
  Fetch one user by id.
}
`)
	write("issues/create issue.bru", `
meta {
  name: Create Issue
}

post {
  url: {{baseUrl}}/issues
}
`)
	return dir
}

func TestBrunoImportsACollection(t *testing.T) {
	res, err := Bruno(writeCollection(t), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Requests != 2 {
		t.Fatalf("imported %d requests, want 2 (skipped: %v)\n%s", res.Requests, res.Skipped, res.Documents)
	}

	objs, err := object.Parse("generated.yaml", res.Documents)
	if err != nil {
		t.Fatalf("the generated yaml does not apply: %v\n%s", err, res.Documents)
	}
	byName := map[string]*object.Object{}
	for _, o := range objs {
		byName[o.Metadata.Name] = o
	}

	got, ok := byName["get-user"]
	if !ok {
		t.Fatalf("no get-user; got %v", keysOf(byName))
	}
	spec, err := got.HTTPRequest()
	if err != nil {
		t.Fatal(err)
	}

	// Bruno writes :id for a path segment; drover writes {id}, because {id}
	// is the only syntax that becomes a tool parameter. And the query string
	// has to come off the url, or it would be sent twice.
	if spec.URL != "{{baseUrl}}/v1/users/{userId}" {
		t.Errorf("url is %q", spec.URL)
	}
	if len(spec.PathParams) != 1 || spec.PathParams[0].Name != "userId" {
		t.Errorf("path params are %+v", spec.PathParams)
	}
	if spec.PathParams[0].Example != "usr_1a2b3c" {
		t.Errorf("the example value was lost: %+v", spec.PathParams[0])
	}
	if len(spec.Query) != 1 || spec.Query[0].Name != "verbose" {
		t.Errorf("query params are %+v", spec.Query)
	}

	// A ~ prefix means the row is disabled in Bruno. Importing it would send
	// a header the person had deliberately switched off.
	for _, h := range spec.Headers {
		if h.Name == "X-Debug" {
			t.Error("a disabled header was imported")
		}
	}
	for _, p := range spec.PathParams {
		if p.Name == "disabled" {
			t.Error("a disabled path parameter was imported")
		}
	}
	if spec.Description != "Fetch one user by id." {
		t.Errorf("docs did not become the description: %q", spec.Description)
	}

	// A non-GET is stored, and drover's own rules keep it unadvertised.
	post, _ := byName["create-issue"].HTTPRequest()
	if post.Method != "POST" || post.IsSafe() {
		t.Errorf("create-issue is %s, safe=%v", post.Method, post.IsSafe())
	}
}

func TestBrunoEnvironmentMergesCollectionVars(t *testing.T) {
	res, err := Bruno(writeCollection(t), Options{})
	if err != nil {
		t.Fatal(err)
	}
	doc := string(res.Documents)
	if !strings.Contains(doc, "baseUrl: https://api.acme.com") {
		t.Errorf("the environment did not carry baseUrl:\n%s", doc)
	}
	if !strings.Contains(doc, "tenant: acme") {
		t.Errorf("the collection's own vars did not merge in:\n%s", doc)
	}
	// Bruno's secret vars hold literal values, and drover refuses a literal
	// in secrets. Emitting one would produce a document that cannot apply.
	if strings.Contains(doc, "secrets:") {
		t.Errorf("a secrets block was emitted:\n%s", doc)
	}
}

func TestBrunoNeedsADirectory(t *testing.T) {
	f := filepath.Join(t.TempDir(), "x.bru")
	if err := os.WriteFile(f, []byte("meta {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Bruno(f, Options{}); err == nil {
		t.Fatal("a single file was accepted as a collection")
	}
}
