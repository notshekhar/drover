package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/notshekhar/drover/internal/activity"
	"github.com/notshekhar/drover/internal/api"
	"github.com/notshekhar/drover/internal/object"
)

// Backend is everything the MCP layer needs from drover.
//
// It exists so the same tool code serves two callers: `drover serve`, which
// wires it straight to its own store and executors, and `drover mcp`, which
// wires it to an HTTP client pointed at an engine elsewhere. Without it the
// in-process transport would have to call itself over loopback HTTP.
// Every method takes a context because a tool call is cancellable: an HTTP
// client that hangs up, or an agent that gives up on a slow grep, should stop
// the work rather than leave it running against a machine nobody is waiting
// on. It is also what carries who is calling, for the activity log.
type Backend interface {
	List(ctx context.Context, kind object.Kind) ([]api.ObjectView, error)
	Get(ctx context.Context, kind object.Kind, name string) (*api.ObjectView, error)

	Ls(ctx context.Context, req api.LsRequest) (*api.LsResponse, error)
	ReadFile(ctx context.Context, req api.ReadRequest) (*api.ReadResponse, error)
	Grep(ctx context.Context, req api.GrepRequest) (*api.GrepResponse, error)
	Find(ctx context.Context, req api.FindRequest) (*api.FindResponse, error)

	Git(ctx context.Context, req api.GitRequest) (*api.GitResponse, error)
	LSP(ctx context.Context, req api.LSPRequest) (*api.LSPResponse, error)

	Call(ctx context.Context, name string, req api.CallRequest) (*api.CallResponse, error)
	Query(ctx context.Context, name, query string) (*api.QueryResponse, error)

	// DocWrite is the only write in drover. Stores are the one place an
	// agent may leave something behind.
	DocWrite(ctx context.Context, store string, req api.DocWriteRequest) (*api.DocWriteResponse, error)

	// Hotspots is the behavioural index: which files agents have actually
	// read here. It is on the interface rather than read from the ledger
	// directly because the stdio bridge holds no ledger of its own.
	Hotspots(ctx context.Context) (*api.HotspotsResponse, error)
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

	// invCache holds the rendered inventory, on the same reasoning: building
	// it lists four kinds from the engine, and both initialize and the
	// inventory resource ask for it.
	invMu    sync.Mutex
	invCache string
	invAt    time.Time

	// notify sends a server-initiated message, and is set only by the stdio
	// transport -- the HTTP endpoint answers GET with 405 and so has no way to
	// reach the client between requests. Its presence is what initialize
	// consults before promising tools/listChanged.
	notify    func(method string, params any)
	watchCtx  context.Context
	watchDone chan struct{}

	// hub is the HTTP transport's equivalent of notify: the GET event stream
	// every listener is fanned out to. Set by HTTPHandler, so its presence is
	// what tells initialize an HTTP client can be sent a notification.
	hub      *hub
	watchMu  sync.Mutex
	watching bool

	callerMu   sync.Mutex
	clientName string
	sessionID  string
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
	r.handle("notifications/initialized", func(context.Context, json.RawMessage) (any, *rpcError) {
		// The handshake is over, so a server-initiated message is now legal.
		// Starting the watcher any earlier would put one in the middle of it.
		s.startToolWatch()
		return nil, nil
	})
	r.handle("ping", func(context.Context, json.RawMessage) (any, *rpcError) { return map[string]any{}, nil })
	r.handle("tools/list", s.listTools)
	r.handle("tools/call", s.callTool)

	r.handle("resources/list", s.listResources)
	r.handle("resources/read", s.readResource)

	r.handle("prompts/list", s.listPrompts)
	r.handle("prompts/get", s.getPrompt)
	return r
}

// Serve runs the MCP server over the given streams until input closes. This is
// the stdio transport, which is what an agent spawning a process speaks.
//
// It is also the only transport that can deliver a server-initiated message,
// so it is the only one that advertises tools/listChanged. The notifier is
// installed before the router is built, because initialize reads it to decide
// what to promise.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	c := newConn(in, out, nil)

	done := make(chan struct{})
	defer close(done)

	s.notify = c.notify
	s.watchDone = done
	s.watchCtx = ctx
	c.router = s.router()

	return c.serve(ctx)
}

