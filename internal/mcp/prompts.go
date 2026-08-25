package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/notshekhar/drover/internal/object"
)

// MCP has a prompt surface and drover answered it with an empty array, which
// is a whole standard channel left on the floor.
//
// A prompt is user-invoked, which makes it a different thing from a tool
// description: a tool description says what a tool does, a prompt says what
// order to do things in. The three here encode the sequences that actually
// work against this engine -- grep before lsp, lsp before blame, blame before
// the pull request -- which a model will otherwise rediscover, badly, every
// session.
//
// They are generated from what the engine holds rather than written down, so
// they cannot go stale: a prompt that names a repository names one that
// exists, and an engine holding no databases does not offer the schema
// prompt.

// Prompt is one entry in prompts/list.
type Prompt struct {
	Name        string           `json:"name"`
	Description string           `json:"description"`
	Arguments   []PromptArgument `json:"arguments,omitempty"`
}

// PromptArgument is one value the user fills in.
type PromptArgument struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Required    bool   `json:"required,omitempty"`
}

// PromptMessage is one message of an expanded prompt.
type PromptMessage struct {
	Role    string        `json:"role"`
	Content PromptContent `json:"content"`
}

// PromptContent is the text of a message.
type PromptContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func (s *Server) listPrompts(ctx context.Context, _ json.RawMessage) (any, *rpcError) {
	prompts := []Prompt{
		{
			Name:        "investigate",
			Description: "Track a symptom through the code, its history and the argument about it.",
			Arguments: []PromptArgument{
				{Name: "symptom", Description: "What is wrong, in one line.", Required: true},
				{Name: "repository", Description: "Where to start, if you know. Otherwise every checkout is searched."},
			},
		},
		{
			Name:        "onboard",
			Description: "Get oriented in a repository this engine holds.",
			Arguments: []PromptArgument{
				{Name: "repository", Description: "Which checkout.", Required: true},
			},
		},
	}
	if s.hasDatabases(ctx) {
		prompts = append(prompts, Prompt{
			Name:        "schema",
			Description: "Understand a database before querying it.",
			Arguments: []PromptArgument{
				{Name: "connection", Description: "Which database."},
			},
		})
	}
	return map[string]any{"prompts": prompts}, nil
}

func (s *Server) getPrompt(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var req struct {
		Name      string            `json:"name"`
		Arguments map[string]string `json:"arguments"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, errf(CodeInvalidParams, "prompts/get: %v", err)
		}
	}

	var text string
	switch req.Name {
	case "investigate":
		text = investigatePrompt(req.Arguments["symptom"], req.Arguments["repository"])
	case "onboard":
		text = onboardPrompt(req.Arguments["repository"])
	case "schema":
		text = schemaPrompt(req.Arguments["connection"])
	default:
		return nil, errf(CodeInvalidParams, "unknown prompt %q", req.Name)
	}

	return map[string]any{
		"description": req.Name,
		"messages": []PromptMessage{{
			Role:    "user",
			Content: PromptContent{Type: "text", Text: text},
		}},
	}, nil
}

// investigatePrompt is the order that works, written down once.
//
// grep finds the string, lsp finds the symbol, blame finds the commit, the
// commit index finds the pull request, and the pull request is where somebody
// explained themselves. Each step narrows the next; done in any other order,
// most of them return everything.
func investigatePrompt(symptom, repository string) string {
	var b strings.Builder
	b.WriteString("Investigate this, using drover:\n\n")
	if strings.TrimSpace(symptom) != "" {
		fmt.Fprintf(&b, "  %s\n\n", strings.TrimSpace(symptom))
	}
	scope := "across every checkout"
	if strings.TrimSpace(repository) != "" {
		scope = "in " + strings.TrimSpace(repository)
		fmt.Fprintf(&b, "Start in the %s repository.\n\n", strings.TrimSpace(repository))
	}
	fmt.Fprintf(&b, `Work in this order. Each step narrows the next; done in another order
most of them return everything.

1. grep %s for the words in the symptom -- an error string, a route, a
   field name. Read the files that match.
2. lsp to turn a name into the symbol: `+"`definition`"+` for where it comes
   from, `+"`references`"+` for every real use. grep finds text; lsp finds
   the thing.
3. git blame the line that looks wrong, then git show the commit blame
   names. That is when it changed, and by whom.
4. If mirrors/<repository>/index/commits.tsv exists, grep that commit sha
   in it. It gives the pull request number. Read
   mirrors/<repository>/pulls/<number>.md -- that is where somebody
   explained why.
5. Only then say what you think is happening, and say which step you are
   least sure about.

Do not guess a repository name: the connection inventory listed them, and
ls with no path lists them again.
`, scope)
	return b.String()
}

func onboardPrompt(repository string) string {
	name := strings.TrimSpace(repository)
	if name == "" {
		name = "<repository>"
	}
	return fmt.Sprintf(`Get me oriented in the %s repository, using drover.

1. ls %s to see the top level, and read its README if there is one.
2. find in %s for the entry points -- main.go, index.ts, cmd/, app/ --
   and read the one that starts the thing.
3. git status and git log --limit 20 on %s: what is it, how old, who
   works on it, what changed lately.
4. lsp document_symbols on the two or three files that look central.
5. If the connection inventory listed most-read files for %s, read those:
   they are where other people's questions actually landed.

Then tell me, in a short paragraph each: what this service does, how a
request flows through it, and the two or three files I would have to
understand before changing anything.
`, name, name, name, name, name)
}

func schemaPrompt(connection string) string {
	name := strings.TrimSpace(connection)
	if name == "" {
		name = "<connection>"
	}
	return fmt.Sprintf(`Understand the %s database before querying it.

1. read docs/schema/%s.sql. drover dumped it: every table, column,
   foreign key, index and row-count estimate. Do not spend a query
   rediscovering this.
2. Note the row estimates. They are estimates, and they decide whether a
   query is safe to run unbounded.
3. grep docs/schema for REFERENCES to see what points at what before you
   invent a join.
4. Only then use sql_query, read-only, with a LIMIT.

Tell me what the important tables are and how they relate.
`, name, name)
}

// hasDatabases reports whether any SQLConnection is queryable, so the schema
// prompt is not offered by an engine that has no database.
func (s *Server) hasDatabases(ctx context.Context) bool {
	items, err := s.Backend.List(ctx, object.KindSQLConnection)
	return err == nil && len(items) > 0
}
