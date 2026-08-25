package importer

import (
	"strings"
	"testing"

	"github.com/notshekhar/drover/internal/object"
)

const petstore = `
openapi: 3.0.3
info:
  title: Petstore
servers:
  - url: https://api.petstore.example/v1/
paths:
  /users/{userId}:
    parameters:
      - name: userId
        in: path
        required: true
        description: Opaque user id
        example: usr_1a2b3c
    get:
      operationId: getUserById
      summary: Fetch one user by id.
      tags: [users]
      parameters:
        - $ref: '#/components/parameters/Verbose'
    delete:
      operationId: deleteUser
      tags: [users]
  /pets:
    get:
      operationId: listPets
      tags: [pets]
      parameters:
        - name: limit
          in: query
          schema:
            type: integer
            default: 20
components:
  parameters:
    Verbose:
      name: verbose
      in: query
      description: Return the long form.
      schema:
        type: boolean
        example: true
`

func importPetstore(t *testing.T, opts Options) *Result {
	t.Helper()
	res, err := OpenAPI([]byte(petstore), opts)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestOpenAPIProducesApplicableDocuments(t *testing.T) {
	res := importPetstore(t, Options{Environment: "prod"})
	if res.Requests != 3 {
		t.Fatalf("generated %d requests, want 3:\n%s", res.Requests, res.Documents)
	}

	// The real test of an importer: what it emits has to survive apply.
	objs, err := object.Parse("generated.yaml", res.Documents)
	if err != nil {
		t.Fatalf("the generated yaml does not apply: %v\n%s", err, res.Documents)
	}
	if len(objs) != 4 { // one Environment plus three requests
		t.Fatalf("parsed %d objects, want 4", len(objs))
	}

	byName := map[string]*object.Object{}
	for _, o := range objs {
		byName[o.Metadata.Name] = o
	}

	// operationId is what the API's own authors called it, camelCase and all.
	got, ok := byName["get-user-by-id"]
	if !ok {
		t.Fatalf("no get-user-by-id; got %v", keysOf(byName))
	}
	spec, err := got.HTTPRequest()
	if err != nil {
		t.Fatal(err)
	}
	if spec.URL != "{{baseUrl}}/users/{userId}" {
		t.Errorf("url is %q", spec.URL)
	}
	if len(spec.PathParams) != 1 || spec.PathParams[0].Example != "usr_1a2b3c" {
		t.Errorf("path params are %+v", spec.PathParams)
	}
	// A path-level parameter applies to every operation under it. Missing
	// this is why generated requests come out short a required id.
	del, _ := byName["delete-user"].HTTPRequest()
	if len(del.PathParams) != 1 {
		t.Errorf("the path-level parameter did not reach the sibling operation: %+v", del.PathParams)
	}

	// A local $ref must resolve, and an example is worth more than a type.
	var verbose *object.Param
	for i := range spec.Query {
		if spec.Query[i].Name == "verbose" {
			verbose = &spec.Query[i]
		}
	}
	if verbose == nil || verbose.Description != "Return the long form." {
		t.Errorf("the $ref parameter did not resolve: %+v", spec.Query)
	}
	if verbose.Example != "true" {
		t.Errorf("the example did not survive: %q", verbose.Example)
	}
}

func TestOpenAPIEnvironmentCarriesTheServer(t *testing.T) {
	res := importPetstore(t, Options{Environment: "prod"})
	if !strings.Contains(string(res.Documents), "baseUrl: https://api.petstore.example/v1") {
		t.Errorf("the server url did not become baseUrl:\n%s", res.Documents)
	}
	if strings.Contains(string(res.Documents), "/v1/\n") {
		t.Error("the trailing slash was kept, which would produce a double slash in every url")
	}
}

func TestOpenAPITagFilter(t *testing.T) {
	res := importPetstore(t, Options{Environment: "prod", Tags: []string{"pets"}})
	if res.Requests != 1 {
		t.Fatalf("a tag filter produced %d requests, want 1", res.Requests)
	}
	if !strings.Contains(string(res.Documents), "list-pets") {
		t.Errorf("the wrong operation survived:\n%s", res.Documents)
	}
}

func TestPrefixDisambiguates(t *testing.T) {
	res := importPetstore(t, Options{Prefix: "petstore"})
	if !strings.Contains(string(res.Documents), "name: petstore-get-user-by-id") {
		t.Errorf("the prefix was not applied:\n%s", res.Documents)
	}
}

// A caller-settable header is a credential-theft primitive and drover refuses
// one. Dropping it silently would produce a request that looks complete and
// is not, so the operation is skipped with the reason.
func TestHeaderParameterIsSkippedLoudly(t *testing.T) {
	spec := `
openapi: 3.0.0
info: {title: t}
paths:
  /x:
    get:
      operationId: getX
      parameters:
        - name: X-Tenant
          in: header
`
	res, err := OpenAPI([]byte(spec), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Requests != 0 {
		t.Errorf("a header parameter was silently accepted")
	}
	if len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0], "header") {
		t.Errorf("skips were %v", res.Skipped)
	}
}