// startToolWatch launches the tool-list watcher, at most once per connection.
func (s *Server) startToolWatch() {
	if s.notify == nil {
		return
	}
	s.watchMu.Lock()
	defer s.watchMu.Unlock()
	if s.watching {
		// A client may send initialized more than once; a second watcher would
		// double every notification.
		return
	}
	s.watching = true

	// Synchronously, before the goroutine exists. A baseline read inside the
	// goroutine would be read at whatever moment the scheduler chose, and
	// anything applied in between would be folded into it -- invisible from
	// then on, since it would never differ from the baseline again.
	//
	// An unreachable engine leaves this empty on purpose: the first successful
	// read then becomes the baseline, rather than the jump from nothing to
	// everything counting as a change.
	// The watcher outlives the message that started it, so it takes the
	// connection's context, not that message's. Only stdio gets here at all --
	// the HTTP endpoint leaves notify nil and returns above -- so there is no
	// case where this is a request context that dies in a second.
	ctx := s.watchCtx
	if ctx == nil {
		ctx = context.Background()
	}
	baseline, _ := s.toolFingerprint(ctx)
	go s.watchToolChanges(ctx, s.notify, baseline, s.watchDone, nil)
}

func (s *Server) caller() activity.Caller {
	s.callerMu.Lock()
	defer s.callerMu.Unlock()
	src := "mcp-http"
	if s.notify != nil {
		src = "mcp-stdio"
	}
	return activity.Caller{Source: src, Client: s.clientName, Session: s.sessionID}
}

