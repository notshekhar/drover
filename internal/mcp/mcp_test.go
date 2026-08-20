package mcp_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/notshekhar/drover/internal/api"
	"github.com/notshekhar/drover/internal/client"
	"github.com/notshekhar/drover/internal/mcp"
	"github.com/notshekhar/drover/internal/server"
)

// session drives a real MCP server over a pipe, the way an agent would.
type session struct {
	t    *testing.T
	in   *io.PipeWriter
	out  *bufio.Reader
	done chan error
	next int
}

// start builds an engine over dataDir, wraps it in an httptest server, and
// runs the MCP bridge against it. Nothing is stubbed: a tool call travels the
// whole path an agent's call would.
func start(t *testing.T, dataDir string) *session {
	t.Helper()

	eng, err := server.New(server.Options{DataDir: dataDir, Version: "test", NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	httpSrv := httptest.NewServer(eng.Handler())
	t.Cleanup(httpSrv.Close)

	clientIn, serverIn := io.Pipe()
	serverOut, clientOut := io.Pipe()

	s := &mcp.Server{Backend: client.New(httpSrv.URL), Version: "test"}
	done := make(chan error, 1)
	go func() { done <- s.Serve(clientIn, clientOut) }()

	sess := &session{t: t, in: serverIn, out: bufio.NewReaderSize(serverOut, 1<<20), done: done}
	t.Cleanup(func() { serverIn.Close() })
	return sess
}

// call sends a request and returns the parsed response.
func (s *session) call(method string, params any) map[string]any {
	s.t.Helper()
	s.next++
	req := map[string]any{"jsonrpc": "2.0", "id": s.next, "method": method}
	if params != nil {
		req["params"] = params
	}
	data, err := json.Marshal(req)
	if err != nil {
		s.t.Fatal(err)
	}
	if _, err := s.in.Write(append(data, '\n')); err != nil {
		s.t.Fatal(err)
	}

	line, err := s.readLine()
	if err != nil {
		s.t.Fatalf("%s: %v", method, err)
	}
	var resp map[string]any
	if err := json.Unmarshal(line, &resp); err != nil {
		s.t.Fatalf("%s: bad response %q: %v", method, line, err)
	}
	// Every reply must carry the id it answers. Checking this on each call is
	// what catches a stream that has desynchronised -- an extra message, or a
	// reply to a notification, shows up here as a mismatched id.
	gotID, ok := resp["id"].(float64)
	if !ok || int(gotID) != s.next {
		s.t.Fatalf("%s: reply id = %v, want %d (the stream is out of step)", method, resp["id"], s.next)
	}
	return resp
}

// notify sends a notification, which must produce no reply at all.
func (s *session) notify(method string) {
	s.t.Helper()
	data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method})
	if _, err := s.in.Write(append(data, '\n')); err != nil {
		s.t.Fatal(err)
	}
}

func (s *session) readLine() ([]byte, error) {
	var buf []byte
	for {
		chunk, isPrefix, err := s.out.ReadLine()
		if err != nil {
			return nil, err
		}
		buf = append(buf, chunk...)
		if !isPrefix {
			return buf, nil
		}
	}
}

// callTool runs a tool and returns its text, plus whether it reported an error.
func (s *session) callTool(name string, args map[string]any) (string, bool) {
	s.t.Helper()
	resp := s.call("tools/call", map[string]any{"name": name, "arguments": args})
	if e, ok := resp["error"]; ok {
		s.t.Fatalf("tools/call %s returned a protocol error: %v", name, e)
	}
	result, ok := resp["result"].(map[string]any)
	if !ok {
		s.t.Fatalf("tools/call %s: no result in %v", name, resp)
	}

	var sb strings.Builder
	if content, ok := result["content"].([]any); ok {
		for _, c := range content {
			if m, ok := c.(map[string]any); ok {
				if t, ok := m["text"].(string); ok {
					sb.WriteString(t)
				}
			}
		}
	}
	isErr, _ := result["isError"].(bool)
	return sb.String(), isErr
}

