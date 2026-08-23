package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/notshekhar/drover/internal/api"
	"github.com/notshekhar/drover/internal/lsp"
)

// lspTools is the one navigation tool.
//
// Unconditional, like the file tools it sits beside. api_call and sql_query
// are conditional because without a configured request or connection there is
// literally nothing for them to act on; lsp acts on the same checkouts grep
// does, so if there is anything to grep there is something to navigate. And
// nothing is launched until a question is asked, so offering it costs nothing.
func (s *Server) lspTools(ctx context.Context) []Tool {
	return []Tool{{
		Name:        "lsp",
		Description: fmt.Sprintf(lspDescription, s.lspLanguages(ctx)),
		InputSchema: Schema{
			Type:       "object",
			Additional: boolPtr(false),
			Required:   []string{"operation"},
			Properties: map[string]Prop{
				"operation": {
					Type:        "string",
					Description: "Which question to ask. See this tool's description for what each one needs.",
					Enum:        lsp.Operations,
				},
				"path": {Type: "string", Description: "The file, spelled as the file tools spell it: `api/internal/db.go`. Required for everything except servers."},
				"symbol": {
					Type:        "string",
					Description: "The identifier you are asking about, e.g. `Connect`. Prefer this over character -- you almost always know the name and not the column. Combine with line to disambiguate.",
				},
				"line":       {Type: "integer", Description: "1-based line, exactly as read and grep print them."},
				"character":  {Type: "integer", Description: "1-based column. Only needed if you know it; symbol is easier."},
				"occurrence": {Type: "integer", Description: "Which occurrence of symbol to use when it appears more than once. 1 is the first."},
				"query":      {Type: "string", Description: "For workspace_symbols: the name to search for across the project."},
				"limit":      {Type: "integer", Description: "Cap the number of results. Defaults to 100."},
			},
		},
	}}
}

const lspDescription = "Navigate code by meaning rather than by text. " +
	"grep finds a string; this finds the SYMBOL -- the one definition, every real use, the type, the callers. " +
	"Read-only, and it never changes a checkout.\n\n" +
	"Operations:\n" +
	"  definition         where this symbol is defined.\n" +
	"  references         everywhere it is actually used -- not everywhere the text appears.\n" +
	"  hover              its type and doc comment.\n" +
	"  implementations    who implements this interface.\n" +
	"  document_symbols   one file's outline: what is declared in it, indented.\n" +
	"  workspace_symbols  find a symbol by name across the project. Start here when you know a name but not a file. Needs query.\n" +
	"  incoming_calls     who calls this function.\n" +
	"  outgoing_calls     what this function calls.\n" +
	"  diagnostics        what the compiler says about a file.\n" +
	"  servers            which languages are ready, and why one is not.\n\n" +
	"Positions: give `path` plus `symbol` -- the identifier's text. You do not need a column, and `line` is only needed when a name appears more than once in a file.\n\n" +
	"Languages: %s. A file in any other language has no server, and grep is the right tool for it.\n\n" +
	"The three tools compose: grep to find a candidate line, lsp to learn what the symbol really is and where it is used, git blame and git show to learn why it is like that."

// languageCacheTTL keeps the probe off the tools/list path.
//
// Working out what can start means asking a JVM its version, which is cheap
// once and wasteful on every listing. Thirty seconds is long enough that a
// polling client pays it rarely and short enough that installing a toolchain
// shows up while you are still looking.
const languageCacheTTL = 30 * time.Second

// lspLanguages names what is usable, so the description does not promise Java
// on a machine with no JVM.
func (s *Server) lspLanguages(ctx context.Context) string {
	s.langMu.Lock()
	if time.Since(s.langAt) < languageCacheTTL && s.langCache != "" {
		defer s.langMu.Unlock()
		return s.langCache
	}
	s.langMu.Unlock()

	langs, probed := s.probeLanguages(ctx)

	// Only a real probe refreshes the clock. A caller that hung up mid-list
	// gets the unprobed fallback, which is the right answer for them and the
	// wrong one to pin for the next thirty seconds -- the client after them
	// would be told Java works on a machine with no JVM. Same rule the
	// inventory cache uses for an engine that was down.
	if probed {
		s.langMu.Lock()
		s.langCache, s.langAt = langs, time.Now()
		s.langMu.Unlock()
	}
	return langs
}

// probeLanguages reports what it found and whether it got to ask at all.
func (s *Server) probeLanguages(ctx context.Context) (string, bool) {
	res, err := s.Backend.LSP(ctx, api.LSPRequest{Operation: "servers"})
	if err != nil {
		// The tool is still offered: its servers operation is exactly how you
		// find out why this failed.
		return strings.Join(lsp.Languages(), ", "), false
	}
	var ready, missing []string
	for _, server := range res.Servers {
		switch server.State {
		case "unavailable":
			missing = append(missing, server.Language)
		default:
			ready = append(ready, server.Language)
		}
	}
	if len(ready) == 0 {
		return strings.Join(missing, ", ") + " -- none can start on this machine; ask the servers operation why", true
	}
	out := strings.Join(ready, ", ")
	if len(missing) > 0 {
		out += " (" + strings.Join(missing, " and ") + " cannot start here -- ask the servers operation why)"
	}
	return out, true
}

// --- the call ---

func (s *Server) toolLSP(ctx context.Context, raw json.RawMessage) *CallResult {
	var args api.LSPRequest
	if err := decodeArgs(raw, &args); err != nil {
		return toolError("invalid arguments: %v", err)
	}
	if strings.TrimSpace(args.Operation) == "" {
		return toolError("lsp needs an operation: one of %s", strings.Join(lsp.Operations, ", "))
	}
	res, err := s.Backend.LSP(ctx, args)
	if err != nil {
		return toolError("%v", err)
	}
	return text("%s", renderLSP(res))
}

