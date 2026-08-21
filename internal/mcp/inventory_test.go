package mcp_test

import (
	"strings"
	"testing"

	"github.com/notshekhar/drover/internal/docs"
)

// initialize's instructions are the one thing every client shows the model
// before it does anything. A static string can explain which tool answers
// which question, but only a running engine knows that the repositories are
// called `api` and `web` -- and an agent that is not told spends its first
// call finding out, or guesses a path and reads a missing-file error.
func TestInstructionsNameWhatTheEngineHolds(t *testing.T) {
	dir := t.TempDir()
	applyDoc(t, dir, `
apiVersion: drover/v1
kind: Repository
metadata:
  name: api
spec:
  url: https://example.com/acme/api.git
  branch: main
  refreshInterval: never
`)

	s := start(t, dir)
	instructions := initInstructions(t, s)

	// The static half must survive: it is what tells the model that lsp
	// answers a question grep cannot.
	for _, want := range []string{"ls, read, grep and find", "Prefer lsp over grep"} {
		if !strings.Contains(instructions, want) {
			t.Errorf("the static guidance lost %q", want)
		}
	}

	for _, want := range []string{"REPOSITORIES", "api", "main"} {
		if !strings.Contains(instructions, want) {
			t.Errorf("instructions never mention %q:\n%s", want, instructions)
		}
	}

	// Instructions are sent once and then go stale, so they must say where to
	// look for the current answer.
	if !strings.Contains(instructions, "drover://inventory") {
		t.Error("nothing points at the resource that refreshes this")
	}
}

// Knowing which tool does what is not the same as knowing when to reach for
// any of them. A model that is told only the mechanics uses drover when it is
// already looking at a path, and answers from memory the rest of the time --
// which is the case drover exists for.
func TestInstructionsSayWhatDroverIsFor(t *testing.T) {
	s := start(t, t.TempDir())
	instructions := initInstructions(t, s)

	for _, want := range []string{
		"NOT the code you are editing", // why it is worth calling at all
		"PRD",                          // grounding a plan in the real thing
		"Debugging",                    // following a failure across a boundary
		"Checking a claim",
	} {
		if !strings.Contains(instructions, want) {
			t.Errorf("instructions never mention %q:\n%s", want, instructions)
		}
	}

	// The one promise that must never drift. Every tool reads; a model that
	// believed otherwise would plan an edit it cannot make and report work it
	// never did.
	if !strings.Contains(instructions, "every drover tool reads") {
		t.Error("instructions do not state that nothing here writes")
	}
}

// An environment's names orient the model; its values are the thing the whole
// placeholder design exists to keep away from it. A preamble that printed one
// would be a way of asking for a secret without calling a tool.
func TestInstructionsNeverCarryEnvironmentValues(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DROVER_TEST_TOKEN", "sk-live-do-not-leak")
	applyDoc(t, dir, `
apiVersion: drover/v1
kind: Environment
metadata:
  name: prod
spec:
  variables:
    baseUrl: https://api.acme.internal
  secrets:
    token: ${DROVER_TEST_TOKEN}
`)

	s := start(t, dir)
	instructions := initInstructions(t, s)

	if !strings.Contains(instructions, "prod") {
		t.Errorf("the environment's name is missing:\n%s", instructions)
	}
	for _, leak := range []string{"sk-live-do-not-leak", "api.acme.internal", "DROVER_TEST_TOKEN"} {
		if strings.Contains(instructions, leak) {
			t.Errorf("instructions leaked %q:\n%s", leak, instructions)
		}
	}
}

// `drover mcp` is routinely started before `drover serve` -- an agent spawns it
// on its own schedule. Failing initialize because the inventory could not be
// fetched would turn a vague preamble into no session at all.
func TestInitializeSurvivesAnUnreachableEngine(t *testing.T) {
	s := startDetached(t)
	instructions := initInstructions(t, s)

	if !strings.Contains(instructions, "ls, read, grep and find") {
		t.Errorf("the static guidance did not survive an unreachable engine:\n%s", instructions)
	}
	if strings.Contains(instructions, "This engine currently holds") {
		t.Error("an unreachable engine reported an inventory")
	}
}

