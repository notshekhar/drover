package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/notshekhar/drover/internal/api"
	"github.com/notshekhar/drover/internal/config"
	"github.com/notshekhar/drover/internal/object"
)

const repoDoc = `apiVersion: drover/v1
kind: Repository
metadata:
  name: api
spec:
  url: https://github.com/acme/api
  branch: main
`

func newServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := New(Options{DataDir: dir, Listen: config.DefaultListen, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return s, dir
}

func apply(t *testing.T, s *Server, docs ...api.Document) (*httptest.ResponseRecorder, *api.ApplyResponse) {
	t.Helper()
	body, err := json.Marshal(api.ApplyRequest{Documents: docs})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, api.Prefix+"/apply", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	var out api.ApplyResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, &out
}

func doc(source, data string) api.Document { return api.Document{Source: source, Data: data} }

func TestApplyCreatesThenUpdates(t *testing.T) {
	s, dataDir := newServer(t)

	rec, resp := apply(t, s, doc("/work/repo.yaml", repoDoc))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if len(resp.Results) != 1 || resp.Results[0].Action != api.ActionCreated {
		t.Fatalf("results = %+v, want one created", resp.Results)
	}

	// It landed on disk, which is the whole point of apply.
	stored := filepath.Join(dataDir, "objects", "Repository", "api.yaml")
	if _, err := os.Stat(stored); err != nil {
		t.Fatalf("apply did not persist to %s: %v", stored, err)
	}

	_, resp = apply(t, s, doc("/work/repo.yaml", repoDoc))
	if resp.Results[0].Action != api.ActionUpdated {
		t.Errorf("second apply = %q, want updated", resp.Results[0].Action)
	}
}

// Two objects of the same kind cannot share a name, and the whole apply is
// refused rather than picking a winner.
func TestApplyRejectsDuplicateNames(t *testing.T) {
	s, dataDir := newServer(t)

	rec, _ := apply(t, s,
		doc("/work/a.yaml", repoDoc),
		doc("/work/b.yaml", repoDoc),
	)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409: %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{"Repository/api", "/work/a.yaml", "/work/b.yaml"} {
		if !strings.Contains(body, want) {
			t.Errorf("body = %q, want it to mention %q", body, want)
		}
	}

	// Nothing was written.
	if _, err := os.Stat(filepath.Join(dataDir, "objects", "Repository", "api.yaml")); err == nil {
		t.Error("a rejected apply still wrote an object")
	}
}

// A bad document in the batch must leave the earlier ones unapplied.
func TestApplyIsAllOrNothing(t *testing.T) {
	s, dataDir := newServer(t)
	bad := strings.Replace(repoDoc, "branch: main", "branch: -nope", 1)

	rec, _ := apply(t, s,
		doc("/work/good.yaml", strings.Replace(repoDoc, "name: api", "name: web", 1)),
		doc("/work/bad.yaml", bad),
	)
	if rec.Code == http.StatusOK {
		t.Fatal("an invalid document was accepted")
	}
	if _, err := os.Stat(filepath.Join(dataDir, "objects", "Repository", "web.yaml")); err == nil {
		t.Error("the good document was written even though the batch failed")
	}
}

// Every kind applies now. A SQLConnection needs its provider spelled out when
// the url is a ${ENV} reference, because there is no scheme to infer from.
func TestApplySQLConnection(t *testing.T) {
	s, dataDir := newServer(t)
	sql := `apiVersion: drover/v1
kind: SQLConnection
metadata:
  name: prod
spec:
  provider: redshift
  url: ${DATABASE_URL}
  health: SELECT 1
`
	rec, resp := apply(t, s, doc("/work/sql.yaml", sql))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if len(resp.Results) != 1 || resp.Results[0].Action != api.ActionCreated {
		t.Fatalf("results = %+v", resp.Results)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "objects", "SQLConnection", "prod.yaml")); err != nil {
		t.Errorf("the object was not persisted: %v", err)
	}

	// Without a provider and without a scheme to infer from, it must fail.
	noProvider := strings.Replace(sql, "  provider: redshift\n", "", 1)
	noProvider = strings.Replace(noProvider, "name: prod", "name: other", 1)
	rec, _ = apply(t, s, doc("/work/sql2.yaml", noProvider))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status %d, want 400 for a provider that cannot be inferred", rec.Code)
	}
}

// An HTTPRequest may reference an Environment applied in an earlier batch,
// not only one in its own.
func TestApplyHTTPRequestAgainstStoredEnvironment(t *testing.T) {
	s, _ := newServer(t)
	env := `apiVersion: drover/v1
kind: Environment
metadata:
  name: stage
spec:
  variables:
    baseUrl: https://stage.example.com
`
	req := `apiVersion: drover/v1
kind: HTTPRequest
metadata:
  name: get-user
spec:
  description: Fetch a user.
  method: GET
  url: "{{baseUrl}}/users/{userId}"
  environments: [stage]
  defaultEnvironment: stage
  pathParams:
    - name: userId
      description: The user id.
      required: true
`
	if rec, _ := apply(t, s, doc("/work/env.yaml", env)); rec.Code != http.StatusOK {
		t.Fatalf("environment apply failed: %s", rec.Body)
	}
	rec, _ := apply(t, s, doc("/work/req.yaml", req))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}

	// And a request naming an environment nobody applied must fail.
	orphan := strings.Replace(req, "name: get-user", "name: orphan", 1)
	orphan = strings.Replace(orphan, "[stage]", "[nowhere]", 1)
	orphan = strings.Replace(orphan, "defaultEnvironment: stage", "defaultEnvironment: nowhere", 1)
	rec, _ = apply(t, s, doc("/work/orphan.yaml", orphan))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status %d, want 400 for a missing environment", rec.Code)
	}
}