func renderLSP(res *api.LSPResponse) string {
	var b strings.Builder

	if res.Operation != "servers" {
		fmt.Fprintf(&b, "%s: %s", res.Operation, orDash(res.Position, res.Path))
		if res.Language != "" {
			fmt.Fprintf(&b, " (%s)", res.Language)
		}
		b.WriteString("\n\n")
	}

	switch res.Operation {
	case "definition", "implementations", "references":
		writeLocations(&b, res)
	case "hover":
		writeHover(&b, res)
	case "document_symbols", "workspace_symbols":
		writeSymbols(&b, res)
	case "incoming_calls", "outgoing_calls":
		writeCalls(&b, res)
	case "diagnostics":
		writeProblems(&b, res)
	case "servers":
		writeServers(&b, res)
	}

	if res.Note != "" {
		fmt.Fprintf(&b, "\n(%s)\n", res.Note)
	}
	if res.ServerState != "" {
		fmt.Fprintf(&b, "\n(the server is still busy: %s -- ask again if this looks incomplete)\n", res.ServerState)
	}
	if res.Truncated {
		b.WriteString("\n(more results; raise limit)\n")
	}
	return b.String()
}

func writeLocations(b *strings.Builder, res *api.LSPResponse) {
	if len(res.Refs) == 0 {
		switch res.Operation {
		case "references":
			b.WriteString("Nothing uses it.\n")
		default:
			b.WriteString("No result. The symbol may be from a dependency that was never installed in this checkout.\n")
		}
		return
	}
	for _, r := range res.Refs {
		fmt.Fprintf(b, "%s:%d:%d", r.Path, r.Line, r.Character)
		if r.Text != "" {
			fmt.Fprintf(b, "  %s", r.Text)
		}
		b.WriteString("\n")
	}
	if res.Operation == "references" {
		fmt.Fprintf(b, "\n%d use(s).\n", len(res.Refs))
	}
}

func writeHover(b *strings.Builder, res *api.LSPResponse) {
	if res.Hover == "" {
		b.WriteString("The server has nothing to say about that position.\n")
		return
	}
	b.WriteString(res.Hover + "\n")
}

func writeSymbols(b *strings.Builder, res *api.LSPResponse) {
	if len(res.Symbols) == 0 {
		b.WriteString("Nothing found.\n")
		return
	}
	outline := res.Operation == "document_symbols"
	for _, s := range res.Symbols {
		if outline {
			fmt.Fprintf(b, "%s%d: %s %s", strings.Repeat("  ", s.Depth), s.Line, s.Kind, s.Name)
			if s.Detail != "" {
				fmt.Fprintf(b, " %s", s.Detail)
			}
			b.WriteString("\n")
			continue
		}
		fmt.Fprintf(b, "%s:%d  %s %s", s.Path, s.Line, s.Kind, s.Name)
		if s.Detail != "" {
			fmt.Fprintf(b, "  (in %s)", s.Detail)
		}
		b.WriteString("\n")
	}
}

func writeCalls(b *strings.Builder, res *api.LSPResponse) {
	if len(res.Calls) == 0 {
		b.WriteString("None.\n")
		return
	}
	direction := "calls it from"
	if res.Operation == "outgoing_calls" {
		direction = "called at"
	}
	for _, c := range res.Calls {
		fmt.Fprintf(b, "%s %s  %s:%d", c.Kind, c.Name, c.Path, c.Line)
		if len(c.Sites) > 0 {
			fmt.Fprintf(b, "  (%s line %s)", direction, joinInts(c.Sites))
		}
		b.WriteString("\n")
	}
}

func writeProblems(b *strings.Builder, res *api.LSPResponse) {
	if len(res.Problems) == 0 {
		b.WriteString("No problems reported.\n")
		return
	}
	for _, p := range res.Problems {
		fmt.Fprintf(b, "%s [%d:%d] %s", strings.ToUpper(p.Severity), p.Line, p.Character, p.Message)
		if p.Source != "" {
			fmt.Fprintf(b, " (%s", p.Source)
			if p.Code != "" {
				fmt.Fprintf(b, " %s", p.Code)
			}
			b.WriteString(")")
		}
		b.WriteString("\n")
	}
}

func writeServers(b *strings.Builder, res *api.LSPResponse) {
	b.WriteString("language servers:\n\n")
	for _, s := range res.Servers {
		fmt.Fprintf(b, "%-12s %-12s", s.Language, s.State)
		if s.Repo != "" {
			fmt.Fprintf(b, " %s", s.Root)
		}
		b.WriteString("\n")
		if s.Detail != "" {
			fmt.Fprintf(b, "             %s\n", s.Detail)
		}
		if s.Source != "" {
			fmt.Fprintf(b, "             %s", s.Source)
			if s.Version != "" {
				fmt.Fprintf(b, ", version %s", s.Version)
			}
			b.WriteString("\n")
		}
		if s.State == "running" {
			fmt.Fprintf(b, "             up %s, %d request(s), idle %s\n", duration(s.UptimeSec), s.Requests, duration(s.IdleSec))
		}
	}
}

func joinInts(values []int) string {
	parts := make([]string, 0, len(values))
	for _, v := range values {
		parts = append(parts, fmt.Sprint(v))
	}
	return strings.Join(parts, ", ")
}

func duration(seconds int64) string {
	switch {
	case seconds < 60:
		return fmt.Sprintf("%ds", seconds)
	case seconds < 3600:
		return fmt.Sprintf("%dm", seconds/60)
	}
	return fmt.Sprintf("%dh%dm", seconds/3600, (seconds%3600)/60)
}

func orDash(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return "-"
}
