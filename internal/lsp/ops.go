package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// Operations is every operation the tool understands. Exported because the
// MCP schema, the docs and the dispatch table all have to agree, and three
// hand-maintained copies would not stay in agreement.
var Operations = []string{
	"definition", "references", "hover", "implementations",
	"document_symbols", "workspace_symbols",
	"incoming_calls", "outgoing_calls",
	"diagnostics", "servers",
}

// Result limits. A file with four hundred references is a real thing, and all
// four hundred is not an answer anybody reads.
const (
	DefaultLimit       = 100
	MaxLimit           = 500
	diagnosticsWait    = 2 * time.Second
	maxOutlineDepth    = 6
	maxSymbolNameLines = 12
)

// Request is one question.
type Request struct {
	Operation  string
	Path       string
	Line       int // 1-based, as read and grep print them
	Character  int // 1-based
	Symbol     string
	Occurrence int
	Query      string
	Limit      int
}

// Ref is a place in the code.
type Ref struct {
	Path      string
	Line      int
	Character int
	Text      string
	Name      string
	Kind      string
	Container string
}

// Symbol is one entry of an outline.
type Symbol struct {
	Name      string
	Kind      string
	Detail    string
	Path      string
	Line      int
	Character int
	Depth     int
}

// Problem is one diagnostic.
type Problem struct {
	Path      string
	Line      int
	Character int
	Severity  string
	Source    string
	Code      string
	Message   string
}

// Call is one edge of a call graph.
type Call struct {
	Name  string
	Kind  string
	Path  string
	Line  int
	Sites []int // the lines the call is made from
}

// Result is a union: which fields are set depends on the operation.
type Result struct {
	Operation string
	Path      string
	Language  string
	Position  string

	Refs        []Ref
	Hover       string
	Symbols     []Symbol
	Problems    []Problem
	Calls       []Call
	Servers     []ServerStatus
	Truncated   bool
	Note        string
	ServerState string
}

// Run answers one question.
func (m *Manager) Run(ctx context.Context, req Request) (*Result, error) {
	op := strings.TrimSpace(strings.ToLower(req.Operation))
	if op == "" {
		return nil, fmt.Errorf("operation is required; one of %s", strings.Join(Operations, ", "))
	}
	if !knownOp(op) {
		return nil, fmt.Errorf("unknown operation %q; one of %s", req.Operation, strings.Join(Operations, ", "))
	}
	req.Operation = op

	if op == "servers" {
		return &Result{Operation: op, Servers: m.Status(ctx)}, nil
	}

	target, err := m.Resolve(req.Path)
	if err != nil {
		return nil, err
	}
	client, def, err := m.ClientFor(ctx, target)
	if err != nil {
		return nil, err
	}

	res := &Result{Operation: op, Path: target.Rel, Language: def.Language}
	if what := client.Indexing(); what != "" {
		res.ServerState = what
	}

	switch op {
	case "definition", "implementations":
		err = m.locations(ctx, client, target, &req, res)
	case "references":
		err = m.references(ctx, client, target, &req, res)
	case "hover":
		err = m.hover(ctx, client, target, &req, res)
	case "document_symbols":
		err = m.documentSymbols(ctx, client, target, res)
	case "workspace_symbols":
		err = m.workspaceSymbols(ctx, client, &req, res)
	case "incoming_calls", "outgoing_calls":
		err = m.calls(ctx, client, target, &req, res)
	case "diagnostics":
		err = m.diagnostics(ctx, client, target, res)
	}
	if err != nil {
		return nil, err
	}
	return res, nil
}

func knownOp(op string) bool {
	for _, o := range Operations {
		if o == op {
			return true
		}
	}
	return false
}

// --- positions ---