// The reference is served from the binary, never from ~/.drover/docs.md. That
// file is written once and deliberately never overwritten, so a data directory
// created by an older drover still describes that older drover; the embedded
// copy always matches whatever is answering.
func TestResourcesServeTheReferenceAndTheInventory(t *testing.T) {
	dir := t.TempDir()
	applyDoc(t, dir, `
apiVersion: drover/v1
kind: Repository
metadata:
  name: api
spec:
  url: https://example.com/acme/api.git
  branch: main
  refreshInterval: never
`)
	s := start(t, dir)
	s.call("initialize", map[string]any{"protocolVersion": "2024-11-05"})

	listed := s.call("resources/list", nil)
	result, ok := listed["result"].(map[string]any)
	if !ok {
		t.Fatalf("resources/list returned %v", listed)
	}
	items, _ := result["resources"].([]any)
	if len(items) != 2 {
		t.Fatalf("resources/list returned %d resources, want 2", len(items))
	}

	if body := readResource(t, s, "drover://reference"); body != docs.Markdown {
		t.Error("drover://reference is not the embedded reference")
	}
	if body := readResource(t, s, "drover://inventory"); !strings.Contains(body, "api") {
		t.Errorf("drover://inventory does not mention the repository:\n%s", body)
	}

	// A resource that does not exist is a call that could not be made, so it
	// is a protocol error rather than a result the model should read.
	resp := s.call("resources/read", map[string]any{"uri": "drover://nope"})
	if _, ok := resp["error"]; !ok {
		t.Errorf("an unknown resource returned a result: %v", resp)
	}
}

// listChanged is a promise to push a message. Only stdio can keep it: the HTTP
// endpoint answers GET with 405, so a client there has nowhere to receive one
// and would wait forever for a notification that cannot be sent.
func TestListChangedIsPromisedOnlyWhereItCanBeKept(t *testing.T) {
	s := start(t, t.TempDir())
	if !toolsCapability(t, initCapabilities(t, s))["listChanged"].(bool) {
		t.Error("stdio did not advertise listChanged, which it can deliver")
	}

	if _, ok := toolsCapability(t, httpInitCapabilities(t, t.TempDir()))["listChanged"]; ok {
		t.Error("the HTTP transport promised listChanged, which it cannot deliver")
	}
}

// --- helpers ---

func initInstructions(t *testing.T, s *session) string {
	t.Helper()
	resp := s.call("initialize", map[string]any{"protocolVersion": "2024-11-05"})
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("initialize returned %v", resp)
	}
	instructions, _ := result["instructions"].(string)
	if instructions == "" {
		t.Fatal("initialize returned no instructions at all")
	}
	return instructions
}

func initCapabilities(t *testing.T, s *session) map[string]any {
	t.Helper()
	resp := s.call("initialize", map[string]any{"protocolVersion": "2024-11-05"})
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("initialize returned %v", resp)
	}
	caps, _ := result["capabilities"].(map[string]any)
	return caps
}

func toolsCapability(t *testing.T, caps map[string]any) map[string]any {
	t.Helper()
	tools, ok := caps["tools"].(map[string]any)
	if !ok {
		t.Fatalf("no tools capability in %v", caps)
	}
	return tools
}

func readResource(t *testing.T, s *session, uri string) string {
	t.Helper()
	resp := s.call("resources/read", map[string]any{"uri": uri})
	if e, ok := resp["error"]; ok {
		t.Fatalf("resources/read %s: %v", uri, e)
	}
	result, _ := resp["result"].(map[string]any)
	contents, _ := result["contents"].([]any)
	if len(contents) != 1 {
		t.Fatalf("resources/read %s returned %d contents", uri, len(contents))
	}
	entry, _ := contents[0].(map[string]any)
	if got, _ := entry["uri"].(string); got != uri {
		t.Errorf("resources/read %s answered for %q", uri, got)
	}
	body, _ := entry["text"].(string)
	return body
}

// httpInitCapabilities initializes over the HTTP transport, which is the one
// that cannot push.
func httpInitCapabilities(t *testing.T, dataDir string) map[string]any {
	t.Helper()
	_, body := rpc(t, engine(t, dataDir),
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}`, nil)
	result, ok := body["result"].(map[string]any)
	if !ok {
		t.Fatalf("initialize over HTTP returned %v", body)
	}
	caps, _ := result["capabilities"].(map[string]any)
	return caps
}
