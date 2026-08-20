package mcp

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/notshekhar/drover/internal/api"
	"github.com/notshekhar/drover/internal/object"
)

// Backend is everything the MCP layer needs from drover.
//
// It exists so the same tool code serves two callers: `drover serve`, which
// wires it straight to its own store and executors, and `drover mcp`, which
// wires it to an HTTP client pointed at an engine elsewhere. Without it the
// in-process transport would have to call itself over loopback HTTP.
type Backend interface {
	List(kind object.Kind) ([]api.ObjectView, error)
	Get(kind object.Kind, name string) (*api.ObjectView, error)

	Ls(req api.LsRequest) (*api.LsResponse, error)
	ReadFile(req api.ReadRequest) (*api.ReadResponse, error)
	Grep(req api.GrepRequest) (*api.GrepResponse, error)
	Find(req api.FindRequest) (*api.FindResponse, error)

	Git(req api.GitRequest) (*api.GitResponse, error)
	LSP(req api.LSPRequest) (*api.LSPResponse, error)

	Call(name string, req api.CallRequest) (*api.CallResponse, error)
	Query(name, query string) (*api.QueryResponse, error)
}

// ProtocolVersion is the MCP revision drover implements.
const ProtocolVersion = "2024-11-05"

// supportedVersions are the revisions drover will agree to. A client asking
// for one of these gets it echoed back; anything else gets our default, which
// is what the spec asks for.
var supportedVersions = map[string]bool{
	"2024-11-05": true,
	"2025-03-26": true,
	"2025-06-18": true,
}

// Server serves drover's objects as MCP tools.
//
// It holds no state of its own: every call goes to the engine over HTTP, so
// the tool list reflects whatever has been applied without a restart, and the
// engine can be on another machine.
type Server struct {
	Backend Backend
	Version string

	// langCache holds what the lsp tool's description says about which
	// languages can start here, so working it out does not happen on every
	// tools/list.
	langMu    sync.Mutex
	langCache string
	langAt    time.Time
}

// Tool is one advertised tool.
type Tool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema Schema `json:"inputSchema"`
}

// Schema is a JSON Schema object describing a tool's arguments.
type Schema struct {
	Type       string          `json:"type"`
	Properties map[string]Prop `json:"properties,omitempty"`
	Required   []string        `json:"required,omitempty"`
	Additional *bool           `json:"additionalProperties,omitempty"`
}

// Prop is one argument.
type Prop struct {
	Type        string   `json:"type"`
	Description string   `json:"description,omitempty"`
	Enum        []string `json:"enum,omitempty"`
	Default     any      `json:"default,omitempty"`
}

// Content is one piece of a tool result.
type Content struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// CallResult is what a tool call returns.
//
// A tool that fails sets IsError and puts the reason in the text, rather than
// returning a JSON-RPC error. That is the MCP convention: a protocol error
// means the call could not be made, while a tool error is a result the model
// should read and react to.
type CallResult struct {
	Content []Content `json:"content"`
	IsError bool      `json:"isError,omitempty"`
}

func text(format string, args ...any) *CallResult {
	return &CallResult{Content: []Content{{Type: "text", Text: fmt.Sprintf(format, args...)}}}
}

func toolError(format string, args ...any) *CallResult {
	return &CallResult{Content: []Content{{Type: "text", Text: fmt.Sprintf(format, args...)}}, IsError: true}
}

// router builds the method table. Both transports use it, so neither can
// support a method the other does not.
func (s *Server) router() *router {
	r := newRouter()

	r.handle("initialize", s.initialize)
	r.handle("notifications/initialized", func(json.RawMessage) (any, *rpcError) { return nil, nil })
	r.handle("ping", func(json.RawMessage) (any, *rpcError) { return map[string]any{}, nil })
	r.handle("tools/list", s.listTools)
	r.handle("tools/call", s.callTool)

	// Declared but empty, so a client that asks does not get a method-not-found
	// it has to special-case.
	r.handle("resources/list", func(json.RawMessage) (any, *rpcError) {
		return map[string]any{"resources": []any{}}, nil
	})
	r.handle("prompts/list", func(json.RawMessage) (any, *rpcError) {
		return map[string]any{"prompts": []any{}}, nil
	})
	return r
}

// Serve runs the MCP server over the given streams until input closes. This is
// the stdio transport, which is what an agent spawning a process speaks.
func (s *Server) Serve(in io.Reader, out io.Writer) error {
	return newConn(in, out, s.router()).serve()
}

func (s *Server) initialize(params json.RawMessage) (any, *rpcError) {
	var req struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(params, &req)

	version := ProtocolVersion
	if supportedVersions[req.ProtocolVersion] {
		version = req.ProtocolVersion
	}

	return map[string]any{
		"protocolVersion": version,
		"capabilities": map[string]any{
			// listChanged: the tool list really does change when someone
			// applies a new object, and a client that re-lists will see it.
			"tools": map[string]any{"listChanged": true},
		},
		"serverInfo": map[string]any{
			"name":    "drover",
			"version": s.Version,
		},
		"instructions": instructions,
	}, nil
}