// position works out where in the file to ask about.
//
// An agent arrives here from grep, holding a line number and the text of an
// identifier -- never a column. Demanding a column is the single largest
// source of failed language-server calls, so `symbol` is accepted instead and
// the column is worked out here.
func (m *Manager) position(target *Target, req *Request) (Position, string, error) {
	lines, err := readLines(target.Abs)
	if err != nil {
		return Position{}, "", err
	}

	if req.Symbol == "" {
		if req.Line < 1 {
			return Position{}, "", fmt.Errorf("this operation needs a position: give line and character, or the symbol you are asking about")
		}
		char := req.Character
		if char < 1 {
			char = 1
		}
		return Position{Line: req.Line - 1, Character: char - 1}, "", nil
	}

	hits := findSymbol(lines, req.Symbol, req.Line)
	if len(hits) == 0 {
		where := target.Rel
		if req.Line > 0 {
			where = fmt.Sprintf("%s line %d", target.Rel, req.Line)
		}
		return Position{}, "", fmt.Errorf("%q does not appear in %s", req.Symbol, where)
	}

	pick := 0
	if req.Occurrence > 1 {
		if req.Occurrence > len(hits) {
			return Position{}, "", fmt.Errorf("%q appears %d time(s) in %s, so occurrence %d does not exist", req.Symbol, len(hits), target.Rel, req.Occurrence)
		}
		pick = req.Occurrence - 1
	}
	chosen := hits[pick]

	note := ""
	if len(hits) > 1 && req.Occurrence == 0 {
		note = fmt.Sprintf("%q appears on %s; this is the one on line %d -- pass line, or occurrence, for another",
			req.Symbol, lineList(hits), chosen.Line+1)
	}
	return chosen, note, nil
}

// findSymbol locates whole-word occurrences of a name.
//
// Whole-word, because `Connect` should not match `Connected`; and comment
// lines are skipped, because a server has nothing to say about a word in a
// comment and answering "no definition found" from one is misleading.
func findSymbol(lines []string, symbol string, onLine int) []Position {
	var out []Position
	for i, line := range lines {
		if onLine > 0 && i != onLine-1 {
			continue
		}
		if onLine == 0 && isComment(line) {
			continue
		}
		for from := 0; ; {
			at := strings.Index(line[from:], symbol)
			if at < 0 {
				break
			}
			at += from
			from = at + len(symbol)
			if !wholeWord(line, at, len(symbol)) {
				continue
			}
			out = append(out, Position{Line: i, Character: at})
		}
	}
	return out
}

func wholeWord(line string, at, length int) bool {
	if at > 0 && isIdentByte(line[at-1]) {
		return false
	}
	end := at + length
	return end >= len(line) || !isIdentByte(line[end])
}

func isIdentByte(c byte) bool {
	return c == '_' || c == '$' ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

func isComment(line string) bool {
	t := strings.TrimSpace(line)
	return strings.HasPrefix(t, "//") || strings.HasPrefix(t, "#") ||
		strings.HasPrefix(t, "*") || strings.HasPrefix(t, "/*") || strings.HasPrefix(t, "--")
}

func lineList(hits []Position) string {
	seen := map[int]bool{}
	var parts []string
	for _, h := range hits {
		if seen[h.Line] || len(parts) >= maxSymbolNameLines {
			continue
		}
		seen[h.Line] = true
		parts = append(parts, fmt.Sprint(h.Line+1))
	}
	return "line " + strings.Join(parts, ", ")
}

// --- the operations ---

func (m *Manager) locations(ctx context.Context, c *Client, target *Target, req *Request, res *Result) error {
	method := "textDocument/definition"
	capability := "definitionProvider"
	if req.Operation == "implementations" {
		method, capability = "textDocument/implementation", "implementationProvider"
	}
	if !c.Supports(capability) {
		return fmt.Errorf("the %s server does not answer %s", res.Language, req.Operation)
	}

	pos, note, err := m.position(target, req)
	if err != nil {
		return err
	}
	res.Note, res.Position = note, describe(target.Rel, pos)

	var raw json.RawMessage
	if err := c.Request(ctx, method, map[string]any{
		"textDocument": map[string]any{"uri": PathToURI(target.Abs)},
		"position":     pos,
	}, &raw); err != nil {
		return err
	}
	res.Refs = m.refs(ToLocations(raw), req.Limit, res)
	return nil
}

func (m *Manager) references(ctx context.Context, c *Client, target *Target, req *Request, res *Result) error {
	if !c.Supports("referencesProvider") {
		return fmt.Errorf("the %s server does not answer references", res.Language)
	}
	pos, note, err := m.position(target, req)
	if err != nil {
		return err
	}
	res.Note, res.Position = note, describe(target.Rel, pos)

	var locations []Location
	if err := c.Request(ctx, "textDocument/references", map[string]any{
		"textDocument": map[string]any{"uri": PathToURI(target.Abs)},
		"position":     pos,
		"context":      map[string]any{"includeDeclaration": false},
	}, &locations); err != nil {
		return err
	}
	res.Refs = m.refs(locations, req.Limit, res)
	return nil
}

func (m *Manager) hover(ctx context.Context, c *Client, target *Target, req *Request, res *Result) error {
	if !c.Supports("hoverProvider") {
		return fmt.Errorf("the %s server does not answer hover", res.Language)
	}
	pos, note, err := m.position(target, req)
	if err != nil {
		return err
	}
	res.Note, res.Position = note, describe(target.Rel, pos)

	var reply struct {
		Contents json.RawMessage `json:"contents"`
	}
	if err := c.Request(ctx, "textDocument/hover", map[string]any{
		"textDocument": map[string]any{"uri": PathToURI(target.Abs)},
		"position":     pos,
	}, &reply); err != nil {
		return err
	}
	res.Hover = hoverText(reply.Contents)
	return nil
}

// hoverText flattens every shape hover contents can take: a MarkupContent
// object, a bare string, a {language, value} pair, or an array of any of them.
func hoverText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		return strings.TrimSpace(str)
	}
	var object struct {
		Kind     string `json:"kind"`
		Value    string `json:"value"`
		Language string `json:"language"`
	}
	if err := json.Unmarshal(raw, &object); err == nil && object.Value != "" {
		return strings.TrimSpace(object.Value)
	}
	var many []json.RawMessage
	if err := json.Unmarshal(raw, &many); err == nil {
		var parts []string
		for _, item := range many {
			if text := hoverText(item); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n\n")
	}
	return ""
}

