package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/notshekhar/drover/internal/activity"
	"github.com/notshekhar/drover/internal/api"
	"github.com/notshekhar/drover/internal/mcp"
	"github.com/notshekhar/drover/internal/object"
)

// recordingBackend is the one wrap around every tool path. MCP HTTP, the
// stdio bridge (via REST) and `drover call` / `query` all land here once.
type recordingBackend struct {
	inner mcp.Backend
	rec   *activity.Ledger
	// knows reports whether a name is a stored Repository. The first segment
	// of a file path is only a repository if one is actually called that;
	// without the check, `read no/such/file.go` files itself under a
	// repository named "no", and the log grows fictional repositories that
	// the dashboard then offers as filters.
	knows func(name string) bool
}

func (b *recordingBackend) repo(path string) string {
	name := repoFromPath(path)
	if name == "" || b.knows == nil || !b.knows(name) {
		return ""
	}
	return name
}

func (b *recordingBackend) List(ctx context.Context, kind object.Kind) ([]api.ObjectView, error) {
	return b.inner.List(ctx, kind)
}

// DocWrite is recorded like any other tool call, and more carefully than
// most: it is the only one that changes anything.
func (b *recordingBackend) DocWrite(ctx context.Context, store string, req api.DocWriteRequest) (*api.DocWriteResponse, error) {
	start := time.Now()
	res, err := b.inner.DocWrite(ctx, store, req)
	rec := activity.Record{
		Tool:   "doc_write",
		Args:   map[string]any{"store": store, "path": req.Path, "bytes": len(req.Content)},
		Reason: req.Reason,
		Object: store,
	}
	if err == nil && res != nil {
		rec.Bytes = res.Bytes
		rec.Outcome = "ok"
		switch {
		case res.Unchanged:
			rec.Summary = res.Path + " · unchanged"
		case res.Created:
			rec.Summary = res.Path + " · created · " + res.Commit
		default:
			rec.Summary = res.Path + " · updated · " + res.Commit
		}
	}
	b.finish(ctx, rec, start, err)
	return res, err
}

// Hotspots is not a tool call and is not recorded. Recording the query that
// reads the log into the log would be a feedback loop with nothing in it.
func (b *recordingBackend) Hotspots(ctx context.Context) (*api.HotspotsResponse, error) {
	return b.inner.Hotspots(ctx)
}

func (b *recordingBackend) Get(ctx context.Context, kind object.Kind, name string) (*api.ObjectView, error) {
	return b.inner.Get(ctx, kind, name)
}

func (b *recordingBackend) Ls(ctx context.Context, req api.LsRequest) (*api.LsResponse, error) {
	start := time.Now()
	res, err := b.inner.Ls(ctx, req)
	rec := activity.Record{
		Tool:       "ls",
		Args:       map[string]any{"path": req.Path},
		Repository: b.repo(req.Path),
	}
	if err == nil {
		n := 0
		if res != nil {
			n = len(res.Entries)
			rec.Truncated = res.Truncated
		}
		rec.Summary = fmt.Sprintf("%d entries", n)
		if n == 0 {
			rec.Outcome = "empty"
		} else {
			rec.Outcome = "ok"
		}
	}
	b.finish(ctx, rec, start, err)
	return res, err
}

func (b *recordingBackend) ReadFile(ctx context.Context, req api.ReadRequest) (*api.ReadResponse, error) {
	start := time.Now()
	res, err := b.inner.ReadFile(ctx, req)
	rec := activity.Record{
		Tool:       "read",
		Args:       map[string]any{"path": req.Path},
		Repository: b.repo(req.Path),
	}
	if err == nil && res != nil {
		rec.Summary = fmt.Sprintf("%s:%d-%d", res.Path, res.StartLine, res.EndLine)
		rec.Bytes = len(res.Content)
		rec.Truncated = res.Truncated
		rec.Outcome = "ok"
	}
	b.finish(ctx, rec, start, err)
	return res, err
}

func (b *recordingBackend) Grep(ctx context.Context, req api.GrepRequest) (*api.GrepResponse, error) {
	start := time.Now()
	res, err := b.inner.Grep(ctx, req)
	rec := activity.Record{
		Tool:       "grep",
		Args:       map[string]any{"pattern": req.Pattern, "path": req.Path},
		Repository: b.repo(req.Path),
	}
	if err == nil && res != nil {
		rec.Truncated = res.Truncated
		rec.Summary = fmt.Sprintf("%d matches in %d files", len(res.Matches), res.Files)
		if rec.Truncated {
			rec.Summary += " · truncated"
		}
		if len(res.Matches) == 0 {
			rec.Outcome = "empty"
		} else {
			rec.Outcome = "ok"
		}
	}
	b.finish(ctx, rec, start, err)
	return res, err
}

func (b *recordingBackend) Find(ctx context.Context, req api.FindRequest) (*api.FindResponse, error) {
	start := time.Now()
	res, err := b.inner.Find(ctx, req)
	rec := activity.Record{
		Tool:       "find",
		Args:       map[string]any{"pattern": req.Pattern, "path": req.Path},
		Repository: b.repo(req.Path),
	}
	if err == nil && res != nil {
		n := len(res.Paths)
		rec.Truncated = res.Truncated
		rec.Summary = fmt.Sprintf("%d paths", n)
		if n == 0 {
			rec.Outcome = "empty"
		} else {
			rec.Outcome = "ok"
		}
	}
	b.finish(ctx, rec, start, err)
	return res, err
}