const instructions = `drover holds git checkouts, HTTP requests and database connections that have been applied to it.

Use ls, read, grep and find to explore the repositories it holds. Paths are relative to the repository root, so they start with the repository name: ` + "`api/src/main.go`" + `. Call ls with no path to see which repositories are available.

Prefer grep and find to locate code, then read the specific file. These are real files on disk, not a search index, so the results are exact.

The git tool answers the other half of the question. grep and read see the tree as it is now; git sees how it got that way -- commits, diffs, blame, who wrote something and when, and what a file looked like at any revision. It is read-only and never touches the network.

Prefer lsp over grep for anything about a symbol: it knows the difference between a definition and a mention of the same word in a comment. grep is still the right tool for a string, a config value or a language with no server.

For APIs: api_list finds a configured HTTP request (it takes a fuzzy search and also lists the environments), api_describe shows one request's parameters, and api_call performs it. Only GET requests are available.

For databases: sql_query runs one read-only statement against a named connection. The connections are listed in that tool's own description.`

// --- tools/list ---

// The tool set is FIXED at ten, whatever is applied.
//
// One tool per object does not survive contact with a real collection: twenty
// requests became twenty tools, every one of them re-sent on every tools/list
// and all of them competing for the model's attention. The catalogue belongs
// in a tool's arguments, not in the tool list.
func (s *Server) listTools(json.RawMessage) (any, *rpcError) {
	tools := append([]Tool(nil), fileTools...)
	tools = append(tools, s.gitTools()...)
	tools = append(tools, s.lspTools()...)
	tools = append(tools, s.apiTools()...)
	tools = append(tools, s.sqlTools()...)
	return map[string]any{"tools": tools}, nil
}

func boolPtr(b bool) *bool { return &b }

var fileTools = []Tool{
	{
		Name:        "ls",
		Description: "List a directory in the repositories drover holds. Call with no path to see which repositories are available; then use paths like `api` or `api/internal`.",
		InputSchema: Schema{
			Type:       "object",
			Additional: boolPtr(false),
			Properties: map[string]Prop{
				"path": {Type: "string", Description: "Directory relative to the repository root, e.g. `api/internal`. Omit for the list of repositories."},
			},
		},
	},
	{
		Name:        "read",
		Description: "Read a file from a repository. Returns the text with its line numbers, so you can read a large file in windows using offset and limit.",
		InputSchema: Schema{
			Type:       "object",
			Additional: boolPtr(false),
			Required:   []string{"path"},
			Properties: map[string]Prop{
				"path":   {Type: "string", Description: "File relative to the repository root, e.g. `api/internal/db.go`."},
				"offset": {Type: "integer", Description: "1-based line to start at. Omit to start at the beginning."},
				"limit":  {Type: "integer", Description: "How many lines to return. Omit for as much as fits."},
			},
		},
	},
	{
		Name:        "grep",
		Description: "Search file contents across the repositories with a regular expression. Use this to find where something is defined or used, then read that file.",
		InputSchema: Schema{
			Type:       "object",
			Additional: boolPtr(false),
			Required:   []string{"pattern"},
			Properties: map[string]Prop{
				"pattern":       {Type: "string", Description: "Go/RE2 regular expression, e.g. `func NewServer` or `TODO|FIXME`."},
				"path":          {Type: "string", Description: "Limit the search to this subtree, e.g. `api/internal`."},
				"include":       {Type: "string", Description: "Only search files whose name matches this glob, e.g. `*.go`."},
				"caseSensitive": {Type: "boolean", Description: "Match case exactly. Defaults to false.", Default: false},
				"maxResults":    {Type: "integer", Description: "Cap the number of matches returned."},
			},
		},
	},
	{
		Name:        "find",
		Description: "Find files by name or path pattern. A pattern without a slash matches the file name (`*.go`); one with a slash matches the whole path (`api/internal/*.go`).",
		InputSchema: Schema{
			Type:       "object",
			Additional: boolPtr(false),
			Required:   []string{"pattern"},
			Properties: map[string]Prop{
				"pattern":    {Type: "string", Description: "Glob, e.g. `*_test.go` or `api/cmd/*`."},
				"path":       {Type: "string", Description: "Limit the search to this subtree."},
				"maxResults": {Type: "integer", Description: "Cap the number of paths returned."},
			},
		},
	},
}