// --- fixtures ---

func dataDirWithRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(dir, "repos", rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("api/main.go", "package main\n\nfunc main() {\n\tserve()\n}\n")
	write("api/internal/db.go", "package internal\n\n// TODO: pooling\nfunc Connect() {}\n")
	write("api/README.md", "# api\n")
	return dir
}

func applyDoc(t *testing.T, dataDir, doc string) {
	t.Helper()
	eng, err := server.New(server.Options{DataDir: dataDir, Version: "test", NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(api.ApplyRequest{Documents: []api.Document{{Source: "/work/test.yaml", Data: doc}}})
	req := httptest.NewRequest(http.MethodPost, api.Prefix+"/apply", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	eng.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("apply failed: %s", rec.Body)
	}
}

// --- protocol ---

func TestInitialize(t *testing.T) {
	s := start(t, t.TempDir())
	resp := s.call("initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"clientInfo":      map[string]any{"name": "test", "version": "1"},
	})
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %v", resp)
	}
	if result["protocolVersion"] != "2024-11-05" {
		t.Errorf("protocolVersion = %v", result["protocolVersion"])
	}
	info, _ := result["serverInfo"].(map[string]any)
	if info["name"] != "drover" {
		t.Errorf("serverInfo = %v", info)
	}
	caps, _ := result["capabilities"].(map[string]any)
	if _, ok := caps["tools"]; !ok {
		t.Errorf("capabilities = %v, want tools declared", caps)
	}
	if _, ok := result["instructions"].(string); !ok {
		t.Error("no instructions; the model gets no orientation")
	}
}

// A client asking for a newer revision we support should get that revision
// back, not ours.
func TestInitializeEchoesSupportedVersion(t *testing.T) {
	s := start(t, t.TempDir())
	resp := s.call("initialize", map[string]any{"protocolVersion": "2025-06-18"})
	result := resp["result"].(map[string]any)
	if result["protocolVersion"] != "2025-06-18" {
		t.Errorf("protocolVersion = %v, want the client's version echoed", result["protocolVersion"])
	}

	// And an unknown one falls back to ours rather than failing.
	resp = s.call("initialize", map[string]any{"protocolVersion": "1999-01-01"})
	result = resp["result"].(map[string]any)
	if result["protocolVersion"] != mcp.ProtocolVersion {
		t.Errorf("protocolVersion = %v, want the fallback %q", result["protocolVersion"], mcp.ProtocolVersion)
	}
}

// Replying to a notification is a protocol violation, and a client that gets
// an unexpected message will desynchronise.
func TestNotificationGetsNoReply(t *testing.T) {
	s := start(t, t.TempDir())
	s.notify("notifications/initialized")
	s.notify("notifications/somethingUnknown")

	// call checks that the reply carries this request's id, so if either
	// notification had been answered the extra message would surface here as a
	// mismatch rather than being silently consumed.
	if _, ok := s.call("ping", nil)["result"]; !ok {
		t.Error("ping did not return a result")
	}
}

func TestUnknownMethod(t *testing.T) {
	s := start(t, t.TempDir())
	resp := s.call("no/such/method", nil)
	e, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("want an error, got %v", resp)
	}
	if int(e["code"].(float64)) != mcp.CodeMethodNotFound {
		t.Errorf("code = %v, want %d", e["code"], mcp.CodeMethodNotFound)
	}
}

func TestUnknownToolIsAProtocolError(t *testing.T) {
	s := start(t, t.TempDir())
	resp := s.call("tools/call", map[string]any{"name": "nope"})
	if _, ok := resp["error"]; !ok {
		t.Errorf("an unknown tool should be a protocol error, got %v", resp)
	}
}

// --- file tools ---

