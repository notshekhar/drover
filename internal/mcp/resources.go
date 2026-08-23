package mcp

import (
	"context"
	"encoding/json"

	"github.com/notshekhar/drover/internal/docs"
)

// Two resources, and deliberately only two.
//
// A resource is read when the model decides it needs it, which is the right
// shape for a long reference and for a snapshot that goes stale -- neither
// belongs in a tool description that is re-sent on every tools/list.
//
// It is not the right shape for a catalogue. Listing every configured request
// as its own resource would be the mistake that made twenty requests into
// twenty tools, wearing a different hat: api_list is the catalogue.
const (
	uriReference = "drover://reference"
	uriInventory = "drover://inventory"
)

func (s *Server) listResources(_ context.Context, _ json.RawMessage) (any, *rpcError) {
	return map[string]any{"resources": []map[string]any{
		{
			"uri":         uriReference,
			"name":        "drover reference",
			"description": "How to configure drover: every kind that can be applied, its fields, and the placeholder rules. Read this before writing a drover yaml file.",
			"mimeType":    "text/markdown",
		},
		{
			"uri":         uriInventory,
			"name":        "drover inventory",
			"description": "What this engine holds right now -- its repositories and their sync state, environments, requests and databases. Read this when the list you were given at connect may have changed.",
			"mimeType":    "text/markdown",
		},
	}}, nil
}

func (s *Server) readResource(ctx context.Context, params json.RawMessage) (any, *rpcError) {
	var req struct {
		URI string `json:"uri"`
	}
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, errf(CodeInvalidParams, "invalid params: %v", err)
	}

	var body string
	switch req.URI {
	case uriReference:
		// The embedded copy, never ~/.drover/docs.md. That file is written
		// once and then never overwritten, on purpose, so a data directory
		// first created by an older drover still describes that older drover.
		// docs.Markdown always matches the binary that is answering.
		body = docs.Markdown
	case uriInventory:
		body = s.inventory(ctx)
		if body == "" {
			body = "This engine holds nothing yet. Apply a Repository to give it something to serve; the drover://reference resource explains how."
		}
	default:
		// A resource that does not exist is a call that could not be made, so
		// it is a JSON-RPC error rather than an isError result.
		return nil, errf(CodeInvalidParams, "unknown resource %q", req.URI)
	}

	return map[string]any{"contents": []map[string]any{{
		"uri":      req.URI,
		"mimeType": "text/markdown",
		"text":     body,
	}}}, nil
}
