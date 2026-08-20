package object

import (
	"net/url"
	"strings"
	"testing"
)

func TestScanPlaceholders(t *testing.T) {
	got := ScanPlaceholders("{{baseUrl}}/v1/users/{userId}?k=${API_KEY}&n={{tenant}}")
	want := []Placeholder{
		{FromEnvironment, "baseUrl"},
		{FromParam, "userId"},
		{FromProcessEnv, "API_KEY"},
		{FromEnvironment, "tenant"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d placeholders, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// {{name}} must be matched before {name}, or every environment reference also
// registers as a stray parameter and validation collapses.
func TestDoubleBraceIsNotAParam(t *testing.T) {
	if names := PlaceholderNames("{{baseUrl}}/x", FromParam); len(names) != 0 {
		t.Errorf("params = %v, want none -- {{baseUrl}} is an environment value", names)
	}
	if names := PlaceholderNames("{{baseUrl}}/x", FromEnvironment); len(names) != 1 || names[0] != "baseUrl" {
		t.Errorf("environment names = %v, want [baseUrl]", names)
	}
}

func TestResolve(t *testing.T) {
	r := &Resolver{
		EnvName: "prod",
		Env: func(n string) (string, bool) {
			return map[string]string{"baseUrl": "https://api.example.com"}[n], n == "baseUrl"
		},
		Process: func(n string) (string, bool) { return map[string]string{"TOKEN": "s3cret"}[n], n == "TOKEN" },
		Param:   func(n string) (string, bool) { return map[string]string{"userId": "u1"}[n], n == "userId" },
	}
	got, err := r.Resolve("{{baseUrl}}/users/{userId}?t=${TOKEN}")
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://api.example.com/users/u1?t=s3cret" {
		t.Errorf("got %q", got)
	}
}

// An unresolved placeholder must fail loudly. Substituting empty would send a
// request to the wrong place, silently.
func TestResolveReportsMissing(t *testing.T) {
	r := &Resolver{EnvName: "stage", Env: func(string) (string, bool) { return "", false }}
	_, err := r.Resolve("{{baseUrl}}/x")
	if err == nil {
		t.Fatal("a missing environment value resolved anyway")
	}
	if !strings.Contains(err.Error(), "baseUrl") || !strings.Contains(err.Error(), "stage") {
		t.Errorf("error = %q, want the variable and the environment named", err)
	}
}

func TestResolveMissingParamAndProcessEnv(t *testing.T) {
	r := &Resolver{}
	_, err := r.Resolve("{userId}")
	if err == nil || !strings.Contains(err.Error(), "userId") {
		t.Errorf("param error = %v", err)
	}
	_, err = r.Resolve("${NOPE}")
	if err == nil || !strings.Contains(err.Error(), "NOPE") {
		t.Errorf("process env error = %v", err)
	}
}

// Resolution is single-pass, so a value containing braces is data. Otherwise
// an environment value could expand into another placeholder and recurse.
func TestResolveIsSinglePass(t *testing.T) {
	r := &Resolver{
		Env:   func(n string) (string, bool) { return "{{other}}", n == "baseUrl" },
		Param: func(string) (string, bool) { return "", false },
	}
	got, err := r.Resolve("{{baseUrl}}/x")
	if err != nil {
		t.Fatal(err)
	}
	if got != "{{other}}/x" {
		t.Errorf("got %q, want the value substituted literally without re-expanding", got)
	}
}

// A caller's value goes into a URL path, so it must not be able to add
// segments or a query of its own.
func TestResolveEscapesParams(t *testing.T) {
	r := &Resolver{
		Env:   func(n string) (string, bool) { return "https://api.example.com", n == "baseUrl" },
		Param: func(n string) (string, bool) { return "../../admin?x=1", n == "id" },
		Escape: func(kind PlaceholderKind, v string) string {
			if kind == FromParam {
				return url.PathEscape(v)
			}
			return v
		},
	}
	got, err := r.Resolve("{{baseUrl}}/users/{id}")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "../..") || strings.Contains(got, "?x=1") {
		t.Errorf("got %q, want the traversal and query escaped", got)
	}
}

func TestResolveLeavesUnmatchedBracesAlone(t *testing.T) {
	r := &Resolver{}
	for _, in := range []string{"a { b", "a } b", "${unclosed", "{{unclosed"} {
		got, err := r.Resolve(in)
		if err != nil {
			t.Errorf("Resolve(%q) errored: %v", in, err)
			continue
		}
		if got != in {
			t.Errorf("Resolve(%q) = %q, want it unchanged", in, got)
		}
	}
}

// A JSON body opens with a brace. Without an identifier check the scanner
// reads `"tenant": "x"` as a parameter name and mangles the document, which
// broke every JSON body template until it was fixed.
func TestJSONBracesAreNotPlaceholders(t *testing.T) {
	body := `{"tenant": "{{tenant}}", "n": 1}`

	if names := PlaceholderNames(body, FromParam); len(names) != 0 {
		t.Errorf("params = %v, want none -- those braces are JSON", names)
	}
	if names := PlaceholderNames(body, FromEnvironment); len(names) != 1 || names[0] != "tenant" {
		t.Errorf("environment names = %v, want [tenant]", names)
	}

	r := &Resolver{Env: func(n string) (string, bool) { return "acme", n == "tenant" }}
	got, err := r.Resolve(body)
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"tenant": "acme", "n": 1}` {
		t.Errorf("got %q, want the JSON intact with only {{tenant}} substituted", got)
	}
}

// Spaces inside the braces are trimmed, so the Handlebars-style spelling
// people reach for first still works.
func TestSpacesInsideBracesAreAllowed(t *testing.T) {
	r := &Resolver{Env: func(n string) (string, bool) { return "https://x", n == "baseUrl" }}
	got, err := r.Resolve("{{ baseUrl }}/y")
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://x/y" {
		t.Errorf("got %q", got)
	}
}

// Anything that is not an identifier between braces is data.
func TestNonIdentifierBracesAreLiteral(t *testing.T) {
	r := &Resolver{}
	for _, in := range []string{
		`{"a": 1}`,
		"{a.b}",
		"{a/b}",
		"{}",
		"${}",
		"{{}}",
		"a {1,3} b", // a regex quantifier
	} {
		got, err := r.Resolve(in)
		if err != nil {
			t.Errorf("Resolve(%q) errored: %v", in, err)
			continue
		}
		if got != in {
			t.Errorf("Resolve(%q) = %q, want it unchanged", in, got)
		}
	}
}
