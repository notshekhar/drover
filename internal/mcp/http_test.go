package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/notshekhar/drover/internal/server"
)

// engine returns a live drover engine serving its whole API, including /mcp.
func engine(t *testing.T, dataDir string) *httptest.Server {
	t.Helper()
	eng, err := server.New(server.Options{DataDir: dataDir, Version: "test", NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(eng.Handler())
	t.Cleanup(srv.Close)
	t.Cleanup(func() { _ = eng.Shutdown(context.Background()) })
	return srv
}

// rpc posts one JSON-RPC message to /mcp and returns the status and body.
func rpc(t *testing.T, srv *httptest.Server, body string, headers map[string]string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+server.MCPPath, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var out map[string]any
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decoding the reply: %v", err)
		}
	}
	return resp.StatusCode, out
}

// The headline of this change: an agent can point at a running engine instead
// of spawning a process.
func TestServeHostsMCPOverHTTP(t *testing.T) {
	srv := engine(t, dataDirWithRepo(t))

	code, out := rpc(t, srv, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}`, nil)
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	result, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %v", out)
	}
	if info := result["serverInfo"].(map[string]any); info["name"] != "drover" {
		t.Errorf("serverInfo = %v", info)
	}
}

// The tool list over HTTP must be the same list stdio serves; both dispatch
// through one router precisely so they cannot drift.
func TestHTTPToolsListMatchesStdio(t *testing.T) {
	dir := dataDirWithRepo(t)
	srv := engine(t, dir)

	_, out := rpc(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, nil)
	tools := out["result"].(map[string]any)["tools"].([]any)

	got := map[string]bool{}
	for _, x := range tools {
		got[x.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{"ls", "read", "grep", "find"} {
		if !got[want] {
			t.Errorf("tool %q missing over HTTP", want)
		}
	}
}

// A real tool call over HTTP, reaching real files through the in-process
// backend rather than a second hop through the listener.
func TestHTTPToolCall(t *testing.T) {
	srv := engine(t, dataDirWithRepo(t))

	_, out := rpc(t, srv, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"grep","arguments":{"pattern":"TODO"}}}`, nil)
	result, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %v", out)
	}
	var text string
	for _, c := range result["content"].([]any) {
		text += c.(map[string]any)["text"].(string)
	}
	if !strings.Contains(text, "api/internal/db.go:3") {
		t.Errorf("grep over HTTP = %q", text)
	}
}

// A notification has no id, so there is nothing to reply to: 202 with no body.
func TestHTTPNotificationGets202(t *testing.T) {
	srv := engine(t, t.TempDir())
	code, _ := rpc(t, srv, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, nil)
	if code != http.StatusAccepted {
		t.Errorf("status %d, want 202", code)
	}
}

// Older clients still send batches even though the newest revision dropped
// them; answering is cheaper than making them care.
func TestHTTPBatch(t *testing.T) {
	srv := engine(t, t.TempDir())

	body := `[{"jsonrpc":"2.0","id":1,"method":"ping"},{"jsonrpc":"2.0","method":"notifications/initialized"},{"jsonrpc":"2.0","id":2,"method":"ping"}]`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+server.MCPPath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var out []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	// Two requests, one notification: two replies, and the notification is
	// not among them.
	if len(out) != 2 {
		t.Fatalf("got %d replies, want 2 (the notification must not be answered)", len(out))
	}
}

// A visited web page can POST to a localhost port. Without this check it could
// drive the endpoint and read every repository drover holds.
func TestHTTPRejectsForeignOrigin(t *testing.T) {
	srv := engine(t, dataDirWithRepo(t))

	for _, origin := range []string{"https://evil.example.com", "http://attacker.test:8080"} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+server.MCPPath,
			strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.Header.Set("Origin", origin)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("origin %q got status %d, want 403", origin, resp.StatusCode)
		}
	}
}

// A loopback origin is a legitimate local client page, and no Origin at all is
// a direct client like curl or an agent's HTTP library.
func TestHTTPAllowsLoopbackAndAbsentOrigin(t *testing.T) {
	srv := engine(t, dataDirWithRepo(t))

	for _, origin := range []string{"", "http://localhost:3000", "http://127.0.0.1:9999", "http://[::1]:1234"} {
		headers := map[string]string{}
		if origin != "" {
			headers["Origin"] = origin
		}
		code, _ := rpc(t, srv, `{"jsonrpc":"2.0","id":1,"method":"ping"}`, headers)
		if code != http.StatusOK {
			t.Errorf("origin %q got status %d, want 200", origin, code)
		}
	}
}

// GET would be a server-initiated stream; drover never initiates anything.
func TestHTTPGetIsNotAllowed(t *testing.T) {
	srv := engine(t, t.TempDir())
	resp, err := srv.Client().Get(srv.URL + server.MCPPath)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET status %d, want 405", resp.StatusCode)
	}
	if allow := resp.Header.Get("Allow"); !strings.Contains(allow, "POST") {
		t.Errorf("Allow = %q", allow)
	}
}

// A client ending a session is being well-behaved, even though drover keeps
// no session state.
func TestHTTPDeleteIsClean(t *testing.T) {
	srv := engine(t, t.TempDir())
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+server.MCPPath, nil)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("DELETE status %d, want 204", resp.StatusCode)
	}
}

func TestHTTPMalformedJSON(t *testing.T) {
	srv := engine(t, t.TempDir())
	code, out := rpc(t, srv, `{not json`, nil)
	if code != http.StatusOK {
		t.Fatalf("status %d, want a JSON-RPC error body", code)
	}
	e, ok := out["error"].(map[string]any)
	if !ok {
		t.Fatalf("want an error: %v", out)
	}
	if int(e["code"].(float64)) != -32700 {
		t.Errorf("code = %v, want a parse error", e["code"])
	}
}

func TestHTTPEmptyBody(t *testing.T) {
	srv := engine(t, t.TempDir())
	code, out := rpc(t, srv, "", nil)
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if _, ok := out["error"]; !ok {
		t.Errorf("an empty body should be an error: %v", out)
	}
}

// The engine's own REST API must still work with MCP mounted beside it.
func TestRESTAPIStillWorks(t *testing.T) {
	srv := engine(t, dataDirWithRepo(t))
	resp, err := srv.Client().Get(srv.URL + "/apis/drover/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status endpoint returned %d", resp.StatusCode)
	}

	var body bytes.Buffer
	_, _ = body.ReadFrom(resp.Body)
	if !strings.Contains(body.String(), "dataDir") {
		t.Errorf("status body = %q", body.String())
	}
}