func (s *Server) initialize(ctx context.Context, params json.RawMessage) (any, *rpcError) {
	var req struct {
		ProtocolVersion string `json:"protocolVersion"`
		ClientInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"clientInfo"`
	}
	_ = json.Unmarshal(params, &req)

	client := strings.TrimSpace(req.ClientInfo.Name)
	if v := strings.TrimSpace(req.ClientInfo.Version); v != "" && client != "" {
		client = client + "/" + v
	}
	session := activity.NewSession()
	s.callerMu.Lock()
	s.clientName = client
	s.sessionID = session
	s.callerMu.Unlock()
	if a, ok := s.Backend.(interface {
		SetAttribution(client, session, via string)
	}); ok {
		a.SetAttribution(client, session, "stdio")
	}

	version := ProtocolVersion
	if supportedVersions[req.ProtocolVersion] {
		version = req.ProtocolVersion
	}

	// listChanged is a promise to send notifications/tools/list_changed, and
	// both transports can now keep it: stdio writes down its own pipe, and
	// HTTP writes down the GET event stream. The promise is still conditional
	// rather than hardcoded, because a Server built without either -- a test,
	// an in-process caller -- would otherwise leave a client waiting for a
	// message that can never arrive.
	tools := map[string]any{}
	if s.notify != nil || s.hub != nil {
		tools["listChanged"] = true
	}

	return map[string]any{
		"protocolVersion": version,
		"capabilities": map[string]any{
			"tools": tools,
			// resources: the list is fixed at two, so no listChanged here.
			"resources": map[string]any{},
			// prompts: generated from what the engine holds, so the list can
			// change -- but a client re-lists on demand and drover has no
			// reason to push, so no listChanged here either.
			"prompts": map[string]any{},
		},
		"serverInfo": map[string]any{
			"name":    "drover",
			"version": s.Version,
		},
		"instructions": s.instructions(ctx),
	}, nil
}

// instructions is the static how-to-use-drover text with an inventory of what
// this particular engine holds appended.
//
// The two halves are different in kind. The first never changes and explains
// which tool answers which question; the second is knowable only by a running
// engine, and is the half that saves an agent its first round trip.
func (s *Server) instructions(ctx context.Context) string {
	inv := s.inventory(ctx)
	if inv == "" {
		return instructions
	}
	return instructions + "\n\n" + inv +
		"\nThis list was taken when you connected. Read the drover://inventory resource for what the engine holds now."
}

const instructions = `drover holds git checkouts, HTTP requests and database connections that have been applied to it.

These are usually NOT the code you are editing. They are the rest of the system around it: services you call, services that call you, shared libraries, vendored references. Reach for drover whenever a question runs past the edge of your own workspace -- and check here before answering from memory about another service, because these are the real files at a real commit, not a recollection of them.

What that is worth doing:

Writing a plan, a PRD or a design doc: read how the thing is done today before proposing how it should be done. grep every repository at once for the pattern you are about to introduce, read the service you would call, api_describe the endpoint you would depend on, sql_query the table you would write to. A plan that names real files, real fields and real endpoints can be acted on; one written from assumption has to be corrected first.

Debugging something that crosses a boundary: the bug is often not in the code you can see. Read the caller to find out what it actually sends, api_call the endpoint to see what it actually returns, sql_query the row to see what is actually stored, and git log or git blame the line to find out when the behaviour changed and what shipped alongside it. Each of those is a fact, so a chain of them ends in a cause rather than a theory.

Understanding an unfamiliar service: ls it, read its entry point, lsp workspace_symbols for the type the whole thing is built around, git contributors to learn who to ask.

Checking a claim: "we already have a helper for this", "that endpoint returns a list", "nobody calls this any more" -- grep, api_call and lsp references settle each of those in one call.

You write your own files with your own tools. Nothing here writes anything: every drover tool reads.

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
func (s *Server) listTools(ctx context.Context, _ json.RawMessage) (any, *rpcError) {
	tools := append([]Tool(nil), fileTools...)
	tools = append(tools, s.gitTools(ctx)...)
	tools = append(tools, s.lspTools(ctx)...)
	tools = append(tools, s.apiTools(ctx)...)
	tools = append(tools, s.sqlTools(ctx)...)
	tools = append(tools, s.docTools(ctx)...)
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
		Description: "Search file contents across the repositories with a regular expression. Use this to find where something is defined or used, then read that file. Dependency and build directories (node_modules, vendor, dist, target, .venv and the like) are skipped, so results are the project's own code; to search one anyway, name it in `path`.",
		InputSchema: Schema{
			Type:       "object",
			Additional: boolPtr(false),
			Required:   []string{"pattern"},
			Properties: map[string]Prop{
				"pattern":       {Type: "string", Description: "Go/RE2 regular expression, e.g. `func NewServer` or `TODO|FIXME`."},
				"path":          {Type: "string", Description: "Limit the search to this subtree, e.g. `api/internal`. Naming a skipped directory here searches it, e.g. `web/node_modules`."},
				"include":       {Type: "string", Description: "Only search files whose name matches this glob, e.g. `*.go`."},
				"caseSensitive": {Type: "boolean", Description: "Match case exactly. Defaults to false.", Default: false},
				"maxResults":    {Type: "integer", Description: "Cap the number of matches returned."},
				"selector":      {Type: "string", Description: "Search only the repositories whose labels match, e.g. `team=billing` or `tier!=frontend`. Comma-separated clauses are ANDed; a bare key means the label exists, `!key` that it does not. Use this instead of `path` when you know the domain but not the directory. The connection inventory lists each repository's labels."},
			},
		},
	},
	{
		Name:        "find",
		Description: "Find files by name or path pattern. A pattern without a slash matches the file name (`*.go`); one with a slash matches the whole path (`api/internal/*.go`). Dependency and build directories are skipped unless `path` names one.",
		InputSchema: Schema{
			Type:       "object",
			Additional: boolPtr(false),
			Required:   []string{"pattern"},
			Properties: map[string]Prop{
				"pattern":    {Type: "string", Description: "Glob, e.g. `*_test.go` or `api/cmd/*`."},
				"path":       {Type: "string", Description: "Limit the search to this subtree. Naming a skipped directory here searches it."},
				"maxResults": {Type: "integer", Description: "Cap the number of paths returned."},
				"selector":   {Type: "string", Description: "Search only the repositories whose labels match, e.g. `team=billing`. Same grammar as grep's selector."},
			},
		},
	},
}

// apiTools are the three that cover every configured HTTP request.
//
// api_call's description stays the same length whether one request is
// configured or two hundred; discovery happens through api_list.
func (s *Server) apiTools(ctx context.Context) []Tool {
	count, envs := s.apiCounts(ctx)
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
func (s *Server) apiCounts(ctx context.Context) (requests, environments int) {
	items, err := s.Backend.List(ctx, object.KindHTTPRequest)
	if err != nil {
		return 0, 0
	}
	for _, v := range items {
		if v.Safe {
			requests++
		}
	}
	if envs, err := s.Backend.List(ctx, object.KindEnvironment); err == nil {
		environments = len(envs)
	}
	return requests, environments
}

// sqlTools is loop's shape: one query tool, with the catalogue of connections
// in its description so the model can pick the right one.
func (s *Server) sqlTools(ctx context.Context) []Tool {
	items, err := s.Backend.List(ctx, object.KindSQLConnection)
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
			"Read `docs/schema/<connection>.sql` first -- drover dumps every table, column, foreign key and row-count estimate there, so discovering the shape costs a `read` and not a round trip. Fall back to information_schema only if that file is not there. Add a LIMIT to exploratory queries.\n\n" +
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
