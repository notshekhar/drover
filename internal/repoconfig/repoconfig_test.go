package repoconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/notshekhar/drover/internal/object"
)

func checkout(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if body != "" {
		if err := os.WriteFile(filepath.Join(dir, FileName), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const selfDescription = `apiVersion: drover/v1
kind: Environment
metadata:
  name: prod
spec:
  variables:
    baseUrl: https://api.acme.com
---
apiVersion: drover/v1
kind: HTTPRequest
metadata:
  name: get-user
spec:
  description: Fetch one user.
  method: GET
  url: "{{baseUrl}}/v1/users/{userId}"
  environments: [prod]
  defaultEnvironment: prod
  pathParams:
    - name: userId
      description: The user id.
      required: true
`

func TestMissingFileIsNotAnError(t *testing.T) {
	res, err := Read(checkout(t, ""), "api", true)
	if err != nil || res != nil {
		t.Fatalf("res=%v err=%v", res, err)
	}
}

// Names are namespaced so two repositories cannot fight over one name, and so
// an object's origin is visible in the object.
func TestObjectsAreNamespaced(t *testing.T) {
	res, err := Read(checkout(t, selfDescription), "api", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Objects) != 2 {
		t.Fatalf("got %d objects (skipped %v)", len(res.Objects), res.Skipped)
	}
	names := map[string]bool{}
	for _, o := range res.Objects {
		names[o.Metadata.Name] = true
		if got := o.Metadata.Labels[object.LabelPrefix+"source"]; got != "repository/api" {
			t.Errorf("%s carries source label %q", o.Metadata.Name, got)
		}
	}
	if !names["api.prod"] || !names["api.get-user"] {
		t.Fatalf("names are %v", names)
	}

	// A request's environment reference must follow its environment into the
	// namespace, or it points at whatever "prod" means engine-wide.
	for _, o := range res.Objects {
		if o.Kind != object.KindHTTPRequest {
			continue
		}
		spec, err := o.HTTPRequest()
		if err != nil {
			t.Fatal(err)
		}
		if spec.DefaultEnvironment != "api.prod" || spec.Environments[0] != "api.prod" {
			t.Errorf("environments were not namespaced: %+v", spec.Environments)
		}
	}
}

// The security decision of this feature: a repository's yaml is written by
// whoever can push to that repository.
func TestUntrustedIsParsedAndInert(t *testing.T) {
	res, err := Read(checkout(t, selfDescription), "api", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Objects) != 0 {
		t.Fatalf("an untrusted repository produced %d applicable objects", len(res.Objects))
	}
	if len(res.Pending) != 2 {
		t.Fatalf("pending was %d, want 2", len(res.Pending))
	}
	if !strings.Contains(res.Summary(), "trustConfig") {
		t.Errorf("the summary does not say how to trust it: %q", res.Summary())
	}
}

// A clone target and a database url are the two things that reach the network
// on drover's own credentials. Neither may come from a repository, ever.
func TestDangerousKindsAreRefused(t *testing.T) {
	for _, doc := range []string{
		`apiVersion: drover/v1
kind: Repository
metadata: {name: evil}
spec:
  url: https://github.com/attacker/huge
  branch: main
`,
		`apiVersion: drover/v1
kind: SQLConnection
metadata: {name: evil}
spec:
  provider: postgres
  url: ${DATABASE_URL}
  health: SELECT 1
`,
	} {
		res, err := Read(checkout(t, doc), "api", true)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Pending) != 0 {
			t.Errorf("a repository declared %s and it was accepted", res.Pending[0].Kind)
		}
		if len(res.Skipped) != 1 {
			t.Errorf("the refusal was silent: %v", res.Skipped)
		}
	}
}

// A repository can contain a 200 MB yaml file. A parser is not the place to
// find that out.
func TestOversizedFileIsRefusedBeforeParsing(t *testing.T) {
	dir := checkout(t, "")
	big := make([]byte, MaxBytes+1)
	for i := range big {
		big[i] = 'a'
	}
	if err := os.WriteFile(filepath.Join(dir, FileName), big, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(dir, "api", true); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("err was %v", err)
	}
}

// The file does not get to set the namespace separator itself, or it could
// claim to come from another repository.
func TestFileMayNotForgeANamespace(t *testing.T) {
	doc := `apiVersion: drover/v1
kind: Environment
metadata: {name: other.prod}
spec:
  variables: {a: b}
`
	res, err := Read(checkout(t, doc), "api", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Pending) != 0 || len(res.Skipped) != 1 {
		t.Fatalf("pending=%d skipped=%v", len(res.Pending), res.Skipped)
	}
}

// The generated label is under the reserved prefix, which a document may not
// write, so it cannot be forged.
func TestSourceLabelCannotBeForged(t *testing.T) {
	doc := `apiVersion: drover/v1
kind: Environment
metadata:
  name: prod
  labels:
    drover.io/source: repository/trusted-thing
spec:
  variables: {a: b}
`
	if _, err := Read(checkout(t, doc), "api", true); err == nil {
		t.Fatal("a document wrote a reserved label and was accepted")
	}
}
