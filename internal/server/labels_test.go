package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/notshekhar/drover/internal/api"
)

const labelledRepos = `apiVersion: drover/v1
kind: Repository
metadata:
  name: billing
  labels:
    team: billing
    tier: backend
spec:
  url: https://github.com/acme/billing
  branch: main
---
apiVersion: drover/v1
kind: Repository
metadata:
  name: web
  labels:
    team: web
spec:
  url: https://github.com/acme/web
  branch: main
`

func applyLabelled(t *testing.T, s *Server) {
	t.Helper()
	rec, resp := apply(t, s, api.Document{Source: "labels.yaml", Data: labelledRepos})
	if rec.Code != http.StatusOK {
		t.Fatalf("apply returned %d: %s", rec.Code, rec.Body.String())
	}
	if len(resp.Results) != 2 {
		t.Fatalf("applied %d objects, want 2", len(resp.Results))
	}
}

func listWithSelector(t *testing.T, s *Server, selector string) []api.ObjectView {
	t.Helper()
	url := api.Prefix + "/repositories"
	if selector != "" {
		url += "?labelSelector=" + selector
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list %q returned %d: %s", selector, rec.Code, rec.Body.String())
	}
	var out api.ListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Items
}

func TestListFiltersByLabel(t *testing.T) {
	s, _ := newServer(t)
	applyLabelled(t, s)

	if got := listWithSelector(t, s, ""); len(got) != 2 {
		t.Errorf("no selector returned %d objects, want both", len(got))
	}
	got := listWithSelector(t, s, "team=billing")
	if len(got) != 1 || got[0].Name != "billing" {
		t.Fatalf("team=billing returned %+v", got)
	}
	if got[0].Labels["tier"] != "backend" {
		t.Errorf("labels did not survive the round trip: %+v", got[0].Labels)
	}
	if got := listWithSelector(t, s, "tier"); len(got) != 1 {
		t.Errorf("an exists clause returned %d, want 1", len(got))
	}
	if got := listWithSelector(t, s, "team!=billing"); len(got) != 1 || got[0].Name != "web" {
		t.Errorf("team!=billing returned %+v", got)
	}
}

func TestBadSelectorIsARequestError(t *testing.T) {
	s, _ := newServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, api.Prefix+"/repositories?labelSelector==nope", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("a malformed selector returned %d, want 400", rec.Code)
	}
}

// A selector scopes a search to the checkouts it matches, so grep searches
// three repositories out of forty instead of scanning all of them.
func TestGrepSelectorScopesTheSearch(t *testing.T) {
	s, dir := newServer(t)
	applyLabelled(t, s)
	for _, name := range []string{"billing", "web"} {
		path := filepath.Join(dir, "repos", name, "main.go")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("// ChargeIntent lives here\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	res, err := s.Backend().Grep(context.Background(), api.GrepRequest{Pattern: "ChargeIntent", Selector: "team=billing"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Matches) != 1 || !strings.HasPrefix(res.Matches[0].Path, "billing/") {
		t.Fatalf("a scoped grep returned %+v", res.Matches)
	}
	if res.Unsearched != nil {
		t.Errorf("a scoped search disclosed unsearched roots: %v", res.Unsearched)
	}
}

// A selector matching nothing is an error. Searching zero files and reporting
// no matches is the most misleading answer a search tool can give.
func TestSelectorMatchingNothingIsAnError(t *testing.T) {
	s, _ := newServer(t)
	applyLabelled(t, s)

	_, err := s.Backend().Grep(context.Background(), api.GrepRequest{Pattern: "x", Selector: "team=nobody"})
	if err == nil {
		t.Fatal("a selector matching no repository returned a result instead of an error")
	}
	if !strings.Contains(err.Error(), "team=nobody") {
		t.Errorf("the error does not quote the selector: %v", err)
	}
}

// A selector already answers "which checkouts", so a path alongside it is
// either redundant or a contradiction. Guessing would be worse than refusing.
func TestSelectorAndPathTogetherAreRefused(t *testing.T) {
	s, dir := newServer(t)
	applyLabelled(t, s)
	if err := os.MkdirAll(filepath.Join(dir, "repos", "billing"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := s.Backend().Grep(context.Background(), api.GrepRequest{Pattern: "x", Selector: "team=billing", Path: "web"})
	if err == nil {
		t.Fatal("a path and a selector together were accepted")
	}
}