// apiTools are the three that cover every configured HTTP request.
//
// api_call's description stays the same length whether one request is
// configured or two hundred; discovery happens through api_list.
func (s *Server) apiTools() []Tool {
	count, envs := s.apiCounts()
	if count == 0 {
		// Nothing configured: advertising tools that can only fail wastes a
		// slot and invites the model to try them.
		return nil
	}

	return []Tool{
		{
			Name: "api_list",
			Description: fmt.Sprintf(
				"List the HTTP requests drover can perform, and the environments they run against. "+
					"%d request(s) and %d environment(s) are configured. "+
					"Call this first to find the request you need, then api_describe for its parameters, then api_call to perform it. "+
					"With no search argument it returns everything.",
				count, envs),
			InputSchema: Schema{
				Type:       "object",
				Additional: boolPtr(false),
				Properties: map[string]Prop{
					"search": {
						Type:        "string",
						Description: "Optional fuzzy search. Matches against every property of a request -- its name, description, method, url, parameter names and descriptions, and environments -- so `issues`, `github repos` or an abbreviation all work. Omit to list everything.",
					},
				},
			},
		},
		{
			Name:        "api_describe",
			Description: "Show one HTTP request in full: what it does, its url, every parameter with its description and example, which environments it runs against, and which headers it sends. Use this before api_call when you are unsure what a request needs.",
			InputSchema: Schema{
				Type:       "object",
				Additional: boolPtr(false),
				Required:   []string{"request"},
				Properties: map[string]Prop{
					"request": {Type: "string", Description: "Name of the request, as returned by api_list."},
				},
			},
		},
		{
			Name: "api_call",
			Description: "Perform one of the configured HTTP requests and return the response. " +
				"Only GET requests are available here; anything that writes is deliberately not offered. " +
				"Use api_list to find a request and api_describe to see what it needs.",
			InputSchema: Schema{
				Type:       "object",
				Additional: boolPtr(false),
				Required:   []string{"request"},
				Properties: map[string]Prop{
					"request": {Type: "string", Description: "Name of the request, as returned by api_list."},
					"params": {
						Type:        "object",
						Description: "The request's parameters as name/value pairs, e.g. {\"owner\": \"golang\", \"repo\": \"go\"}. See api_describe for which are required.",
					},
					"environment": {Type: "string", Description: "Which environment to run against. Omit to use the request's default."},
				},
			},
		},
	}
}

// apiCounts reports how many callable requests and environments exist, which
// is what the api_list description quotes.
func (s *Server) apiCounts() (requests, environments int) {
	items, err := s.Backend.List(object.KindHTTPRequest)
	if err != nil {
		return 0, 0
	}
	for _, v := range items {
		if v.Safe {
			requests++
		}
	}
	if envs, err := s.Backend.List(object.KindEnvironment); err == nil {
		environments = len(envs)
	}
	return requests, environments
}

// sqlTools is loop's shape: one query tool, with the catalogue of connections
// in its description so the model can pick the right one.
func (s *Server) sqlTools() []Tool {
	items, err := s.Backend.List(object.KindSQLConnection)
	if err != nil {
		return nil
	}

	var ready []api.ObjectView
	for _, v := range items {
		// The health gate: a connection that has not passed is not offered,
		// because an agent handed a database it cannot reach will keep trying.
		if v.Status == "ready" {
			ready = append(ready, v)
		}
	}
	if len(ready) == 0 {
		return nil
	}

	return []Tool{{
		Name: "sql_query",
		Description: "Run a READ-ONLY SQL query against one of the configured databases and return the rows. " +
			"Only SELECT / WITH / SHOW / EXPLAIN / DESCRIBE / VALUES / TABLE statements are allowed -- anything that writes is rejected, and so is sending two statements at once. " +
			"Use information_schema (or the dialect's own catalog) to discover tables and columns before querying, and add a LIMIT to exploratory queries.\n\n" +
			connectionCatalog(ready),
		InputSchema: Schema{
			Type:       "object",
			Additional: boolPtr(false),
			Required:   []string{"connection", "query"},
			Properties: map[string]Prop{
				"connection": {
					Type:        "string",
					Description: "Which database to query -- one of the connections listed in this tool's description.",
					Enum:        connectionNames(ready),
				},
				"query": {Type: "string", Description: "A single read-only SQL statement."},
			},
		},
	}}
}

// connectionCatalog is the list the model reads to choose a connection. The
// dialect is included because Redshift and Postgres are not the same thing to
// somebody writing SQL.
func connectionCatalog(items []api.ObjectView) string {
	var b strings.Builder
	b.WriteString("Available connection values:\n")
	for _, v := range items {
		provider, _ := object.ParseProvider(v.Provider)
		line := fmt.Sprintf("  - %s (%s", v.Name, v.Provider)
		if d := describeFromYAML(v.YAML, "description"); d != "" {
			line += " · " + firstLine(d)
		}
		line += ")"
		if dialect := provider.Dialect(); dialect != "" && dialect != v.Provider {
			line += "\n      dialect: " + dialect
		}
		if v.MaxRows > 0 {
			line += fmt.Sprintf("\n      at most %d rows are returned", v.MaxRows)
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

func connectionNames(items []api.ObjectView) []string {
	out := make([]string, 0, len(items))
	for _, v := range items {
		out = append(out, v.Name)
	}
	sort.Strings(out)
	return out
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