// Generated specs contain $ref cycles. A naive resolver hangs on one.
func TestRefCycleIsRefusedNotHung(t *testing.T) {
	spec := `
openapi: 3.0.0
info: {title: t}
paths:
  /x:
    get:
      operationId: getX
      parameters:
        - $ref: '#/components/parameters/A'
components:
  parameters:
    A: {$ref: '#/components/parameters/B'}
    B: {$ref: '#/components/parameters/A'}
`
	res, err := OpenAPI([]byte(spec), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0], "circle") {
		t.Errorf("skips were %v", res.Skipped)
	}
}

func TestRemoteRefIsRefused(t *testing.T) {
	spec := `
openapi: 3.0.0
info: {title: t}
paths:
  /x:
    get:
      operationId: getX
      parameters:
        - $ref: 'https://example.com/params.yaml#/Verbose'
`
	res, _ := OpenAPI([]byte(spec), Options{})
	if len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0], "remote") {
		t.Errorf("a remote $ref was not refused: %v", res.Skipped)
	}
}

// A url that templates a segment it never declares would be rejected at
// apply. Filling it in is more useful than failing.
func TestUndeclaredPathParameterIsInferred(t *testing.T) {
	spec := `
openapi: 3.0.0
info: {title: t}
servers: [{url: 'https://x.example'}]
paths:
  /orders/{orderId}/items:
    get:
      operationId: listItems
`
	res, err := OpenAPI([]byte(spec), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := object.Parse("g.yaml", res.Documents); err != nil {
		t.Fatalf("the generated document does not apply: %v\n%s", err, res.Documents)
	}
	if !strings.Contains(string(res.Documents), "name: orderId") {
		t.Errorf("the undeclared parameter was not inferred:\n%s", res.Documents)
	}
}

// A big spec announces itself rather than producing four hundred documents.
func TestLargeSpecRefusesWithoutAll(t *testing.T) {
	var b strings.Builder
	b.WriteString("openapi: 3.0.0\ninfo: {title: big}\npaths:\n")
	for i := 0; i < Threshold+5; i++ {
		b.WriteString("  /p")
		b.WriteString(strings.Repeat("x", 1))
		b.WriteString(itoa(i))
		b.WriteString(":\n    get:\n      operationId: op")
		b.WriteString(itoa(i))
		b.WriteString("\n")
	}
	res, err := OpenAPI([]byte(b.String()), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated {
		t.Fatalf("a %d-operation spec was imported without --all", res.Requests)
	}
	res, err = OpenAPI([]byte(b.String()), Options{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Truncated || res.Requests != Threshold+5 {
		t.Errorf("--all produced %d requests, truncated=%v", res.Requests, res.Truncated)
	}
}

func TestSwaggerIsRecognisedAndRefused(t *testing.T) {
	_, err := OpenAPI([]byte("swagger: '2.0'\npaths:\n  /x:\n    get: {}\n"), Options{})
	if err == nil || !strings.Contains(err.Error(), "Swagger") {
		t.Fatalf("a Swagger 2 document produced %v", err)
	}
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