func TestListToolsIncludesFileTools(t *testing.T) {
	s := start(t, t.TempDir())
	resp := s.call("tools/list", nil)
	result := resp["result"].(map[string]any)
	tools := result["tools"].([]any)

	found := map[string]bool{}
	for _, tool := range tools {
		m := tool.(map[string]any)
		found[m["name"].(string)] = true

		// Every tool needs a description and a schema, or a model cannot use it.
		if m["description"] == "" {
			t.Errorf("tool %v has no description", m["name"])
		}
		if _, ok := m["inputSchema"].(map[string]any); !ok {
			t.Errorf("tool %v has no inputSchema", m["name"])
		}
	}
	for _, want := range []string{"ls", "read", "grep", "find"} {
		if !found[want] {
			t.Errorf("tool %q is missing", want)
		}
	}
}

func TestLsTool(t *testing.T) {
	s := start(t, dataDirWithRepo(t))

	// No path lists the repositories, which is how an agent discovers them.
	out, isErr := s.callTool("ls", nil)
	if isErr {
		t.Fatalf("ls failed: %s", out)
	}
	if !strings.Contains(out, "api") {
		t.Errorf("ls = %q, want the repository listed", out)
	}

	out, isErr = s.callTool("ls", map[string]any{"path": "api"})
	if isErr {
		t.Fatalf("ls api failed: %s", out)
	}
	for _, want := range []string{"internal/", "main.go", "README.md"} {
		if !strings.Contains(out, want) {
			t.Errorf("ls api = %q, want it to contain %q", out, want)
		}
	}
}

func TestReadTool(t *testing.T) {
	s := start(t, dataDirWithRepo(t))
	out, isErr := s.callTool("read", map[string]any{"path": "api/main.go"})
	if isErr {
		t.Fatalf("read failed: %s", out)
	}
	if !strings.Contains(out, "func main()") {
		t.Errorf("read = %q", out)
	}
	// Line numbers, since the next thing a model does is cite a line.
	if !strings.Contains(out, "     1\t") {
		t.Errorf("read = %q, want numbered lines", out)
	}
}

func TestReadToolWindow(t *testing.T) {
	s := start(t, dataDirWithRepo(t))
	out, isErr := s.callTool("read", map[string]any{"path": "api/main.go", "offset": 3, "limit": 1})
	if isErr {
		t.Fatalf("read failed: %s", out)
	}
	if strings.Contains(out, "package main") {
		t.Errorf("read = %q, want it to start at line 3", out)
	}
	// The model must be told how to get the rest.
	if !strings.Contains(out, "offset 4") {
		t.Errorf("read = %q, want it to say how to continue", out)
	}
}

func TestGrepTool(t *testing.T) {
	s := start(t, dataDirWithRepo(t))
	out, isErr := s.callTool("grep", map[string]any{"pattern": "TODO"})
	if isErr {
		t.Fatalf("grep failed: %s", out)
	}
	if !strings.Contains(out, "api/internal/db.go:3") {
		t.Errorf("grep = %q, want a path:line hit", out)
	}

	out, _ = s.callTool("grep", map[string]any{"pattern": "definitelynotpresent"})
	if !strings.Contains(out, "No matches") {
		t.Errorf("grep = %q, want a clear empty result", out)
	}
}

func TestFindTool(t *testing.T) {
	s := start(t, dataDirWithRepo(t))
	out, isErr := s.callTool("find", map[string]any{"pattern": "*.go"})
	if isErr {
		t.Fatalf("find failed: %s", out)
	}
	if !strings.Contains(out, "api/main.go") || !strings.Contains(out, "api/internal/db.go") {
		t.Errorf("find = %q", out)
	}
}

// The jail must hold through the whole stack, not just in the files package.
func TestFileToolsRefuseEscape(t *testing.T) {
	s := start(t, dataDirWithRepo(t))
	for _, tool := range []string{"ls", "read"} {
		out, isErr := s.callTool(tool, map[string]any{"path": "../../etc/passwd"})
		if !isErr {
			t.Errorf("%s escaped the root: %s", tool, out)
		}
		if !strings.Contains(out, "outside the repository root") {
			t.Errorf("%s error = %q, want it to say why", tool, out)
		}
	}
}