func (m *Manager) documentSymbols(ctx context.Context, c *Client, target *Target, res *Result) error {
	if !c.Supports("documentSymbolProvider") {
		return fmt.Errorf("the %s server does not answer document_symbols", res.Language)
	}
	var raw json.RawMessage
	if err := c.Request(ctx, "textDocument/documentSymbol", map[string]any{
		"textDocument": map[string]any{"uri": PathToURI(target.Abs)},
	}, &raw); err != nil {
		return err
	}

	// Two shapes again: a nested DocumentSymbol tree from a modern server, a
	// flat SymbolInformation list from an older one.
	var nested []DocumentSymbol
	if err := json.Unmarshal(raw, &nested); err == nil && len(nested) > 0 && nested[0].Name != "" {
		res.Symbols = flatten(nested, target.Rel, 0)
		return nil
	}
	var flat []SymbolInformation
	if err := json.Unmarshal(raw, &flat); err == nil {
		for _, s := range flat {
			res.Symbols = append(res.Symbols, Symbol{
				Name: s.Name, Kind: KindName(s.Kind), Detail: s.ContainerName,
				Path: m.display(s.Location.URI),
				Line: s.Location.Range.Start.Line + 1, Character: s.Location.Range.Start.Character + 1,
			})
		}
	}
	return nil
}

func flatten(symbols []DocumentSymbol, path string, depth int) []Symbol {
	if depth > maxOutlineDepth {
		return nil
	}
	var out []Symbol
	for _, s := range symbols {
		out = append(out, Symbol{
			Name: s.Name, Kind: KindName(s.Kind), Detail: s.Detail, Path: path,
			Line: s.SelectionRange.Start.Line + 1, Character: s.SelectionRange.Start.Character + 1,
			Depth: depth,
		})
		out = append(out, flatten(s.Children, path, depth+1)...)
	}
	return out
}

func (m *Manager) workspaceSymbols(ctx context.Context, c *Client, req *Request, res *Result) error {
	if !c.Supports("workspaceSymbolProvider") {
		return fmt.Errorf("the %s server does not answer workspace_symbols", res.Language)
	}
	query := strings.TrimSpace(req.Query)
	if query == "" {
		query = req.Symbol
	}
	if query == "" {
		return fmt.Errorf("workspace_symbols needs a query -- the name you are looking for")
	}

	var symbols []SymbolInformation
	if err := c.Request(ctx, "workspace/symbol", map[string]any{"query": query}, &symbols); err != nil {
		return err
	}
	limit := clampLimit(req.Limit)
	for i, s := range symbols {
		if i >= limit {
			res.Truncated = true
			break
		}
		res.Symbols = append(res.Symbols, Symbol{
			Name: s.Name, Kind: KindName(s.Kind), Detail: s.ContainerName,
			Path: m.display(s.Location.URI),
			Line: s.Location.Range.Start.Line + 1, Character: s.Location.Range.Start.Character + 1,
		})
	}
	return nil
}

