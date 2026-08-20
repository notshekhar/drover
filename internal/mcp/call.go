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

	switch {
	case strings.HasPrefix(req.Name, "call_"):
		return s.toolCall(objectName(strings.TrimPrefix(req.Name, "call_")), req.Arguments), nil
	case strings.HasPrefix(req.Name, "query_"):
		return s.toolQuery(objectName(strings.TrimPrefix(req.Name, "query_")), req.Arguments), nil
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

func (s *Server) toolCall(name string, raw json.RawMessage) *CallResult {
	args := map[string]string{}
	if len(raw) > 0 {
		// Arguments arrive as JSON values, but every parameter is a string on
		// the wire, so numbers and booleans are coerced rather than refused --
		// a model passing 42 for an id means "42".
		var loose map[string]any
		if err := json.Unmarshal(raw, &loose); err != nil {
			return toolError("invalid arguments: %v", err)
		}
		for k, v := range loose {
			args[k] = stringify(v)
		}
	}

	env := args["environment"]
	delete(args, "environment")

	resp, err := s.Backend.Call(name, api.CallRequest{Environment: env, Params: args})
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

func (s *Server) toolQuery(name string, raw json.RawMessage) *CallResult {
	var args struct {
		Query string `json:"query"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return toolError("invalid arguments: %v", err)
	}
	if strings.TrimSpace(args.Query) == "" {
		return toolError("query needs a SQL statement")
	}

	res, err := s.Backend.Query(name, args.Query)
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