// A tool that runs and fails reports isError, rather than a JSON-RPC error --
// the model should read the reason and try something else.
func TestToolFailureIsAToolError(t *testing.T) {
	s := start(t, dataDirWithRepo(t))
	out, isErr := s.callTool("read", map[string]any{"path": "api/nope.go"})
	if !isErr {
		t.Errorf("reading a missing file was not reported as an error: %s", out)
	}
	if !strings.Contains(out, "does not exist") {
		t.Errorf("error text = %q", out)
	}

	out, isErr = s.callTool("grep", map[string]any{"pattern": "(["})
	if !isErr {
		t.Errorf("an invalid regexp was not reported as an error: %s", out)
	}
}

func TestToolsNeedTheirRequiredArguments(t *testing.T) {
	s := start(t, dataDirWithRepo(t))
	if out, isErr := s.callTool("read", map[string]any{}); !isErr {
		t.Errorf("read with no path succeeded: %s", out)
	}
	if out, isErr := s.callTool("grep", map[string]any{}); !isErr {
		t.Errorf("grep with no pattern succeeded: %s", out)
	}
}

// --- object tools ---

const envDoc = `apiVersion: drover/v1
kind: Environment
metadata:
  name: prod
spec:
  variables:
    baseUrl: https://api.example.com
`

func httpRequestDoc(method string) string {
	return fmt.Sprintf(`apiVersion: drover/v1
kind: HTTPRequest
metadata:
  name: get-user
spec:
  description: Fetch one user by id.
  method: %s
  url: "{{baseUrl}}/users/{userId}"
  environments: [prod]
  defaultEnvironment: prod
  pathParams:
    - name: userId
      description: The user's opaque id.
      required: true
      example: usr_1a2b
`, method)
}

// The tool set is fixed. Twenty requests must not become twenty tools.
func TestToolCountDoesNotGrowWithObjects(t *testing.T) {
	dir := dataDirWithRepo(t)
	applyDoc(t, dir, envDoc)
	for i := 0; i < 12; i++ {
		doc := strings.Replace(httpRequestDoc("GET"), "name: get-user", fmt.Sprintf("name: req-%d", i), 1)
		applyDoc(t, dir, doc)
	}

	s := start(t, dir)
	resp := s.call("tools/list", nil)
	tools := resp["result"].(map[string]any)["tools"].([]any)

	var names []string
	for _, x := range tools {
		names = append(names, x.(map[string]any)["name"].(string))
	}
	want := []string{"ls", "read", "grep", "find", "git", "lsp", "api_list", "api_describe", "api_call"}
	if len(names) != len(want) {
		t.Fatalf("12 requests produced %d tools (%v); the set must stay fixed", len(names), names)
	}
	for _, w := range want {
		if !slicesContains(names, w) {
			t.Errorf("tool %q is missing from %v", w, names)
		}
	}
	for _, n := range names {
		if strings.HasPrefix(n, "call_") || strings.HasPrefix(n, "query_") {
			t.Errorf("a per-object tool survived: %q", n)
		}
	}
}

func slicesContains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// api_list is how a model discovers what it can call, so it must show the
// request AND the environments, and its fuzzy search must actually filter.
func TestAPIListAndSearch(t *testing.T) {
	dir := dataDirWithRepo(t)
	applyDoc(t, dir, envDoc)
	applyDoc(t, dir, httpRequestDoc("GET"))
	applyDoc(t, dir, strings.Replace(
		strings.Replace(httpRequestDoc("GET"), "name: get-user", "name: list-orders", 1),
		"Fetch one user by id.", "List the orders on an account.", 1))

	s := start(t, dir)

	out, isErr := s.callTool("api_list", nil)
	if isErr {
		t.Fatalf("api_list failed: %s", out)
	}
	// An index: names, summaries, parameter names and environments. The
	// parameter *descriptions* belong to api_describe.
	for _, want := range []string{"get-user", "list-orders", "Fetch one user by id", "userId", "environment", "prod"} {
		if !strings.Contains(out, want) {
			t.Errorf("api_list output is missing %q:\n%s", want, out)
		}
	}

	// Search by a word that appears only in one request's description, which
	// proves the haystack is more than the name.
	out, _ = s.callTool("api_list", map[string]any{"search": "orders"})
	if !strings.Contains(out, "list-orders") {
		t.Errorf("search did not find the matching request:\n%s", out)
	}
	if strings.Contains(out, "get-user\n") {
		t.Errorf("search returned a request it should have filtered out:\n%s", out)
	}

	// A search that matches nothing says so, and says how to see everything.
	out, _ = s.callTool("api_list", map[string]any{"search": "zzzznothing"})
	if !strings.Contains(out, "No request matches") {
		t.Errorf("an empty search result is unclear:\n%s", out)
	}
}

