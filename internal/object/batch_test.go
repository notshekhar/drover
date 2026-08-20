package object

import (
	"strings"
	"testing"
)

func repoNamed(name string) string {
	return strings.Replace(repoDoc, "name: api", "name: "+name, 1)
}

// The headline rule: two objects of the same kind cannot share a name, and
// within one apply there is no last-one-wins.
func TestBatchRejectsDuplicateInOneFile(t *testing.T) {
	doc := repoDoc + "---\n" + repoDoc
	objs := mustParse(t, "/work/repos.yaml", doc)

	b := NewBatch()
	err := b.AddAll(objs)
	if err == nil {
		t.Fatal("two Repository/api documents were accepted, want an error")
	}
	if !strings.Contains(err.Error(), "Repository/api") {
		t.Errorf("error = %q, want it to name the object", err)
	}
	// Both sides must be named or the user has to go hunting.
	if strings.Count(err.Error(), "/work/repos.yaml") != 2 {
		t.Errorf("error = %q, want it to point at both documents", err)
	}
}

func TestBatchRejectsDuplicateAcrossFiles(t *testing.T) {
	b := NewBatch()
	if err := b.AddAll(mustParse(t, "/work/a.yaml", repoDoc)); err != nil {
		t.Fatal(err)
	}
	err := b.AddAll(mustParse(t, "/work/b.yaml", repoDoc))
	if err == nil {
		t.Fatal("the same name from two files was accepted, want an error")
	}
	for _, want := range []string{"/work/a.yaml", "/work/b.yaml", "Repository/api"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}
}

// Nothing is written when a batch is rejected, so the batch must not have
// half-accumulated either.
func TestBatchStopsAtFirstDuplicate(t *testing.T) {
	b := NewBatch()
	objs := mustParse(t, "f.yaml", repoDoc+"---\n"+repoNamed("web")+"---\n"+repoDoc)
	if err := b.AddAll(objs); err == nil {
		t.Fatal("want an error")
	}
	if b.Len() != 2 {
		t.Errorf("batch holds %d objects, want the 2 added before the duplicate", b.Len())
	}
}

// Different kinds are different objects, so the same name across kinds is fine.
func TestBatchAllowsSameNameAcrossKinds(t *testing.T) {
	env := `apiVersion: drover/v1
kind: Environment
metadata:
  name: api
spec:
  variables:
    baseUrl: http://127.0.0.1:3000
`
	b := NewBatch()
	if err := b.AddAll(mustParse(t, "a.yaml", repoDoc)); err != nil {
		t.Fatal(err)
	}
	if err := b.AddAll(mustParse(t, "b.yaml", env)); err != nil {
		t.Fatalf("Repository/api and Environment/api collided: %v", err)
	}
	if b.Len() != 2 {
		t.Errorf("batch holds %d objects, want 2", b.Len())
	}
}

func TestBatchDistinctNamesAreFine(t *testing.T) {
	b := NewBatch()
	objs := mustParse(t, "f.yaml", repoDoc+"---\n"+repoNamed("web")+"---\n"+repoNamed("worker"))
	if err := b.AddAll(objs); err != nil {
		t.Fatalf("distinct names were rejected: %v", err)
	}
	if b.Len() != 3 {
		t.Errorf("batch holds %d objects, want 3", b.Len())
	}
}

// An HTTPRequest naming an environment that does not exist is a typo, and
// left alone it surfaces as an unresolved {{baseUrl}} long after the fact.
func TestBatchRejectsUnknownEnvironmentRef(t *testing.T) {
	req := `apiVersion: drover/v1
kind: HTTPRequest
metadata:
  name: get-user
spec:
  method: GET
  url: "{{baseUrl}}/users"
  environments: [stage, prod]
  defaultEnvironment: stage
`
	env := `apiVersion: drover/v1
kind: Environment
metadata:
  name: stage
spec:
  variables:
    baseUrl: https://stage.example.com
`
	b := NewBatch()
	if err := b.AddAll(mustParse(t, "req.yaml", req)); err != nil {
		t.Fatal(err)
	}
	if err := b.AddAll(mustParse(t, "env.yaml", env)); err != nil {
		t.Fatal(err)
	}

	err := b.Check(nil)
	if err == nil {
		t.Fatal("a request naming a missing environment was accepted")
	}
	if !strings.Contains(err.Error(), "prod") {
		t.Errorf("error = %q, want it to name the missing environment", err)
	}
	if strings.Contains(err.Error(), "stage") {
		t.Errorf("error = %q, want it to only complain about the missing one", err)
	}

	// An environment already in the store counts, not only one in this batch.
	existing := map[Ref]bool{{Kind: KindEnvironment, Name: "prod"}: true}
	if err := b.Check(existing); err != nil {
		t.Errorf("an already-applied environment was not accepted: %v", err)
	}
}

// Two names cloning the same remote is legal -- two checkouts on purpose --
// but it is also what a copy-paste looks like, so it warns.
func TestBatchWarnsOnSameRemoteTwice(t *testing.T) {
	b := NewBatch()
	if err := b.AddAll(mustParse(t, "f.yaml", repoDoc+"---\n"+repoNamed("api-copy"))); err != nil {
		t.Fatalf("same remote under two names must be legal: %v", err)
	}
	if err := b.Check(nil); err != nil {
		t.Fatalf("Check: %v", err)
	}
	warnings := b.Warnings()
	if len(warnings) != 1 {
		t.Fatalf("got %d warnings, want 1: %v", len(warnings), warnings)
	}
	for _, want := range []string{"api", "api-copy"} {
		if !strings.Contains(warnings[0], want) {
			t.Errorf("warning = %q, want it to name %q", warnings[0], want)
		}
	}
}

func TestBatchNoSpuriousWarnings(t *testing.T) {
	b := NewBatch()
	other := strings.Replace(repoNamed("web"), "acme/api", "acme/web", 1)
	if err := b.AddAll(mustParse(t, "f.yaml", repoDoc+"---\n"+other)); err != nil {
		t.Fatal(err)
	}
	if w := b.Warnings(); len(w) != 0 {
		t.Errorf("got warnings for distinct remotes: %v", w)
	}
}

// The client validates locally before sending, and it cannot see the store.
// If that pass ran the cross-object rules, every request referencing an
// already-applied environment would be rejected before it left the machine.
func TestCheckLocalIgnoresCrossObjectRefs(t *testing.T) {
	req := `apiVersion: drover/v1
kind: HTTPRequest
metadata:
  name: get-user
spec:
  method: GET
  url: "{{baseUrl}}/users"
  environments: [applied-last-week]
  defaultEnvironment: applied-last-week
`
	b := NewBatch()
	if err := b.AddAll(mustParse(t, "req.yaml", req)); err != nil {
		t.Fatal(err)
	}
	if err := b.CheckLocal(); err != nil {
		t.Errorf("CheckLocal rejected a reference it cannot resolve: %v", err)
	}
	if err := b.Check(nil); err == nil {
		t.Error("Check should still catch it, since the server can see the store")
	}
}
