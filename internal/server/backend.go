package server

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/notshekhar/drover/internal/api"
	"github.com/notshekhar/drover/internal/files"
	"github.com/notshekhar/drover/internal/httpreq"
	"github.com/notshekhar/drover/internal/object"
	"github.com/notshekhar/drover/internal/store"
)

// backend implements mcp.Backend directly against this server's own store and
// executors.
//
// The alternative -- having the /mcp endpoint call the engine's own HTTP API
// over loopback -- would work, but it makes every tool call a second network
// hop through the process's own listener, and breaks if the listener moved
// because its port was taken.
type backend struct{ s *Server }

// Backend returns an mcp.Backend over this server. It is exported so the MCP
// endpoint and any in-process caller share one implementation.
func (s *Server) Backend() *backend { return &backend{s: s} }

func (b *backend) List(kind object.Kind) ([]api.ObjectView, error) {
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

func (b *backend) Get(kind object.Kind, name string) (*api.ObjectView, error) {
	o, err := b.s.store.Get(kind, name)
	if err != nil {
		return nil, err
	}
	v := b.s.viewWithStatus(o)
	return &v, nil
}

func (b *backend) Ls(req api.LsRequest) (*api.LsResponse, error) {
	res, err := b.s.files.List(req.Path)
	if err != nil {
		return nil, err
	}
	out := &api.LsResponse{Path: res.Path, Entries: []api.FileEntry{}, Truncated: res.Truncated}
	for _, e := range res.Entries {
		out.Entries = append(out.Entries, api.FileEntry{Name: e.Name, Path: e.Path, Type: e.Type, Size: e.Size})
	}
	return out, nil
}

func (b *backend) ReadFile(req api.ReadRequest) (*api.ReadResponse, error) {
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

func (b *backend) Grep(req api.GrepRequest) (*api.GrepResponse, error) {
	res, err := b.s.files.Grep(req.Pattern, files.GrepOptions{
		Path:          req.Path,
		Include:       req.Include,
		CaseSensitive: req.CaseSensitive,
		MaxResults:    req.MaxResults,
	})
	if err != nil {
		return nil, err
	}
	out := &api.GrepResponse{Matches: []api.GrepMatch{}, Files: res.Files, Truncated: res.Truncated}
	for _, m := range res.Matches {
		out.Matches = append(out.Matches, api.GrepMatch{Path: m.Path, Line: m.Line, Text: m.Text})
	}
	return out, nil
}

func (b *backend) Find(req api.FindRequest) (*api.FindResponse, error) {
	res, err := b.s.files.Find(req.Pattern, req.Path, req.MaxResults)
	if err != nil {
		return nil, err
	}
	return &api.FindResponse{Paths: res.Paths, Truncated: res.Truncated}, nil
}

func (b *backend) Call(name string, req api.CallRequest) (*api.CallResponse, error) {
	return b.s.callRequest(context.Background(), name, req)
}

func (b *backend) Query(name, query string) (*api.QueryResponse, error) {
	spec, err := b.s.sqlSpecFor(name)
	if err != nil {
		return nil, err
	}
	res, err := b.s.sql.Query(context.Background(), name, spec, query)
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
