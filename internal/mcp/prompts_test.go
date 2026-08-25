package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/notshekhar/drover/internal/api"
)

// newPromptServer builds a server over a stub that holds one database, so the
// conditional schema prompt is exercised too.
func newPromptServer(t *testing.T) *Server {
	t.Helper()
	return &Server{Backend: &stubBackend{sql: []api.ObjectView{
		{Kind: "SQLConnection", Name: "analytics", Provider: "postgres", Status: "ready"},
	}}, Version: "test"}
}

func promptNames(t *testing.T, s *Server) []string {
	t.Helper()
	out, rpcErr := s.listPrompts(context.Background(), nil)
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Prompts []Prompt `json:"prompts"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range got.Prompts {
		names = append(names, p.Name)
	}
	return names
}

func expandPrompt(t *testing.T, s *Server, name string, args map[string]string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"name": name, "arguments": args})
	if err != nil {
		t.Fatal(err)
	}
	out, rpcErr := s.getPrompt(context.Background(), raw)
	if rpcErr != nil {
		t.Fatalf("prompts/get %s: %v", name, rpcErr)
	}
	body, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Messages []PromptMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 1 {
		t.Fatalf("%s expanded to %d messages", name, len(got.Messages))
	}
	return got.Messages[0].Content.Text
}

func TestPromptsAreListed(t *testing.T) {
	s := newPromptServer(t)
	names := promptNames(t, s)
	if len(names) < 2 {
		t.Fatalf("prompts/list returned %v", names)
	}
	var sawInvestigate, sawOnboard bool
	for _, n := range names {
		switch n {
		case "investigate":
			sawInvestigate = true
		case "onboard":
			sawOnboard = true
		}
	}
	if !sawInvestigate || !sawOnboard {
		t.Errorf("prompts are %v", names)
	}
}

// The investigate prompt exists to write down the order that works. If it
// stops naming the steps in order, it is not doing its job.
func TestInvestigateNamesTheChainInOrder(t *testing.T) {
	s := newPromptServer(t)
	got := expandPrompt(t, s, "investigate", map[string]string{
		"symptom": "webhooks are being rate limited", "repository": "api",
	})
	if !strings.Contains(got, "webhooks are being rate limited") {
		t.Error("the symptom did not reach the prompt")
	}
	if !strings.Contains(got, "Start in the api repository") {
		t.Error("the repository argument did not reach the prompt")
	}
	steps := []string{"grep", "lsp", "git blame", "commits.tsv", "pulls/"}
	last := -1
	for _, step := range steps {
		i := strings.Index(got, step)
		if i < 0 {
			t.Fatalf("the prompt never mentions %q:\n%s", step, got)
		}
		if i < last {
			t.Errorf("%q comes out of order", step)
		}
		last = i
	}
}

func TestOnboardWithoutARepositoryStillExpands(t *testing.T) {
	s := newPromptServer(t)
	got := expandPrompt(t, s, "onboard", nil)
	if !strings.Contains(got, "<repository>") {
		t.Errorf("a missing argument produced:\n%s", got)
	}
}

func TestUnknownPromptIsAnError(t *testing.T) {
	s := newPromptServer(t)
	raw, _ := json.Marshal(map[string]any{"name": "nope"})
	if _, err := s.getPrompt(context.Background(), raw); err == nil {
		t.Fatal("an unknown prompt expanded anyway")
	}
}
