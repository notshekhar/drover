package mcp

import (
	"encoding/json"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/notshekhar/drover/internal/api"
	"github.com/notshekhar/drover/internal/object"
)

// callParams is the tools/call payload.
type callParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

func (s *Server) callTool(params json.RawMessage) (any, *rpcError) {
	var req callParams
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, errf(CodeInvalidParams, "invalid params: %v", err)
	}
	if req.Name == "" {
		return nil, errf(CodeInvalidParams, "no tool name")
	}

	switch req.Name {
	case "ls":
		return s.toolLs(req.Arguments), nil
	case "read":
		return s.toolRead(req.Arguments), nil
	case "grep":
		return s.toolGrep(req.Arguments), nil
	case "find":
		return s.toolFind(req.Arguments), nil
	}

	switch req.Name {
	case "api_list":
		return s.toolAPIList(req.Arguments), nil
	case "api_describe":
		return s.toolAPIDescribe(req.Arguments), nil
	case "api_call":
		return s.toolAPICall(req.Arguments), nil
	case "sql_query":
		return s.toolSQLQuery(req.Arguments), nil
	}

	// Unknown tool is a protocol-level error: the call could not be made at
	// all, as opposed to a tool that ran and failed.
	return nil, errf(CodeMethodNotFound, "unknown tool %q", req.Name)
}

func decodeArgs(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, v)
}

// --- file tools ---

func (s *Server) toolLs(raw json.RawMessage) *CallResult {
	var args api.LsRequest
	if err := decodeArgs(raw, &args); err != nil {
		return toolError("invalid arguments: %v", err)
	}
	res, err := s.Backend.Ls(args)
	if err != nil {
		return toolError("%v", err)
	}
	if len(res.Entries) == 0 {
		if res.Path == "" {
			return text("drover holds no repositories yet. Someone needs to apply a Repository object first.")
		}
		return text("%s is empty.", res.Path)
	}

	var b strings.Builder
	where := res.Path
	if where == "" {
		where = "repositories"
	}
	fmt.Fprintf(&b, "%s:\n", where)
	for _, e := range res.Entries {
		switch e.Type {
		case "dir":
			fmt.Fprintf(&b, "  %s/\n", e.Name)
		case "symlink":
			fmt.Fprintf(&b, "  %s -> (symlink)\n", e.Name)
		default:
			fmt.Fprintf(&b, "  %s (%s)\n", e.Name, humanSize(e.Size))
		}
	}
	if res.Truncated {
		b.WriteString("\n(listing truncated)\n")
	}
	return text("%s", b.String())
}

func (s *Server) toolRead(raw json.RawMessage) *CallResult {
	var args api.ReadRequest
	if err := decodeArgs(raw, &args); err != nil {
		return toolError("invalid arguments: %v", err)
	}
	if strings.TrimSpace(args.Path) == "" {
		return toolError("read needs a path, relative to the repository root, like api/internal/db.go")
	}
	res, err := s.Backend.ReadFile(args)
	if err != nil {
		return toolError("%v", err)
	}

	// Line numbers, because the next thing a model does with a file is talk
	// about a specific line in it.
	var b strings.Builder
	fmt.Fprintf(&b, "%s (lines %d-%d of %d)\n\n", res.Path, res.StartLine, res.EndLine, res.TotalLines)
	line := res.StartLine
	for _, l := range strings.Split(strings.TrimSuffix(res.Content, "\n"), "\n") {
		fmt.Fprintf(&b, "%6d\t%s\n", line, l)
		line++
	}
	if res.Truncated {
		fmt.Fprintf(&b, "\n(%d more lines; call read again with offset %d)\n", res.TotalLines-res.EndLine, res.EndLine+1)
	}
	return text("%s", b.String())
}

