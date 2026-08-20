package httpreq

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/notshekhar/drover/internal/object"
)

// specFrom parses a document body into an HTTPRequestSpec, so the tests
// exercise the same validation a real apply would.
func specFrom(t *testing.T, body string) *object.HTTPRequestSpec {
	t.Helper()
	doc := "apiVersion: drover/v1\nkind: HTTPRequest\nmetadata:\n  name: r\nspec:\n" + body
	objs, err := object.Parse("req.yaml", []byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	spec, err := objs[0].HTTPRequest()
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

func envFrom(t *testing.T, body string) *object.EnvironmentSpec {
	t.Helper()
	doc := "apiVersion: drover/v1\nkind: Environment\nmetadata:\n  name: test\nspec:\n" + body
	objs, err := object.Parse("env.yaml", []byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	spec, err := objs[0].Environment()
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

// record captures what the server actually received.
type record struct {
	path     string
	rawPath  string
	rawQuery string
	headers  http.Header
	body     string
}

func testServer(t *testing.T, got *record) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path = r.URL.Path
		got.rawPath = r.URL.EscapedPath()
		got.rawQuery = r.URL.RawQuery
		got.headers = r.Header.Clone()
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		got.body = string(buf[:n])
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDoResolvesAllThreeSources(t *testing.T) {
	var got record
	srv := testServer(t, &got)

	spec := specFrom(t, `  method: GET
  url: "{{baseUrl}}/v1/users/{userId}"
  pathParams:
    - name: userId
      description: id
  query:
    - name: include
      description: relations
  headers:
    - name: Authorization
      value: "Bearer {{token}}"
    - name: X-Trace
      value: "${TRACE_ID}"
`)
	env := envFrom(t, "  variables:\n    baseUrl: "+srv.URL+"\n  secrets:\n    token: ${TEST_TOKEN}\n")

	e := New()
	e.Getenv = func(n string) (string, bool) {
		switch n {
		case "TEST_TOKEN":
			return "s3cret", true
		case "TRACE_ID":
			return "trace-1", true
		}
		return "", false
	}

	resp, err := e.Do(context.Background(), Request{
		Spec:        spec,
		Environment: env,
		EnvName:     "test",
		Params:      map[string]string{"userId": "u1", "include": "profile"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != 200 {
		t.Errorf("status = %d", resp.Status)
	}
	if got.path != "/v1/users/u1" {
		t.Errorf("path = %q, want /v1/users/u1", got.path)
	}
	if got.rawQuery != "include=profile" {
		t.Errorf("query = %q", got.rawQuery)
	}
	if h := got.headers.Get("Authorization"); h != "Bearer s3cret" {
		t.Errorf("Authorization = %q -- the secret did not resolve", h)
	}
	if h := got.headers.Get("X-Trace"); h != "trace-1" {
		t.Errorf("X-Trace = %q", h)
	}
}

// A caller's parameter lands in a URL path. It must not be able to add
// segments, escape the path, or open a query of its own.
func TestParamsAreEscapedIntoTheURL(t *testing.T) {
	var got record
	srv := testServer(t, &got)

	spec := specFrom(t, `  method: GET
  url: "`+srv.URL+`/users/{userId}"
  pathParams:
    - name: userId
      description: id
`)
	e := New()
	_, err := e.Do(context.Background(), Request{
		Spec:   spec,
		Params: map[string]string{"userId": "../../admin"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// r.URL.Path is the decoded form, so the check that matters is what went
	// over the wire: the value must be one escaped segment, not three.
	if strings.Contains(got.rawPath, "/../") || strings.HasSuffix(got.rawPath, "/..") {
		t.Errorf("wire path = %q, want the traversal escaped into one segment", got.rawPath)
	}
	if !strings.Contains(got.rawPath, "%2F") {
		t.Errorf("wire path = %q, want the slashes percent-encoded", got.rawPath)
	}
	if got.rawQuery != "" {
		t.Errorf("query = %q, want none", got.rawQuery)
	}
}

// A parameter must not be able to inject another query parameter.
func TestQueryValuesAreEncoded(t *testing.T) {
	var got record
	srv := testServer(t, &got)

	spec := specFrom(t, `  method: GET
  url: "`+srv.URL+`/search"
  query:
    - name: q
      description: the search
`)
	e := New()
	if _, err := e.Do(context.Background(), Request{
		Spec:   spec,
		Params: map[string]string{"q": "a&admin=true"},
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got.rawQuery, "&admin=true") {
		t.Errorf("query = %q, want the ampersand encoded", got.rawQuery)
	}
}

// Only GET is executed on an agent's behalf, whatever the document stores.
func TestNonGetIsRefusedUnlessExplicitlyAllowed(t *testing.T) {
	var got record
	srv := testServer(t, &got)

	spec := specFrom(t, `  method: POST
  url: "`+srv.URL+`/things"
`)
	e := New()
	_, err := e.Do(context.Background(), Request{Spec: spec})
	if err == nil {
		t.Fatal("a POST was executed without an explicit opt-in")
	}
	if !strings.Contains(err.Error(), "only executes GET") {
		t.Errorf("error = %q", err)
	}

	// The CLI can opt in; MCP never will.
	if _, err := e.Do(context.Background(), Request{Spec: spec, AllowUnsafeMethod: true}); err != nil {
		t.Errorf("an explicitly allowed POST failed: %v", err)
	}
}

func TestMissingRequiredParam(t *testing.T) {
	spec := specFrom(t, `  method: GET
  url: "https://example.com/users/{userId}"
  pathParams:
    - name: userId
      description: id
`)
	_, err := New().Do(context.Background(), Request{Spec: spec})
	if err == nil {
		t.Fatal("a call with no userId succeeded")
	}
	if !strings.Contains(err.Error(), "userId") {
		t.Errorf("error = %q", err)
	}
}

// A caller that can smuggle in an undeclared name is a caller probing for one
// the template happens to use.
func TestUnknownParamIsRefused(t *testing.T) {
	spec := specFrom(t, `  method: GET
  url: "https://example.com/x"
`)
	_, err := New().Do(context.Background(), Request{
		Spec:   spec,
		Params: map[string]string{"token": "sneaky"},
	})
	if err == nil {
		t.Fatal("an undeclared parameter was accepted")
	}
	if !strings.Contains(err.Error(), "token") {
		t.Errorf("error = %q", err)
	}
}

// A parameter must never be able to reach an environment value or a process
// variable -- those resolve server-side only.
func TestParamsCannotReachSecrets(t *testing.T) {
	var got record
	srv := testServer(t, &got)

	spec := specFrom(t, `  method: GET
  url: "{{baseUrl}}/x"
  query:
    - name: baseUrl
      description: deliberately shadows the environment name
`)
	env := envFrom(t, "  variables:\n    baseUrl: "+srv.URL+"\n")

	e := New()
	_, err := e.Do(context.Background(), Request{
		Spec:        spec,
		Environment: env,
		EnvName:     "test",
		Params:      map[string]string{"baseUrl": "https://evil.example.com"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The url still went to the environment's host; the parameter only became
	// a query value.
	if got.path != "/x" {
		t.Errorf("path = %q -- the parameter overrode the environment", got.path)
	}
	if !strings.Contains(got.rawQuery, "evil.example.com") {
		t.Errorf("query = %q, want the parameter to land only in the query", got.rawQuery)
	}
}

func TestUnresolvedEnvironmentValueFails(t *testing.T) {
	spec := specFrom(t, `  method: GET
  url: "{{baseUrl}}/x"
`)
	_, err := New().Do(context.Background(), Request{Spec: spec, EnvName: "prod"})
	if err == nil {
		t.Fatal("a request with an unresolved {{baseUrl}} was sent")
	}
	for _, want := range []string{"baseUrl", "prod"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}
}

func TestQueryDefaultsApply(t *testing.T) {
	var got record
	srv := testServer(t, &got)

	spec := specFrom(t, `  method: GET
  url: "`+srv.URL+`/x"
  query:
    - name: tenant
      description: tenant slug
      default: "{{tenant}}"
`)
	env := envFrom(t, "  variables:\n    tenant: acme\n")

	if _, err := New().Do(context.Background(), Request{
		Spec: spec, Environment: env, EnvName: "test",
	}); err != nil {
		t.Fatal(err)
	}
	if got.rawQuery != "tenant=acme" {
		t.Errorf("query = %q, want the default resolved from the environment", got.rawQuery)
	}
}

func TestNonHTTPSchemeRefused(t *testing.T) {
	spec := specFrom(t, `  method: GET
  url: "{{baseUrl}}/x"
`)
	env := envFrom(t, "  variables:\n    baseUrl: file:///etc\n")
	_, err := New().Do(context.Background(), Request{Spec: spec, Environment: env, EnvName: "test"})
	if err == nil {
		t.Fatal("a file:// url was accepted after substitution")
	}
	if !strings.Contains(err.Error(), "http or https") {
		t.Errorf("error = %q", err)
	}
}

func TestBodyIsRendered(t *testing.T) {
	var got record
	srv := testServer(t, &got)

	spec := specFrom(t, `  method: POST
  url: "`+srv.URL+`/things"
  body:
    contentType: application/json
    template: |
      {"tenant": "{{tenant}}"}
`)
	env := envFrom(t, "  variables:\n    tenant: acme\n")

	if _, err := New().Do(context.Background(), Request{
		Spec: spec, Environment: env, EnvName: "test", AllowUnsafeMethod: true,
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.body, `"tenant": "acme"`) {
		t.Errorf("body = %q", got.body)
	}
	if ct := got.headers.Get("Content-Type"); ct != "application/json" {
		t.Errorf("content type = %q", ct)
	}
}

// A response heading for a context window must be bounded.
func TestLargeResponseIsTruncated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		big := strings.Repeat("x", MaxBodyBytes+1024)
		_, _ = w.Write([]byte(big))
	}))
	defer srv.Close()

	spec := specFrom(t, "  method: GET\n  url: \""+srv.URL+"/big\"\n")
	resp, err := New().Do(context.Background(), Request{Spec: spec})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Truncated {
		t.Error("a huge response was not reported as truncated")
	}
	if len(resp.Body) > MaxBodyBytes {
		t.Errorf("body is %d bytes, over the %d cap", len(resp.Body), MaxBodyBytes)
	}
}

// Echoing every response header risks handing back a Set-Cookie.
func TestOnlySafeResponseHeadersComeBack(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "session=abc")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()

	spec := specFrom(t, "  method: GET\n  url: \""+srv.URL+"/x\"\n")
	resp, err := New().Do(context.Background(), Request{Spec: spec})
	if err != nil {
		t.Fatal(err)
	}
	if _, leaked := resp.Headers["Set-Cookie"]; leaked {
		t.Error("Set-Cookie came back to the caller")
	}
	if resp.Headers["Content-Type"] != "application/json" {
		t.Errorf("headers = %v", resp.Headers)
	}
}
