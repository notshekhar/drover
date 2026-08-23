package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlerServesTheShell(t *testing.T) {
	for _, p := range []string{Path, Path + "/", Path + "/activity"} {
		rec := httptest.NewRecorder()
		Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", p, rec.Code)
		}
		if got := rec.Header().Get("Content-Security-Policy"); got != "default-src 'self'" {
			t.Errorf("%s: CSP = %q", p, got)
		}
		if !strings.Contains(rec.Header().Get("Content-Type"), "text/html") {
			t.Errorf("%s: Content-Type = %q, want HTML", p, rec.Header().Get("Content-Type"))
		}
		body := rec.Body.String()
		if !strings.Contains(body, "drover") {
			t.Errorf("%s: the shell does not name itself:\n%s", p, body)
		}
		if strings.Contains(body, "<script>") && !strings.Contains(body, `src="`) {
			t.Errorf("%s: inline script would violate the CSP", p)
		}
	}
}

func TestHandlerServesAssets(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, Path+"/", nil))
	body := rec.Body.String()
	i := strings.Index(body, `src="`)
	if i < 0 {
		t.Fatalf("shell has no script src:\n%s", body)
	}
	src := body[i+5:]
	src = src[:strings.Index(src, `"`)]
	if !strings.HasPrefix(src, Path+"/") {
		t.Fatalf("script src %q is not under %s", src, Path)
	}

	rec = httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, src, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("asset %s status %d", src, rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "javascript") {
		t.Errorf("asset Content-Type = %q", rec.Header().Get("Content-Type"))
	}
}

func TestHandler404sMissingAssets(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, Path+"/assets/no-such.js", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing asset status %d, want 404", rec.Code)
	}
}