// api_describe is what a model reads before filling a call in, so the
// parameter descriptions the document insists on have to reach it.
func TestAPIDescribe(t *testing.T) {
	dir := dataDirWithRepo(t)
	applyDoc(t, dir, envDoc)
	applyDoc(t, dir, httpRequestDoc("GET"))

	s := start(t, dir)
	out, isErr := s.callTool("api_describe", map[string]any{"request": "get-user"})
	if isErr {
		t.Fatalf("api_describe failed: %s", out)
	}
	for _, want := range []string{"get-user", "Fetch one user by id", "userId", "The user's opaque id", "usr_1a2b", "api_call"} {
		if !strings.Contains(out, want) {
			t.Errorf("api_describe is missing %q:\n%s", want, out)
		}
	}

	if out, isErr := s.callTool("api_describe", map[string]any{"request": "nope"}); !isErr {
		t.Errorf("describing an unknown request succeeded: %s", out)
	}
}

// A non-GET request is stored but must never be reachable from here, whether
// through the tool list or by naming it directly.
func TestNonGetIsNotReachable(t *testing.T) {
	dir := dataDirWithRepo(t)
	applyDoc(t, dir, envDoc)
	applyDoc(t, dir, httpRequestDoc("POST"))

	s := start(t, dir)

	out, _ := s.callTool("api_list", nil)
	if strings.Contains(out, "get-user") {
		t.Errorf("a POST request was listed:\n%s", out)
	}
	out, isErr := s.callTool("api_describe", map[string]any{"request": "get-user"})
	if !isErr {
		t.Errorf("api_describe exposed a POST request: %s", out)
	}
	if !strings.Contains(out, "only GET") {
		t.Errorf("the refusal does not explain itself: %s", out)
	}
}

// A SQLConnection that has not passed its health check is not offered, which
// is the gate the design asks for.
func TestUnhealthySQLConnectionIsNotAdvertised(t *testing.T) {
	dir := dataDirWithRepo(t)
	applyDoc(t, dir, `apiVersion: drover/v1
kind: SQLConnection
metadata:
  name: analytics
spec:
  provider: postgres
  url: ${DROVER_TEST_ABSENT_URL}
  health: SELECT 1
`)

	s := start(t, dir)
	resp := s.call("tools/list", nil)
	for _, x := range resp["result"].(map[string]any)["tools"].([]any) {
		if name := x.(map[string]any)["name"].(string); name == "sql_query" {
			t.Error("sql_query was offered with no healthy connection behind it")
		}
	}
}

// The search haystack includes parameter descriptions, so a model searching
// for what a parameter means finds the request that has it.
func TestAPISearchMatchesParameterDescriptions(t *testing.T) {
	dir := dataDirWithRepo(t)
	applyDoc(t, dir, envDoc)
	applyDoc(t, dir, httpRequestDoc("GET"))
	applyDoc(t, dir, strings.Replace(
		strings.Replace(httpRequestDoc("GET"), "name: get-user", "name: get-thing", 1),
		"The user's opaque id.", "A widget serial number.", 1))

	s := start(t, dir)
	out, _ := s.callTool("api_list", map[string]any{"search": "widget serial"})
	if !strings.Contains(out, "get-thing") {
		t.Errorf("searching a parameter description did not find its request:\n%s", out)
	}
	if strings.Contains(out, "get-user\n") {
		t.Errorf("the search did not filter:\n%s", out)
	}
}
