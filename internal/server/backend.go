package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/notshekhar/drover/internal/activity"
	"github.com/notshekhar/drover/internal/api"
	"github.com/notshekhar/drover/internal/files"
	"github.com/notshekhar/drover/internal/git"
	"github.com/notshekhar/drover/internal/httpreq"
	"github.com/notshekhar/drover/internal/lsp"
	"github.com/notshekhar/drover/internal/mcp"
	"github.com/notshekhar/drover/internal/object"
	"github.com/notshekhar/drover/internal/store"
)

// backend implements mcp.Backend directly against this server's own store and
// executors, and is the one path every tool call takes. The REST handlers are
// adapters over it rather than a second implementation: two copies of "run a
// grep and shape the response" is two places for the answer to drift, and one
// place is what makes a single wrapper able to see every call.
//
// The alternative -- having the /mcp endpoint call the engine's own HTTP API
// over loopback -- would work, but it makes every tool call a second network
// hop through the process's own listener, and breaks if the listener moved
// because its port was taken.
type backend struct{ s *Server }

// Backend returns an mcp.Backend over this server. It is exported so the MCP
// endpoint and any in-process caller share one implementation.
func (s *Server) Backend() mcp.Backend {
	if s.exec != nil {
		return s.exec
	}
	return &backend{s: s}
}

func (b *backend) List(_ context.Context, kind object.Kind) ([]api.ObjectView, error) {
	objs, err := b.s.store.List(kind)
	if err != nil {
		return nil, err
	}
	out := make([]api.ObjectView, 0, len(objs))
	for _, o := range objs {
		out = append(out, b.s.viewWithStatus(o))
	}
	return out, nil
}

func (b *backend) Get(_ context.Context, kind object.Kind, name string) (*api.ObjectView, error) {
	o, err := b.s.store.Get(kind, name)
	if err != nil {
		return nil, err
	}
	v := b.s.viewWithStatus(o)
	return &v, nil
}

func (b *backend) Ls(_ context.Context, req api.LsRequest) (*api.LsResponse, error) {
	res, err := b.s.files.List(req.Path)
	if err != nil {
		return nil, err
	}
	out := &api.LsResponse{Path: res.Path, Entries: []api.FileEntry{}, Truncated: res.Truncated}
	for _, e := range res.Entries {
		out.Entries = append(out.Entries, api.FileEntry{Name: e.Name, Path: e.Path, Type: e.Type, Size: e.Size, Root: e.Root})
	}
	return out, nil
}

func (b *backend) ReadFile(_ context.Context, req api.ReadRequest) (*api.ReadResponse, error) {
	res, err := b.s.files.Read(req.Path, req.Offset, req.Limit)
	if err != nil {
		return nil, err
	}
	return &api.ReadResponse{
		Path:       res.Path,
		Content:    res.Content,
		StartLine:  res.StartLine,
		EndLine:    res.EndLine,
		TotalLines: res.TotalLines,
		Truncated:  res.Truncated,
	}, nil
}

func (b *backend) Grep(ctx context.Context, req api.GrepRequest) (*api.GrepResponse, error) {
	only, err := b.selected(req.Selector)
	if err != nil {
		return nil, err
	}
	res, err := b.s.files.Grep(ctx, req.Pattern, files.GrepOptions{
		Path:          req.Path,
		Include:       req.Include,
		CaseSensitive: req.CaseSensitive,
		MaxResults:    req.MaxResults,
		Only:          only,
	})
	if err != nil {
		return nil, err
	}
	out := &api.GrepResponse{Matches: []api.GrepMatch{}, Files: res.Files, Truncated: res.Truncated, Unsearched: res.Unsearched}
	for _, m := range res.Matches {
		out.Matches = append(out.Matches, api.GrepMatch{Path: m.Path, Line: m.Line, Text: m.Text})
	}
	return out, nil
}

func (b *backend) Find(ctx context.Context, req api.FindRequest) (*api.FindResponse, error) {
	only, err := b.selected(req.Selector)
	if err != nil {
		return nil, err
	}
	res, err := b.s.files.Find(ctx, req.Pattern, files.FindOptions{Path: req.Path, MaxResults: req.MaxResults, Only: only})
	if err != nil {
		return nil, err
	}
	return &api.FindResponse{Paths: res.Paths, Truncated: res.Truncated, Unsearched: res.Unsearched}, nil
}

