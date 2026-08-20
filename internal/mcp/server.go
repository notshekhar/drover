package mcp

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

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

Tools named call_* perform a specific HTTP GET that someone has configured. Tools named query_* run a read-only SQL query against a configured database.`

// --- tools/list ---

func (s *Server) listTools(json.RawMessage) (any, *rpcError) {
	tools := append([]Tool(nil), fileTools...)

	// The dynamic half: whatever has been applied. An engine that is
	// unreachable still lists the file tools, because a partial answer beats
	// failing the whole call.
	tools = append(tools, s.requestTools()...)
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

// requestTools advertises every stored HTTPRequest that is a GET.
//
// Non-GET requests are stored but never advertised, so an agent is not handed
// a way to write to somebody's API.
func (s *Server) requestTools() []Tool {
	items, err := s.Backend.List(object.KindHTTPRequest)
	if err != nil {
		return nil
	}

	var out []Tool
	for _, v := range items {
		if !v.Safe {
			continue
		}
		props := map[string]Prop{}
		var required []string

		for _, name := range v.Params {
			props[name] = Prop{Type: "string", Description: "Required parameter."}
			required = append(required, name)
		}
		// The engine knows each parameter's real description; fetch the object
		// so the model gets it rather than a placeholder.
		if detail, err := s.Backend.Get(object.KindHTTPRequest, v.Name); err == nil {
			applyParamDocs(detail, props, &required)
		}
		if len(v.Environments) > 1 {
			props["environment"] = Prop{
				Type:        "string",
				Description: environmentDescription(v),
				Enum:        v.Environments,
			}
		}

		sort.Strings(required)
		out = append(out, Tool{
			Name:        "call_" + toolSuffix(v.Name),
			Description: requestDescription(v),
			InputSchema: Schema{Type: "object", Properties: props, Required: required, Additional: boolPtr(false)},
		})
	}
	return out
}

func environmentDescription(v api.ObjectView) string {
	if v.DefaultEnvironment != "" {
		return fmt.Sprintf("Which environment to run against. Defaults to %s.", v.DefaultEnvironment)
	}
	return "Which environment to run against."
}

func requestDescription(v api.ObjectView) string {
	var b strings.Builder
	if d := describeFromYAML(v.YAML, "description"); d != "" {
		b.WriteString(d)
	} else {
		b.WriteString(fmt.Sprintf("Perform the %s request.", v.Name))
	}
	b.WriteString(fmt.Sprintf("\n\nHTTP %s %s", v.Method, v.URL))
	if len(v.Environments) > 0 {
		b.WriteString("\nEnvironments: " + strings.Join(v.Environments, ", "))
		if v.DefaultEnvironment != "" {
			b.WriteString(" (default " + v.DefaultEnvironment + ")")
		}
	}
	return b.String()
}

// applyParamDocs replaces placeholder descriptions with the ones written in
// the document, which is the whole reason drover insists on them at apply.
func applyParamDocs(v *api.ObjectView, props map[string]Prop, required *[]string) {
	spec, err := specFromYAML(v.YAML)
	if err != nil {
		return
	}
	for _, p := range spec.PathParams {
		prop := props[p.Name]
		prop.Type = "string"
		prop.Description = p.Description
		if p.Example != "" {
			prop.Description += fmt.Sprintf(" Example: %s", p.Example)
		}
		props[p.Name] = prop
	}
	for _, q := range spec.Query {
		prop, existed := props[q.Name]
		prop.Type = "string"
		prop.Description = q.Description
		if q.Example != "" {
			prop.Description += fmt.Sprintf(" Example: %s", q.Example)
		}
		props[q.Name] = prop
		if !existed && q.Required {
			*required = append(*required, q.Name)
		}
	}
}

// sqlTools advertises one query tool per healthy SQLConnection.
//
// The health gate is the point: a connection whose health query has not
// passed is not offered at all, because an agent handed a database it cannot
// reach will keep trying.
func (s *Server) sqlTools() []Tool {
	items, err := s.Backend.List(object.KindSQLConnection)
	if err != nil {
		return nil
	}

	var out []Tool
	for _, v := range items {
		if v.Status != "ready" {
			continue
		}
		out = append(out, Tool{
			Name:        "query_" + toolSuffix(v.Name),
			Description: sqlDescription(v),
			InputSchema: Schema{
				Type:       "object",
				Additional: boolPtr(false),
				Required:   []string{"query"},
				Properties: map[string]Prop{
					"query": {Type: "string", Description: "One read-only SQL statement. Send a single statement, not several separated by semicolons."},
				},
			},
		})
	}
	return out
}

func sqlDescription(v api.ObjectView) string {
	var b strings.Builder
	if d := describeFromYAML(v.YAML, "description"); d != "" {
		b.WriteString(d)
		b.WriteString("\n\n")
	}
	provider, _ := object.ParseProvider(v.Provider)
	b.WriteString(fmt.Sprintf("Run a read-only query against the %s database %q.", v.Provider, v.Name))
	if dialect := provider.Dialect(); dialect != "" {
		b.WriteString("\nDialect: " + dialect)
	}
	if v.ReadOnly {
		b.WriteString("\nOnly SELECT, WITH, SHOW, EXPLAIN, DESCRIBE, VALUES and TABLE are permitted; writes are refused.")
	}
	if v.MaxRows > 0 {
		b.WriteString(fmt.Sprintf("\nAt most %d rows are returned; add LIMIT and filters to narrow the result.", v.MaxRows))
	}
	return b.String()
}

// toolSuffix turns an object name into a tool-name segment. Object names are
// already lowercase alphanumerics and dashes; MCP tool names conventionally
// use underscores.
func toolSuffix(name string) string { return strings.ReplaceAll(name, "-", "_") }

// objectName reverses toolSuffix.
func objectName(suffix string) string { return strings.ReplaceAll(suffix, "_", "-") }
