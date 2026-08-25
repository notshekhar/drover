package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/notshekhar/drover/internal/api"
	"github.com/notshekhar/drover/internal/object"
)

// docTools is the one write drover offers.
//
// It carries a catalogue in its description, the way sql_query does, and for
// the same reason: reads need no catalogue because `ls` is right there, but a
// write has to pick a destination before it has looked at anything.
//
// It is offered only when a writable store exists. An engine with no store
// advertises no way to write, which is the honest shape of a read-only
// context engine.
func (s *Server) docTools(ctx context.Context) []Tool {
	stores := s.writableStores(ctx)
	if len(stores) == 0 {
		return nil
	}

	return []Tool{{
		Name: "doc_write",
		Description: "Write or replace one markdown document in a document store. " +
			"This is the ONLY tool in drover that changes anything -- everything else is read-only. " +
			"Use it to leave behind what you worked out: a design note, a decision record, an investigation writeup. " +
			"Writing replaces the whole file, so `read` it first if you mean to edit rather than overwrite. " +
			"Every write is committed locally with your attribution and your stated reason, so it can be reviewed and undone.\n\n" +
			storeCatalog(stores),
		InputSchema: Schema{
			Type:       "object",
			Additional: boolPtr(false),
			Required:   []string{"store", "path", "content"},
			Properties: map[string]Prop{
				"store": {
					Type:        "string",
					Description: "Which store to write into -- one of those listed in this tool's description.",
					Enum:        storeNames(stores),
				},
				"path": {
					Type:        "string",
					Description: "Path inside the store, ending in .md, e.g. `prd-billing.md` or `decisions/0001-why-postgres.md`. Directories are created as needed.",
				},
				"content": {
					Type:        "string",
					Description: "The whole document, as markdown. This replaces any existing file at that path.",
				},
				"reason": {
					Type:        "string",
					Description: "One short line on why you are writing this. It becomes the commit message and is shown to the human running this engine.",
				},
			},
		},
	}}
}

func (s *Server) writableStores(ctx context.Context) []api.ObjectView {
	items, err := s.Backend.List(ctx, object.KindDocumentStore)
	if err != nil {
		return nil
	}
	var out []api.ObjectView
	for _, v := range items {
		if v.Writable {
			out = append(out, v)
		}
	}
	return out
}

// storeCatalog names the stores and what each is for, so a model picks a
// destination rather than inventing one.
func storeCatalog(stores []api.ObjectView) string {
	var b strings.Builder
	b.WriteString("Stores available:\n")
	for _, v := range stores {
		if v.Description != "" {
			fmt.Fprintf(&b, "  %s -- %s\n", v.Name, v.Description)
			continue
		}
		fmt.Fprintf(&b, "  %s\n", v.Name)
	}
	b.WriteString("\nRead what is already there with ls, read and grep under documents/<store>/.")
	return b.String()
}

func storeNames(stores []api.ObjectView) []string {
	out := make([]string, 0, len(stores))
	for _, v := range stores {
		out = append(out, v.Name)
	}
	return out
}

func (s *Server) toolDocWrite(ctx context.Context, raw json.RawMessage) *CallResult {
	var args struct {
		Store   string `json:"store"`
		Path    string `json:"path"`
		Content string `json:"content"`
		Reason  string `json:"reason"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return toolError("invalid arguments: %v", err)
	}
	if strings.TrimSpace(args.Store) == "" {
		return toolError("doc_write needs a store; this tool's description lists them")
	}
	if strings.TrimSpace(args.Content) == "" {
		return toolError("doc_write needs content; an empty document is not worth a commit")
	}

	res, err := s.Backend.DocWrite(ctx, args.Store, api.DocWriteRequest{
		Path:    args.Path,
		Content: args.Content,
		Reason:  args.Reason,
	})
	if err != nil {
		return toolError("%v", err)
	}

	switch {
	case res.Unchanged:
		return text("%s is already exactly this. Nothing written.", res.Path)
	case res.Created:
		return text("Created %s (%d bytes)%s", res.Path, res.Bytes, commitNote(res.Commit))
	default:
		return text("Replaced %s (%d bytes)%s", res.Path, res.Bytes, commitNote(res.Commit))
	}
}

func commitNote(sha string) string {
	if sha == "" {
		return "."
	}
	return ", committed as " + sha + "."
}