// selected resolves a label selector to the repository names it matches.
//
// An empty selector resolves to no names at all rather than to every name,
// because "no restriction" and "restricted to everything" have to stay
// distinguishable -- the first searches the root, the second would search a
// list that happens to be complete today.
//
// A selector that matches nothing is an error, not an empty result. Silently
// searching zero files and reporting no matches is the most misleading answer
// a search tool can give.
func (b *backend) selected(expr string) ([]string, error) {
	sel, err := object.ParseSelector(expr)
	if err != nil {
		return nil, err
	}
	if len(sel) == 0 {
		return nil, nil
	}
	objs, err := b.s.store.List(object.KindRepository)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, o := range objs {
		if sel.Matches(o.Metadata.Labels) {
			names = append(names, o.Metadata.Name)
		}
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("no repository matches the selector %q; `drover get repository` lists what is here, with its labels", sel)
	}
	return names, nil
}

// DocWrite is the only write path an agent can reach.
//
// The author on the commit is the attribution the ledger already resolves, so
// "who wrote this" is answerable with the git tool that already exists rather
// than with a second audit mechanism.
func (b *backend) DocWrite(ctx context.Context, store string, req api.DocWriteRequest) (*api.DocWriteResponse, error) {
	return b.s.writeDocument(ctx, store, req, describeCaller(activity.CallerFrom(ctx)))
}

// describeCaller renders attribution the way a commit author reads.
func describeCaller(c activity.Caller) string {
	client := c.Client
	if client == "" {
		client = "an agent"
	}
	if c.Source == "" {
		return client
	}
	return client + " via " + c.Source
}

// Hotspots is the behavioural index: what agents actually read here.
func (b *backend) Hotspots(ctx context.Context) (*api.HotspotsResponse, error) {
	out := &api.HotspotsResponse{}
	spots, err := b.s.ledger.Hotspots(ctx, hotspotWindow, 5)
	if err != nil {
		return nil, err
	}
	for _, h := range spots {
		out.Hotspots = append(out.Hotspots, api.Hotspot{Repository: h.Repository, Path: h.Path, Reads: h.Reads})
	}
	return out, nil
}

func (b *backend) Call(ctx context.Context, name string, req api.CallRequest) (*api.CallResponse, error) {
	return b.s.callRequest(ctx, name, req)
}

func (b *backend) Query(ctx context.Context, name, query string) (*api.QueryResponse, error) {
	spec, err := b.s.sqlSpecFor(name)
	if err != nil {
		return nil, err
	}
	res, err := b.s.sql.Query(ctx, name, spec, query)
	if err != nil {
		return nil, err
	}
	return &api.QueryResponse{
		Columns:   res.Columns,
		Rows:      res.Rows,
		RowCount:  res.RowCount,
		Truncated: res.Truncated,
		Provider:  res.Provider,
		ElapsedMS: res.Elapsed,
	}, nil
}

// --- shared with the HTTP handlers ---

// callRequest executes one stored HTTPRequest. Both the REST endpoint and the
// MCP tool go through here, so the two cannot diverge on which environment is
// selected or which methods are permitted.
func (s *Server) callRequest(ctx context.Context, name string, req api.CallRequest) (*api.CallResponse, error) {
	o, err := s.store.Get(object.KindHTTPRequest, name)
	if err != nil {
		return nil, err
	}
	spec, err := o.HTTPRequest()
	if err != nil {
		return nil, err
	}

	envName, err := spec.SelectEnvironment(req.Environment)
	if err != nil {
		return nil, err
	}
	var envSpec *object.EnvironmentSpec
	if envName != "" {
		envObj, err := s.store.Get(object.KindEnvironment, envName)
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("environment %q does not exist", envName)
		}
		if err != nil {
			return nil, err
		}
		envSpec, err = envObj.Environment()
		if err != nil {
			return nil, err
		}
	}

	resp, err := s.http.Do(ctx, httpreq.Request{
		Spec:              spec,
		Environment:       envSpec,
		EnvName:           envName,
		Params:            req.Params,
		AllowUnsafeMethod: req.AllowUnsafeMethod,
	})
	if err != nil {
		return nil, err
	}
	return &api.CallResponse{
		Status:     resp.Status,
		URL:        resp.URL,
		Method:     resp.Method,
		Headers:    resp.Headers,
		Body:       resp.Body,
		Truncated:  resp.Truncated,
		DurationMS: resp.DurationMS,
	}, nil
}