func TestGetListDelete(t *testing.T) {
	s, dataDir := newServer(t)
	apply(t, s, doc("/work/a.yaml", repoDoc))
	apply(t, s, doc("/work/b.yaml", strings.Replace(repoDoc, "name: api", "name: web", 1)))

	// list
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, api.Prefix+"/repositories", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list status %d", rec.Code)
	}
	var list api.ListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 2 {
		t.Fatalf("got %d items, want 2", len(list.Items))
	}
	if list.Items[0].Branch != "main" || list.Items[0].URL == "" {
		t.Errorf("item = %+v, want repository fields filled", list.Items[0])
	}

	// get one
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, api.Prefix+"/repositories/api", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("get status %d", rec.Code)
	}

	// get missing
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, api.Prefix+"/repositories/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("missing object status %d, want 404", rec.Code)
	}

	// delete, and the checkout goes with it
	checkout := filepath.Join(dataDir, "repos", "api")
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, api.Prefix+"/repositories/api", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete status %d: %s", rec.Code, rec.Body)
	}
	if _, err := os.Stat(checkout); err == nil {
		t.Error("the checkout survived the delete")
	}
	if _, err := os.Stat(filepath.Join(dataDir, "objects", "Repository", "api.yaml")); err == nil {
		t.Error("the object survived the delete")
	}
}

func TestDeleteMissing(t *testing.T) {
	s, _ := newServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, api.Prefix+"/repositories/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status %d, want 404", rec.Code)
	}
}

func TestPutRouteChecksNameAndKind(t *testing.T) {
	s, _ := newServer(t)

	// name in the path disagrees with the document
	req := httptest.NewRequest(http.MethodPut, api.Prefix+"/repositories/web", strings.NewReader(repoDoc))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status %d, want 400 for a name mismatch", rec.Code)
	}

	// kind in the path disagrees with the document
	req = httptest.NewRequest(http.MethodPut, api.Prefix+"/environments/api", strings.NewReader(repoDoc))
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status %d, want 400 for a kind mismatch", rec.Code)
	}

	// matching
	req = httptest.NewRequest(http.MethodPut, api.Prefix+"/repositories/api", strings.NewReader(repoDoc))
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status %d: %s", rec.Code, rec.Body)
	}
}

// Restarting must not forget what was applied: the store, not the client's
// yaml, is the desired state.
func TestBootstrapLoadsStoredObjects(t *testing.T) {
	dir := t.TempDir()
	s, err := New(Options{DataDir: dir, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	apply(t, s, doc("/work/a.yaml", repoDoc))

	// A second engine over the same data dir, as if the process restarted.
	s2, err := New(Options{DataDir: dir, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s2.Bootstrap(&config.Config{}); err != nil {
		t.Fatal(err)
	}
	objs, err := s2.Store().List(object.KindRepository)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 1 || objs[0].Metadata.Name != "api" {
		t.Fatalf("restart lost the applied object: %+v", objs)
	}
}

// The config's apply: paths are applied at boot, before serving.
func TestBootstrapAppliesConfigPaths(t *testing.T) {
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "repo.yaml")
	if err := os.WriteFile(yamlPath, []byte(repoDoc), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := New(Options{DataDir: filepath.Join(dir, "data"), Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Bootstrap(&config.Config{Apply: []string{yamlPath}}); err != nil {
		t.Fatal(err)
	}
	objs, err := s.Store().List(object.KindRepository)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 1 {
		t.Fatalf("got %d objects, want the one from apply:", len(objs))
	}
}

// A missing apply: path must stop the engine rather than start it holding
// less than the user asked for.
func TestBootstrapFailsOnMissingApplyPath(t *testing.T) {
	dir := t.TempDir()
	s, err := New(Options{DataDir: dir, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	err = s.Bootstrap(&config.Config{Apply: []string{filepath.Join(dir, "gone.yaml")}})
	if err == nil {
		t.Fatal("a missing apply: path started the engine anyway")
	}
	if !strings.Contains(err.Error(), "forget") {
		t.Errorf("error = %q, want it to suggest a way out", err)
	}
}

// Two apply: paths naming the same object must fail the boot, not race.
func TestBootstrapRejectsDuplicateAcrossApplyPaths(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.yaml")
	b := filepath.Join(dir, "b.yaml")
	for _, p := range []string{a, b} {
		if err := os.WriteFile(p, []byte(repoDoc), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s, err := New(Options{DataDir: filepath.Join(dir, "data"), Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	err = s.Bootstrap(&config.Config{Apply: []string{a, b}})
	if err == nil {
		t.Fatal("duplicate names across apply: paths were accepted")
	}
	if !strings.Contains(err.Error(), "Repository/api") {
		t.Errorf("error = %q", err)
	}
}

func TestStatus(t *testing.T) {
	s, _ := newServer(t)
	apply(t, s, doc("/work/a.yaml", repoDoc))

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, api.Prefix+"/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var out api.StatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Objects != 1 || out.Version != "test" {
		t.Errorf("status = %+v", out)
	}
}

// The port walk means a second engine does not simply die on a collision.
func TestListenWalksPastABusyPort(t *testing.T) {
	s, _ := newServer(t)
	first, err := s.Listen()
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()

	s2, _ := newServer(t)
	s2.opts.Listen = first.Addr().String()
	second, err := s2.Listen()
	if err != nil {
		t.Fatalf("second Listen failed instead of walking: %v", err)
	}
	defer second.Close()

	if first.Addr().String() == second.Addr().String() {
		t.Fatal("both listeners claim the same address")
	}
}