func (b *recordingBackend) Git(ctx context.Context, req api.GitRequest) (*api.GitResponse, error) {
	start := time.Now()
	res, err := b.inner.Git(ctx, req)
	rec := activity.Record{
		Tool:       "git",
		Op:         req.Operation,
		Args:       map[string]any{"operation": req.Operation, "path": req.Path, "repository": req.Repository},
		Repository: req.Repository,
	}
	if rec.Repository == "" {
		rec.Repository = b.repo(req.Path)
	}
	if err == nil && res != nil {
		rec.Truncated = res.Truncated
		rec.Summary = gitSummary(req, res)
		rec.Outcome = "ok"
		if rec.Summary == "" {
			rec.Outcome = "empty"
		}
	}
	b.finish(ctx, rec, start, err)
	return res, err
}

func (b *recordingBackend) LSP(ctx context.Context, req api.LSPRequest) (*api.LSPResponse, error) {
	start := time.Now()
	res, err := b.inner.LSP(ctx, req)
	rec := activity.Record{
		Tool:       "lsp",
		Op:         req.Operation,
		Args:       map[string]any{"operation": req.Operation, "path": req.Path, "symbol": req.Symbol},
		Repository: b.repo(req.Path),
	}
	if err == nil && res != nil {
		rec.Summary = lspSummary(req, res)
		if rec.Summary == "" {
			rec.Outcome = "empty"
		} else {
			rec.Outcome = "ok"
		}
	}
	b.finish(ctx, rec, start, err)
	return res, err
}

func (b *recordingBackend) Call(ctx context.Context, name string, req api.CallRequest) (*api.CallResponse, error) {
	start := time.Now()
	res, err := b.inner.Call(ctx, name, req)
	args := map[string]any{"name": name}
	if req.Environment != "" {
		args["environment"] = req.Environment
	}
	if len(req.Params) > 0 {
		// Params as received, never the resolved URL.
		p := map[string]any{}
		for k, v := range req.Params {
			p[k] = v
		}
		args["params"] = p
	}
	rec := activity.Record{Tool: "api_call", Object: name, Args: args}
	if err == nil && res != nil {
		rec.Summary = fmt.Sprintf("%s %s → %d · %d B · %dms", res.Method, name, res.Status, len(res.Body), res.DurationMS)
		rec.Bytes = len(res.Body)
		rec.Truncated = res.Truncated
		rec.Outcome = "ok"
	}
	b.finish(ctx, rec, start, err)
	return res, err
}

func (b *recordingBackend) Query(ctx context.Context, name, query string) (*api.QueryResponse, error) {
	start := time.Now()
	res, err := b.inner.Query(ctx, name, query)
	rec := activity.Record{
		Tool:   "sql_query",
		Object: name,
		Args:   map[string]any{"connection": name, "query": query},
	}
	if err == nil && res != nil {
		rec.Summary = fmt.Sprintf("%s · %d rows · %dms", name, res.RowCount, res.ElapsedMS)
		if res.Truncated {
			rec.Summary = fmt.Sprintf("%s · %d rows (truncated) · %dms", name, res.RowCount, res.ElapsedMS)
			rec.Truncated = true
		}
		if res.RowCount == 0 {
			rec.Outcome = "empty"
		} else {
			rec.Outcome = "ok"
		}
	}
	b.finish(ctx, rec, start, err)
	return res, err
}

func (b *recordingBackend) finish(ctx context.Context, rec activity.Record, start time.Time, err error) {
	if b.rec == nil {
		return
	}
	rec.At = start
	rec.Duration = time.Since(start)
	c := activity.CallerFrom(ctx)
	if rec.Source == "" {
		rec.Source = c.Source
	}
	if rec.Source == "" {
		rec.Source = "cli"
	}
	rec.Client = c.Client
	rec.Session = c.Session
	if err != nil {
		if errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
			rec.Outcome = "cancelled"
		} else {
			rec.Outcome = "error"
		}
		rec.Error = err.Error()
		if rec.Summary == "" {
			rec.Summary = err.Error()
		}
	}
	_ = b.rec.Record(rec)
}

func repoFromPath(path string) string {
	path = strings.TrimPrefix(path, "/")
	if path == "" {
		return ""
	}
	name, _, _ := strings.Cut(path, "/")
	return name
}

func gitSummary(req api.GitRequest, res *api.GitResponse) string {
	switch req.Operation {
	case "log":
		n := len(res.Commits)
		if n == 0 {
			return ""
		}
		return fmt.Sprintf("log · %d commits", n)
	case "blame":
		if req.Path == "" {
			return "blame"
		}
		return fmt.Sprintf("blame %s · %d lines", req.Path, len(res.Blame))
	case "show":
		if res.Commit != nil {
			return "show " + res.Commit.Short
		}
		return "show"
	default:
		if req.Path != "" {
			return req.Operation + " " + req.Path
		}
		return req.Operation
	}
}

func lspSummary(req api.LSPRequest, res *api.LSPResponse) string {
	n := len(res.Refs)
	if n == 0 {
		n = len(res.Symbols)
	}
	if n == 0 {
		n = len(res.Calls)
	}
	if n == 0 {
		n = len(res.Problems)
	}
	if n == 0 && res.Hover == "" && req.Operation != "servers" {
		return ""
	}
	if req.Operation == "servers" {
		return fmt.Sprintf("servers · %d", len(res.Servers))
	}
	if res.Hover != "" {
		return req.Operation + " · hover"
	}
	return fmt.Sprintf("%s · %d", req.Operation, n)
}