func (m *Manager) calls(ctx context.Context, c *Client, target *Target, req *Request, res *Result) error {
	if !c.Supports("callHierarchyProvider") {
		return fmt.Errorf("the %s server does not answer call hierarchies", res.Language)
	}
	pos, note, err := m.position(target, req)
	if err != nil {
		return err
	}
	res.Note, res.Position = note, describe(target.Rel, pos)

	var items []CallHierarchyItem
	if err := c.Request(ctx, "textDocument/prepareCallHierarchy", map[string]any{
		"textDocument": map[string]any{"uri": PathToURI(target.Abs)},
		"position":     pos,
	}, &items); err != nil {
		return err
	}
	if len(items) == 0 {
		res.Note = joinNotes(res.Note, "there is no callable thing at that position")
		return nil
	}

	if req.Operation == "incoming_calls" {
		var calls []CallHierarchyIncomingCall
		if err := c.Request(ctx, "callHierarchy/incomingCalls", map[string]any{"item": items[0]}, &calls); err != nil {
			return err
		}
		for _, call := range calls {
			res.Calls = append(res.Calls, m.call(call.From, call.FromRanges))
		}
		return nil
	}
	var calls []CallHierarchyOutgoingCall
	if err := c.Request(ctx, "callHierarchy/outgoingCalls", map[string]any{"item": items[0]}, &calls); err != nil {
		return err
	}
	for _, call := range calls {
		res.Calls = append(res.Calls, m.call(call.To, call.FromRanges))
	}
	return nil
}

func (m *Manager) call(item CallHierarchyItem, ranges []Range) Call {
	c := Call{
		Name: item.Name, Kind: KindName(item.Kind),
		Path: m.display(item.URI), Line: item.SelectionRange.Start.Line + 1,
	}
	for _, r := range ranges {
		c.Sites = append(c.Sites, r.Start.Line+1)
	}
	return c
}

func (m *Manager) diagnostics(ctx context.Context, c *Client, target *Target, res *Result) error {
	for _, d := range c.Diagnostics(ctx, target.Abs, diagnosticsWait) {
		res.Problems = append(res.Problems, Problem{
			Path:      target.Rel,
			Line:      d.Range.Start.Line + 1,
			Character: d.Range.Start.Character + 1,
			Severity:  SeverityLabel(d.Severity),
			Source:    d.Source,
			Code:      codeString(d.Code),
			Message:   strings.TrimSpace(d.Message),
		})
	}
	sort.Slice(res.Problems, func(i, j int) bool { return res.Problems[i].Line < res.Problems[j].Line })
	if len(res.Problems) == 0 && !c.Supports("diagnosticProvider") {
		res.Note = "this server volunteers diagnostics rather than answering on demand; nothing has been reported for this file"
	}
	return nil
}

func codeString(code any) string {
	switch v := code.(type) {
	case nil:
		return ""
	case string:
		return v
	case float64:
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprint(code)
}

// --- shared ---

// refs turns locations into answers, attaching the source line so the caller
// can usually stop there instead of making a read call per result.
func (m *Manager) refs(locations []Location, limit int, res *Result) []Ref {
	max := clampLimit(limit)
	cache := map[string][]string{}

	out := make([]Ref, 0, len(locations))
	for i, loc := range locations {
		if i >= max {
			res.Truncated = true
			break
		}
		abs := URIToPath(loc.URI)
		ref := Ref{
			Path:      m.display(loc.URI),
			Line:      loc.Range.Start.Line + 1,
			Character: loc.Range.Start.Character + 1,
		}
		lines, ok := cache[abs]
		if !ok {
			lines, _ = readLines(abs)
			cache[abs] = lines
		}
		if n := loc.Range.Start.Line; n >= 0 && n < len(lines) {
			ref.Text = strings.TrimSpace(lines[n])
		}
		out = append(out, ref)
	}
	return out
}

// display turns a URI back into the repo-prefixed path the file tools use, so
// a result can be handed straight to read or to git blame.
func (m *Manager) display(uri string) string {
	abs := URIToPath(uri)
	rel := m.Files.Display(abs)
	if strings.HasPrefix(rel, "..") {
		// Outside the checkouts: a standard-library or module-cache file. Say
		// where it really is rather than pretending it is in a repository.
		return abs
	}
	return rel
}

func clampLimit(limit int) int {
	if limit <= 0 {
		return DefaultLimit
	}
	if limit > MaxLimit {
		return MaxLimit
	}
	return limit
}

func describe(path string, pos Position) string {
	return fmt.Sprintf("%s:%d:%d", path, pos.Line+1, pos.Character+1)
}

func joinNotes(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	return a + "; " + b
}

func readLines(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n"), nil
}