func (s *Server) toolGrep(raw json.RawMessage) *CallResult {
	var args api.GrepRequest
	if err := decodeArgs(raw, &args); err != nil {
		return toolError("invalid arguments: %v", err)
	}
	if strings.TrimSpace(args.Pattern) == "" {
		return toolError("grep needs a pattern")
	}
	res, err := s.Backend.Grep(args)
	if err != nil {
		return toolError("%v", err)
	}
	if len(res.Matches) == 0 {
		return text("No matches for %q in %d file(s).", args.Pattern, res.Files)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d match(es) for %q:\n\n", len(res.Matches), args.Pattern)
	for _, m := range res.Matches {
		fmt.Fprintf(&b, "%s:%d: %s\n", m.Path, m.Line, strings.TrimSpace(m.Text))
	}
	if res.Truncated {
		b.WriteString("\n(more matches exist; narrow with path or include)\n")
	}
	return text("%s", b.String())
}

func (s *Server) toolFind(raw json.RawMessage) *CallResult {
	var args api.FindRequest
	if err := decodeArgs(raw, &args); err != nil {
		return toolError("invalid arguments: %v", err)
	}
	if strings.TrimSpace(args.Pattern) == "" {
		return toolError("find needs a pattern")
	}
	res, err := s.Backend.Find(args)
	if err != nil {
		return toolError("%v", err)
	}
	if len(res.Paths) == 0 {
		return text("No files match %q.", args.Pattern)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d file(s) matching %q:\n\n", len(res.Paths), args.Pattern)
	for _, p := range res.Paths {
		fmt.Fprintf(&b, "%s\n", p)
	}
	if res.Truncated {
		b.WriteString("\n(more files exist; narrow the pattern)\n")
	}
	return text("%s", b.String())
}

// --- object tools ---

// --- api tools ---

// toolAPIList is the catalogue, with the fuzzy search that makes a large
// collection usable.
func (s *Server) toolAPIList(raw json.RawMessage) *CallResult {
	var args struct {
		Search string `json:"search"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return toolError("invalid arguments: %v", err)
	}

	items, err := s.Backend.List(object.KindHTTPRequest)
	if err != nil {
		return toolError("%v", err)
	}
	// Only what can actually be called from here.
	var callable []api.ObjectView
	for _, v := range items {
		if v.Safe {
			callable = append(callable, v)
		}
	}
	if len(callable) == 0 {
		return text("No HTTP requests are configured. Add one by writing an HTTPRequest document -- see docs.md in drover's data directory.")
	}

	matched := rank(callable, args.Search, s.requestSearchable)
	if len(matched) == 0 {
		return text("No request matches %q. There are %d configured; call api_list with no search to see them all.",
			args.Search, len(callable))
	}

	var b strings.Builder
	if args.Search != "" {
		fmt.Fprintf(&b, "%d of %d request(s) match %q:\n\n", len(matched), len(callable), args.Search)
	} else {
		fmt.Fprintf(&b, "%d request(s):\n\n", len(matched))
	}
	for _, v := range matched {
		b.WriteString(summarizeRequest(v))
		b.WriteString("\n")
	}

	// The environments, because a caller choosing between stage and prod needs
	// to know which exist and what each one points at.
	if envs, err := s.Backend.List(object.KindEnvironment); err == nil && len(envs) > 0 {
		fmt.Fprintf(&b, "%d environment(s):\n", len(envs))
		for _, e := range envs {
			b.WriteString(summarizeEnvironment(e))
		}
		b.WriteString("\n")
	}

	b.WriteString("Use api_describe for one request's full parameters, then api_call to perform it.")
	return text("%s", b.String())
}

// requestSearchable folds every property into the haystack, so a search hits
// on anything the document says rather than only on the name.
func (s *Server) requestSearchable(v api.ObjectView) searchable {
	fields := []string{
		strings.ToLower(v.Method),
		strings.ToLower(v.URL),
		strings.ToLower(strings.Join(v.Environments, " ")),
		strings.ToLower(strings.Join(v.Params, " ")),
	}
	if d := describeFromYAML(v.YAML, "description"); d != "" {
		fields = append(fields, strings.ToLower(d))
	}
	// Parameter descriptions too: "the org that owns it" should find a request
	// whose owner parameter says exactly that.
	if spec, err := specFromYAML(v.YAML); err == nil {
		for _, p := range spec.PathParams {
			fields = append(fields, strings.ToLower(p.Name+" "+p.Description))
		}
		for _, q := range spec.Query {
			fields = append(fields, strings.ToLower(q.Name+" "+q.Description))
		}
	}
	return searchable{Name: v.Name, Fields: fields}
}

func summarizeRequest(v api.ObjectView) string {
	var b strings.Builder
	fmt.Fprintf(&b, "  %s\n", v.Name)
	if d := describeFromYAML(v.YAML, "description"); d != "" {
		fmt.Fprintf(&b, "      %s\n", firstLine(d))
	}
	fmt.Fprintf(&b, "      %s %s\n", v.Method, v.URL)
	if len(v.Params) > 0 {
		fmt.Fprintf(&b, "      params: %s\n", strings.Join(v.Params, ", "))
	}
	if len(v.Environments) > 0 {
		envs := make([]string, 0, len(v.Environments))
		for _, e := range v.Environments {
			if e == v.DefaultEnvironment {
				e += " (default)"
			}
			envs = append(envs, e)
		}
		fmt.Fprintf(&b, "      environments: %s\n", strings.Join(envs, ", "))
	}
	return b.String()
}

func summarizeEnvironment(e api.ObjectView) string {
	var b strings.Builder
	fmt.Fprintf(&b, "  %s", e.Name)
	if d := describeFromYAML(e.YAML, "description"); d != "" {
		fmt.Fprintf(&b, " — %s", firstLine(d))
	}
	b.WriteString("\n")
	if len(e.Variables) > 0 {
		fmt.Fprintf(&b, "      variables: %s\n", strings.Join(e.Variables, ", "))
	}
	// Secret names and whether they are set -- never their values.
	if len(e.Secrets) > 0 {
		var parts []string
		for _, sec := range e.Secrets {
			state := "set"
			if !sec.Set {
				state = "NOT SET"
			}
			parts = append(parts, fmt.Sprintf("%s (%s)", sec.Name, state))
		}
		fmt.Fprintf(&b, "      secrets: %s\n", strings.Join(parts, ", "))
	}
	return b.String()
}

// toolAPIDescribe is the detail view: everything needed to fill a call in.
func (s *Server) toolAPIDescribe(raw json.RawMessage) *CallResult {
	var args struct {
		Request string `json:"request"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return toolError("invalid arguments: %v", err)
	}
	if strings.TrimSpace(args.Request) == "" {
		return toolError("api_describe needs a request name; call api_list to see them")
	}

	v, err := s.Backend.Get(object.KindHTTPRequest, args.Request)
	if err != nil {
		return toolError("%v (call api_list to see the configured requests)", err)
	}
	if !v.Safe {
		return toolError("%s is a %s request and is not available here; only GET requests are offered", v.Name, v.Method)
	}
	spec, err := specFromYAML(v.YAML)
	if err != nil {
		return toolError("could not read %s: %v", args.Request, err)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", v.Name)
	if spec.Description != "" {
		fmt.Fprintf(&b, "\n%s\n", spec.Description)
	}
	fmt.Fprintf(&b, "\n%s %s\n", spec.NormalizedMethod(), spec.URL)

	if len(spec.PathParams) > 0 {
		b.WriteString("\nPath parameters (all required):\n")
		for _, p := range spec.PathParams {
			b.WriteString(describeParam(p, true))
		}
	}
	if len(spec.Query) > 0 {
		b.WriteString("\nQuery parameters:\n")
		for _, q := range spec.Query {
			b.WriteString(describeParam(q, q.Required))
		}
	}
	if len(spec.Headers) > 0 {
		// Names only. A header value can carry a secret reference, and its
		// resolved value is never the caller's business.
		var names []string
		for _, h := range spec.Headers {
			names = append(names, h.Name)
		}
		fmt.Fprintf(&b, "\nHeaders sent: %s\n", strings.Join(names, ", "))
	}
	if len(spec.Environments) > 0 {
		fmt.Fprintf(&b, "\nEnvironments: %s", strings.Join(spec.Environments, ", "))
		if spec.DefaultEnvironment != "" {
			fmt.Fprintf(&b, " (default %s)", spec.DefaultEnvironment)
		}
		b.WriteString("\n")
	}

	fmt.Fprintf(&b, "\nCall it with:\n  api_call { \"request\": %q", v.Name)
	if required := spec.RequiredParams(); len(required) > 0 {
		var pairs []string
		for _, name := range required {
			pairs = append(pairs, fmt.Sprintf("%q: \"...\"", name))
		}
		fmt.Fprintf(&b, ", \"params\": {%s}", strings.Join(pairs, ", "))
	}
	b.WriteString(" }\n")
	return text("%s", b.String())
}

func describeParam(p object.Param, required bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "  %s", p.Name)
	if required {
		b.WriteString(" (required)")
	}
	b.WriteString("\n")
	if p.Description != "" {
		fmt.Fprintf(&b, "      %s\n", p.Description)
	}
	if p.Example != "" {
		fmt.Fprintf(&b, "      example: %s\n", p.Example)
	}
	if p.Default != "" {
		fmt.Fprintf(&b, "      default: %s\n", p.Default)
	}
	return b.String()
}

// toolAPICall performs one request.
func (s *Server) toolAPICall(raw json.RawMessage) *CallResult {
	var args struct {
		Request     string         `json:"request"`
		Params      map[string]any `json:"params"`
		Environment string         `json:"environment"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return toolError("invalid arguments: %v", err)
	}
	if strings.TrimSpace(args.Request) == "" {
		return toolError("api_call needs a request name; call api_list to see them")
	}

	// Arguments arrive as JSON values, but every parameter is a string on the
	// wire, so numbers and booleans are coerced rather than refused -- a model
	// passing 42 for an id means "42".
	params := map[string]string{}
	for k, v := range args.Params {
		params[k] = stringify(v)
	}

	resp, err := s.Backend.Call(args.Request, api.CallRequest{
		Environment: args.Environment,
		Params:      params,
	})
	if err != nil {
		return toolError("%v", err)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s %s -> %d (%dms)\n", resp.Method, resp.URL, resp.Status, resp.DurationMS)
	if ct := resp.Headers["Content-Type"]; ct != "" {
		fmt.Fprintf(&b, "Content-Type: %s\n", ct)
	}
	b.WriteString("\n")
	b.WriteString(resp.Body)
	if resp.Truncated {
		b.WriteString("\n\n(response truncated)")
	}

	// A 4xx/5xx is a result the model should read and react to, not a
	// transport failure, so the body comes back with IsError set.
	if resp.Status >= 400 {
		return &CallResult{Content: []Content{{Type: "text", Text: b.String()}}, IsError: true}
	}
	return text("%s", b.String())
}

// toolSQLQuery runs one statement against a named connection.
func (s *Server) toolSQLQuery(raw json.RawMessage) *CallResult {
	var args struct {
		Connection string `json:"connection"`
		Query      string `json:"query"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return toolError("invalid arguments: %v", err)
	}
	if strings.TrimSpace(args.Connection) == "" {
		return toolError("sql_query needs a connection; the available ones are listed in this tool's description")
	}
	if strings.TrimSpace(args.Query) == "" {
		return toolError("sql_query needs a SQL statement")
	}

	res, err := s.Backend.Query(args.Connection, args.Query)
	if err != nil {
		return toolError("%v", err)
	}
	if res.RowCount == 0 {
		return text("No rows. (%s, %dms)", res.Provider, res.ElapsedMS)
	}

	var b strings.Builder
	b.WriteString(strings.Join(res.Columns, "\t"))
	b.WriteString("\n")
	for _, row := range res.Rows {
		b.WriteString(strings.Join(row, "\t"))
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "\n%d row(s) in %dms on %s", res.RowCount, res.ElapsedMS, res.Provider)
	if res.Truncated {
		b.WriteString(" (truncated -- add LIMIT or narrow the query)")
	}
	return text("%s", b.String())
}

func stringify(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	case float64:
		// JSON has one number type; render integers without a trailing .0,
		// since an id of 42 must not become "42.0" in a URL.
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%g", t)
	case bool:
		return fmt.Sprintf("%t", t)
	default:
		data, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprintf("%v", t)
		}
		return string(data)
	}
}

func humanSize(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%dB", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1fK", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1fM", float64(n)/(1024*1024))
	}
}

// --- reading the stored document ---

// specFromYAML pulls an HTTPRequest spec out of a stored object, so the tool
// schema can carry the parameter descriptions the document declares.
func specFromYAML(doc string) (*object.HTTPRequestSpec, error) {
	if doc == "" {
		return nil, fmt.Errorf("no document")
	}
	objs, err := object.Parse("stored", []byte(doc))
	if err != nil || len(objs) == 0 {
		return nil, fmt.Errorf("unreadable document")
	}
	return objs[0].HTTPRequest()
}

// describeFromYAML pulls a single top-level spec field out of a stored
// document without needing a typed decode for every kind.
func describeFromYAML(doc, field string) string {
	if doc == "" {
		return ""
	}
	var wrapper struct {
		Spec map[string]any `yaml:"spec"`
	}
	if err := yaml.Unmarshal([]byte(doc), &wrapper); err != nil {
		return ""
	}
	if v, ok := wrapper.Spec[field].(string); ok {
		return v
	}
	return ""
}