// sqlSpecFor loads a SQLConnection spec by name.
func (s *Server) sqlSpecFor(name string) (*object.SQLConnectionSpec, error) {
	o, err := s.store.Get(object.KindSQLConnection, name)
	if err != nil {
		return nil, err
	}
	return o.SQLConnection()
}

// isNotFound reports whether an error should map to 404 on the REST surface.
func isNotFound(err error) bool { return errors.Is(err, store.ErrNotFound) }

// isForbidden reports whether an error is a jail violation.
func isForbidden(err error) bool { return errors.Is(err, files.ErrOutsideRoot) }

// unsupportedMethod recognises the executor's refusal to run a non-GET, which
// is a 400 rather than a 500.
func unsupportedMethod(err error) bool {
	return err != nil && strings.Contains(err.Error(), "only executes GET")
}

// --- git ---

func (b *backend) Git(ctx context.Context, req api.GitRequest) (*api.GitResponse, error) {
	return b.s.gitQuery(ctx, req)
}

// gitQuery runs one history operation and converts the engine's result to the
// wire shape. The REST endpoint and the MCP tool both come through here, so
// neither can end up with an operation the other does not have.
func (s *Server) gitQuery(ctx context.Context, req api.GitRequest) (*api.GitResponse, error) {
	res, err := s.git.Run(ctx, git.Options{
		Operation:  req.Operation,
		Repository: req.Repository,
		Path:       req.Path,
		Rev:        req.Rev,
		From:       req.From,
		To:         req.To,
		Author:     req.Author,
		Since:      req.Since,
		Until:      req.Until,
		Grep:       req.Grep,
		Query:      req.Query,
		Regex:      req.Regex,
		Merges:     req.Merges,
		Patch:      req.Patch,
		Lines:      req.Lines,
		Limit:      req.Limit,
	})
	if err != nil {
		return nil, err
	}
	return gitView(res), nil
}

func gitView(res *git.Result) *api.GitResponse {
	out := &api.GitResponse{
		Operation:  res.Operation,
		Repository: res.Repository,
		Branch:     res.Branch,
		Range:      res.Range,
		Rev:        res.Rev,
		Path:       res.Path,
		Patch:      res.Patch,
		Content:    res.Content,
		Truncated:  res.Truncated,
		Note:       res.Note,
	}
	for _, c := range res.Commits {
		out.Commits = append(out.Commits, gitCommitView(c))
	}
	if res.Commit != nil {
		c := gitCommitView(*res.Commit)
		out.Commit = &c
	}
	for _, f := range res.Files {
		out.Files = append(out.Files, api.GitFileChange{
			Status:     f.Status,
			Path:       f.Path,
			OldPath:    f.OldPath,
			Insertions: f.Insertions,
			Deletions:  f.Deletions,
			Binary:     f.Binary,
		})
	}
	for _, l := range res.Blame {
		out.Blame = append(out.Blame, api.GitBlameLine{
			Line: l.Line, Hash: l.Hash, Short: l.Short,
			Author: l.Author, Date: l.Date, Summary: l.Summary, Text: l.Text,
		})
	}
	for _, r := range res.Refs {
		out.Refs = append(out.Refs, api.GitRef{
			Name: r.Name, Type: r.Type, Hash: r.Hash, Short: r.Short,
			Date: r.Date, Subject: r.Subject, Head: r.Head,
		})
	}
	for _, a := range res.Authors {
		out.Authors = append(out.Authors, api.GitAuthor{
			Name: a.Name, Email: a.Email, Commits: a.Commits, First: a.First, Last: a.Last,
		})
	}
	if res.Status != nil {
		out.Status = &api.GitStatus{
			Repository: res.Status.Repository,
			URL:        res.Status.URL,
			Branch:     res.Status.Branch,
			Head:       gitCommitView(res.Status.Head),
			Commits:    res.Status.Commits,
			FirstDate:  res.Status.FirstDate,
			Clean:      res.Status.Clean,
			Dirty:      res.Status.Dirty,
			LastFetch:  res.Status.LastFetch,
		}
	}
	return out
}

func gitCommitView(c git.Commit) api.GitCommit {
	return api.GitCommit{
		Hash:       c.Hash,
		Short:      c.Short,
		Author:     c.Author,
		Email:      c.Email,
		Date:       c.Date,
		Committer:  c.Committer,
		CommitDate: c.CommitDate,
		Parents:    c.Parents,
		Subject:    c.Subject,
		Body:       c.Body,
		Files:      c.Files,
		Insertions: c.Insertions,
		Deletions:  c.Deletions,
	}
}

// gitErrStatus separates "there is no such repository" from "that argument
// makes no sense", so a REST caller does not have to read the message to know
// which it was.
func gitErrStatus(err error) int {
	switch {
	case errors.Is(err, git.ErrNoRepository):
		return http.StatusNotFound
	case strings.HasPrefix(err.Error(), "no checkout named"):
		return http.StatusNotFound
	default:
		return http.StatusBadRequest
	}
}

// --- lsp ---

func (b *backend) LSP(ctx context.Context, req api.LSPRequest) (*api.LSPResponse, error) {
	return b.s.lspQuery(ctx, req)
}

// lspQuery answers one navigation question.
func (s *Server) lspQuery(ctx context.Context, req api.LSPRequest) (*api.LSPResponse, error) {
	res, err := s.lsp.Run(ctx, lsp.Request{
		Operation:  req.Operation,
		Path:       req.Path,
		Line:       req.Line,
		Character:  req.Character,
		Symbol:     req.Symbol,
		Occurrence: req.Occurrence,
		Query:      req.Query,
		Limit:      req.Limit,
	})
	if err != nil {
		return nil, err
	}
	return lspView(res), nil
}

func lspView(res *lsp.Result) *api.LSPResponse {
	out := &api.LSPResponse{
		Operation:   res.Operation,
		Path:        res.Path,
		Language:    res.Language,
		Position:    res.Position,
		Hover:       res.Hover,
		Truncated:   res.Truncated,
		Note:        res.Note,
		ServerState: res.ServerState,
	}
	for _, r := range res.Refs {
		out.Refs = append(out.Refs, api.LSPRef{Path: r.Path, Line: r.Line, Character: r.Character, Text: r.Text})
	}
	for _, s := range res.Symbols {
		out.Symbols = append(out.Symbols, api.LSPSymbol{
			Name: s.Name, Kind: s.Kind, Detail: s.Detail,
			Path: s.Path, Line: s.Line, Character: s.Character, Depth: s.Depth,
		})
	}
	for _, p := range res.Problems {
		out.Problems = append(out.Problems, api.LSPProblem{
			Path: p.Path, Line: p.Line, Character: p.Character,
			Severity: p.Severity, Source: p.Source, Code: p.Code, Message: p.Message,
		})
	}
	for _, c := range res.Calls {
		out.Calls = append(out.Calls, api.LSPCall{Name: c.Name, Kind: c.Kind, Path: c.Path, Line: c.Line, Sites: c.Sites})
	}
	for _, s := range res.Servers {
		out.Servers = append(out.Servers, api.LSPServer{
			Key: s.Key, Language: s.Language, State: s.State, Repo: s.Repo, Root: s.Root,
			Bin: s.Bin, Source: s.Source, Version: s.Version,
			UptimeSec: int64(s.Uptime.Seconds()), IdleSec: int64(s.IdleFor.Seconds()),
			Requests: s.Requests, Detail: s.Detail,
		})
	}
	return out
}

// lspErrStatus maps a jail violation to 403, the way the file tools do.
func lspErrStatus(err error) int {
	if isForbidden(err) {
		return http.StatusForbidden
	}
	return http.StatusBadRequest
}
