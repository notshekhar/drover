// Package server is the drover engine: it owns the object store and serves
// the apply API.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/notshekhar/drover/internal/api"
	"github.com/notshekhar/drover/internal/config"
	"github.com/notshekhar/drover/internal/docs"
	"github.com/notshekhar/drover/internal/files"
	"github.com/notshekhar/drover/internal/git"
	"github.com/notshekhar/drover/internal/httpreq"
	"github.com/notshekhar/drover/internal/lsp"
	"github.com/notshekhar/drover/internal/mcp"
	"github.com/notshekhar/drover/internal/object"
	"github.com/notshekhar/drover/internal/repo"
	"github.com/notshekhar/drover/internal/sqldb"
	"github.com/notshekhar/drover/internal/store"
	syncmgr "github.com/notshekhar/drover/internal/sync"
)

// Options configure a server.
type Options struct {
	DataDir string
	Listen  string
	Version string
	Log     io.Writer

	// Sync is the server-wide default refresh interval, used by repositories
	// that do not set their own. Zero turns the ticker off.
	Sync time.Duration

	// NoSync builds a server that never shells out to git. Tests that only
	// care about the object API use it so they do not need a network.
	NoSync bool

	// ConfigPath is the file a reload re-reads. Empty means reload is refused,
	// since there would be nothing to re-read.
	ConfigPath string

	// ServersDir is where language servers are installed. Empty means
	// ~/.drover/servers.
	ServersDir string

	// NoServerInstall leaves drover to whatever language servers are already
	// on the machine, rather than fetching one on first use.
	//
	// The tool is offered either way. This gates the network, not the feature:
	// downloading 50MB the first time somebody asks about a Java file is the
	// surprising part, not the asking.
	NoServerInstall bool
}

// Server is the engine.
type Server struct {
	opts  Options
	store *store.Store
	repo  *repo.Reconciler
	sync  *syncmgr.Manager
	http  *httpreq.Executor
	sql   *sqldb.Pool
	files *files.Root
	git   *git.Repos
	lsp   *lsp.Manager
	mux   *http.ServeMux

	started time.Time

	// httpSrv is kept so shutdown can drain in-flight requests instead of
	// closing the listener out from under them.
	mu      sync.Mutex
	httpSrv *http.Server
}

// New builds a server over the data directory.
func New(opts Options) (*Server, error) {
	if opts.Log == nil {
		opts.Log = io.Discard
	}
	st, err := store.New(opts.DataDir)
	if err != nil {
		return nil, err
	}
	s := &Server{
		opts:  opts,
		store: st,
		repo:  repo.New(opts.DataDir),
		http:  httpreq.New(),
		sql:   sqldb.NewPool(),
		files: files.New(opts.DataDir),
		git:   git.New(opts.DataDir),

		started: time.Now(),
	}
	// Language servers are wired up unconditionally, like the file tools they
	// sit beside. Nothing is launched here: a server starts the first time
	// somebody asks a question it can answer, and is reaped when nobody has
	// asked for a while, so an engine nobody queries pays nothing for it.
	serversDir := opts.ServersDir
	if serversDir == "" {
		serversDir = lsp.DefaultDir()
	}
	acquirer := lsp.NewAcquirer(serversDir)
	acquirer.NoInstall = opts.NoServerInstall || os.Getenv("DROVER_NO_SERVER_INSTALL") != ""
	s.lsp = lsp.NewManager(s.files, acquirer)
	s.lsp.Start()

	if !opts.NoSync {
		s.sync = syncmgr.New(syncmgr.Options{
			Store:   st,
			Repo:    s.repo,
			Default: opts.Sync,
			Log:     opts.Log,
			// Reconcile resets the working tree, so a server that has already
			// parsed it is now answering about a tree that no longer exists.
			OnCommitChanged: s.restartLanguageServers,
		})
	}
	s.routes()
	return s, nil
}

// restartLanguageServers drops the servers for a repository whose checkout
// just moved. Safe to call when they are turned off.
func (s *Server) restartLanguageServers(repository string) {
	if s.lsp == nil {
		return
	}
	s.logf("repository %s: restarting its language servers", repository)
	s.lsp.Restart(repository)
}

// Store exposes the object store, for tests and for the CLI's local paths.
func (s *Server) Store() *store.Store { return s.store }

// Handler is the HTTP handler, exported so tests can drive it without a
// listener.
func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) logf(format string, args ...any) {
	fmt.Fprintf(s.opts.Log, format+"\n", args...)
}

// MCPPath is where the engine serves the Model Context Protocol.
const MCPPath = "/mcp"

func (s *Server) routes() {
	s.mux = http.NewServeMux()

	// MCP on the engine itself, so an agent can point at a running
	// `drover serve` rather than spawning a second process. The handler is
	// wired to an in-process backend, so a tool call does not travel back
	// through this listener.
	mcpHandler := (&mcp.Server{Backend: s.Backend(), Version: s.opts.Version}).HTTPHandler()
	s.mux.Handle(MCPPath, mcpHandler)
	s.mux.Handle(MCPPath+"/", mcpHandler)
	s.mux.HandleFunc("POST "+api.Prefix+"/apply", s.handleApply)
	s.mux.HandleFunc("GET "+api.Prefix+"/status", s.handleStatus)
	s.mux.HandleFunc("POST "+api.Prefix+"/repositories/{name}/sync", s.handleSyncOne)
	s.mux.HandleFunc("POST "+api.Prefix+"/sync", s.handleSyncAll)
	s.mux.HandleFunc("GET "+api.Prefix+"/dashboard", s.handleDashboard)
	s.mux.HandleFunc("POST "+api.Prefix+"/reload", s.handleReload)
	s.mux.HandleFunc("POST "+api.Prefix+"/files/ls", s.handleLs)
	s.mux.HandleFunc("POST "+api.Prefix+"/files/read", s.handleRead)
	s.mux.HandleFunc("POST "+api.Prefix+"/files/grep", s.handleGrep)
	s.mux.HandleFunc("POST "+api.Prefix+"/files/find", s.handleFind)
	s.mux.HandleFunc("POST "+api.Prefix+"/git", s.handleGit)
	s.mux.HandleFunc("POST "+api.Prefix+"/lsp", s.handleLSP)
	s.mux.HandleFunc("POST "+api.Prefix+"/httprequests/{name}/call", s.handleCall)
	s.mux.HandleFunc("POST "+api.Prefix+"/sqlconnections/{name}/query", s.handleQuery)
	s.mux.HandleFunc("POST "+api.Prefix+"/sqlconnections/{name}/health", s.handleHealth)

	for _, k := range object.Kinds {
		kind := k
		p := api.Prefix + "/" + kind.Plural()
		s.mux.HandleFunc("GET "+p, func(w http.ResponseWriter, r *http.Request) { s.handleList(w, r, kind) })
		s.mux.HandleFunc("GET "+p+"/{name}", func(w http.ResponseWriter, r *http.Request) { s.handleGet(w, r, kind) })
		s.mux.HandleFunc("PUT "+p+"/{name}", func(w http.ResponseWriter, r *http.Request) { s.handlePut(w, r, kind) })
		s.mux.HandleFunc("DELETE "+p+"/{name}", func(w http.ResponseWriter, r *http.Request) { s.handleDelete(w, r, kind) })
	}
}

// Bootstrap loads what is already on disk, then applies the config's apply:
// paths. It runs before the listener opens, so the engine never serves a
// half-loaded view of the world.
func (s *Server) Bootstrap(cfg *config.Config) error {
	existing, err := s.store.ListAll()
	if err != nil {
		return fmt.Errorf("read stored objects: %w", err)
	}
	s.logf("loaded %d object(s) from %s", len(existing), s.store.Dir())

	// Write the reference before anything else, so a first run leaves a
	// data directory that explains itself.
	if written, err := docs.Ensure(s.opts.DataDir); err != nil {
		s.logf("could not write %s: %v", docs.Path(s.opts.DataDir), err)
	} else if written {
		s.logf("wrote %s — point an agent at it to add repositories, APIs and databases", docs.Path(s.opts.DataDir))
	}

	files, err := s.sourceFiles(cfg)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return nil
	}

	batch, err := s.readBatch(files)
	if err != nil {
		return fmt.Errorf("apply: %w", err)
	}
	if err := batch.Check(s.storedRefs()); err != nil {
		return fmt.Errorf("apply: %w", err)
	}
	for _, warning := range batch.Warnings() {
		s.logf("warning: %s", warning)
	}
	for _, o := range batch.Objects {
		if err := s.store.Put(o); err != nil {
			return fmt.Errorf("apply: %w", err)
		}
	}
	s.logf("applied %d object(s) from %d file(s)", batch.Len(), len(files))
	return nil
}

// StartSync brings up the per-repository refresh workers. It runs after
// Bootstrap and before Serve; reconciles happen in the background so a slow
// first clone does not hold the listener shut.
func (s *Server) StartSync(ctx context.Context) error {
	if s.sync == nil {
		return nil
	}
	if err := repo.Available(""); err != nil {
		return err
	}
	return s.sync.Start(ctx)
}

// StopSync shuts the workers down and waits for them.
func (s *Server) StopSync() {
	if s.sync != nil {
		s.sync.StopAll()
	}
	if s.sql != nil {
		s.sql.Close()
	}
	if s.lsp != nil {
		// A language server that outlives the engine is a gigabyte of resident
		// memory with nobody left to own it.
		s.lsp.Close()
	}
}

// CheckSQLHealth runs the health gate over every SQLConnection.
//
// A connection with no health query, or one that fails, is recorded as such
// and no sql tool will be offered for it. This never fails startup: a
// database being down is a fact to report, not a reason to refuse to serve
// the repositories that are fine.
func (s *Server) CheckSQLHealth(ctx context.Context) {
	objs, err := s.store.List(object.KindSQLConnection)
	if err != nil {
		return
	}
	for _, o := range objs {
		spec, err := o.SQLConnection()
		if err != nil {
			continue
		}
		name := o.Metadata.Name
		if strings.TrimSpace(spec.Health) == "" {
			_ = s.store.SetStatus(object.KindSQLConnection, name, &store.Status{
				Phase: store.PhasePending,
				Error: "no spec.health query, so no sql tool is offered",
			})
			s.logf("sqlconnection %s: no health query, no sql tool offered", name)
			continue
		}
		if err := s.checkSQLHealth(ctx, name, spec); err != nil {
			s.logf("sqlconnection %s: %v", name, err)
			continue
		}
		s.logf("sqlconnection %s: healthy", name)
	}
}

// sourceFiles is everything to apply at startup: the yaml dropped in the data
// directory, then the paths the config lists.
//
// Drop-ins come first so that a file someone (or their agent) put in
// ~/.drover is applied even on a machine whose config has no apply: list at
// all, which is the state after a fresh install.
func (s *Server) sourceFiles(cfg *config.Config) ([]string, error) {
	dropIns, err := config.DropInFiles(s.opts.DataDir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", s.opts.DataDir, err)
	}

	var configured []string
	if len(cfg.Apply) > 0 {
		configured, err = config.CollectAll(cfg.Apply)
		if err != nil {
			return nil, fmt.Errorf("apply: %w (fix the path, or drop it with `drover forget <path>`)", err)
		}
	}

	// A file reached both ways is one file; applying it twice would trip the
	// duplicate-name check against itself.
	seen := map[string]bool{}
	var out []string
	for _, f := range append(dropIns, configured...) {
		key := f
		if resolved, err := filepath.EvalSymlinks(f); err == nil {
			key = resolved
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, f)
	}
	return out, nil
}

// readBatch parses every file into one batch. Bootstrap and the dashboard's
// reload share it, so a reload validates exactly as a startup apply does.
func (s *Server) readBatch(files []string) (*object.Batch, error) {
	batch := object.NewBatch()
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		objs, err := object.Parse(f, data)
		if err != nil {
			return nil, err
		}
		if err := batch.AddAll(objs); err != nil {
			return nil, err
		}
	}
	return batch, nil
}

// storedRefs is what the store already holds, so a batch may reference an
// object applied earlier rather than only its own documents.
func (s *Server) storedRefs() map[object.Ref]bool {
	out := map[object.Ref]bool{}
	objs, err := s.store.ListAll()
	if err != nil {
		return out
	}
	for _, o := range objs {
		out[o.Ref()] = true
	}
	return out
}

// Listen opens the listener, walking forward if the port is taken so a second
// engine does not die on a port collision.
func (s *Server) Listen() (net.Listener, error) {
	host, portStr, err := net.SplitHostPort(s.opts.Listen)
	if err != nil {
		return nil, fmt.Errorf("listen %q: %w", s.opts.Listen, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, fmt.Errorf("listen %q: port is not a number", s.opts.Listen)
	}

	const tries = 20
	for i := 0; i < tries; i++ {
		addr := net.JoinHostPort(host, strconv.Itoa(port+i))
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			return ln, nil
		}
		if !isAddrInUse(err) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("no free port in %d..%d on %s", port, port+tries-1, host)
}

func isAddrInUse(err error) bool {
	return errors.Is(err, syscallEADDRINUSE) || strings.Contains(err.Error(), "address already in use")
}

// Serve runs until the server is shut down. It returns nil on a graceful
// stop, matching http.Server's own convention of reporting that as
// ErrServerClosed rather than a failure.
func (s *Server) Serve(ln net.Listener) error {
	srv := &http.Server{
		Handler:           s.mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	s.mu.Lock()
	s.httpSrv = srv
	s.mu.Unlock()

	err := srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown stops accepting connections and waits for in-flight requests to
// finish, then releases everything the engine holds.
//
// The wait matters: a tool call can be a large grep or an HTTP request to a
// slow API, and closing the listener under it would hand the agent a broken
// pipe instead of an answer. If the context expires first the remaining
// connections are closed anyway, because a shutdown that can be blocked
// forever by one hung request is not a shutdown.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	srv := s.httpSrv
	s.mu.Unlock()

	var err error
	if srv != nil {
		err = srv.Shutdown(ctx)
		if errors.Is(err, context.DeadlineExceeded) {
			s.logf("some requests did not finish in time; closing them")
			_ = srv.Close()
		}
	}

	// Workers and database connections come after the listener, so nothing
	// new can arrive while they are being torn down.
	s.StopSync()
	return err
}

// --- handlers ---

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, api.Error{Message: err.Error()})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	objs, err := s.store.ListAll()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, api.StatusResponse{
		Version: s.opts.Version,
		DataDir: s.opts.DataDir,
		Objects: len(objs),
	})
}

// handleApply validates the whole batch before writing any of it. A bad
// document in the third file must not leave the first two applied.
func (s *Server) handleApply(w http.ResponseWriter, r *http.Request) {
	var req api.ApplyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("malformed request: %w", err))
		return
	}
	if len(req.Documents) == 0 {
		writeErr(w, http.StatusBadRequest, errors.New("no documents"))
		return
	}

	batch := object.NewBatch()
	for _, doc := range req.Documents {
		objs, err := object.Parse(doc.Source, []byte(doc.Data))
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		if err := batch.AddAll(objs); err != nil {
			writeErr(w, http.StatusConflict, err)
			return
		}
	}
	if err := batch.Check(s.storedRefs()); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	// Work out created-vs-updated before writing, while the store still holds
	// the previous state.
	resp := api.ApplyResponse{Warnings: batch.Warnings()}
	actions := make([]api.Action, batch.Len())
	for i, o := range batch.Objects {
		if _, err := s.store.Get(o.Kind, o.Metadata.Name); err == nil {
			actions[i] = api.ActionUpdated
		} else if errors.Is(err, store.ErrNotFound) {
			actions[i] = api.ActionCreated
		} else {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
	}

	for i, o := range batch.Objects {
		if err := s.store.Put(o); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		resp.Results = append(resp.Results, api.Result{
			Kind:   string(o.Kind),
			Name:   o.Metadata.Name,
			Action: actions[i],
		})
		// Reconcile now and pick up any change to refreshInterval, rather
		// than waiting out the old cadence.
		if s.sync != nil {
			if err := s.sync.Ensure(o); err != nil {
				s.logf("repository %s: %v", o.Metadata.Name, err)
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handlePut(w http.ResponseWriter, r *http.Request, kind object.Kind) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	name := r.PathValue("name")
	source := r.Header.Get("X-Drover-Source")

	objs, err := object.Parse(source, body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if len(objs) != 1 {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("expected one document, got %d", len(objs)))
		return
	}
	o := objs[0]
	if o.Kind != kind {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("document is %s but the route is %s", o.Kind, kind))
		return
	}
	if o.Metadata.Name != name {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("document is named %q but the route says %q", o.Metadata.Name, name))
		return
	}
	if !kind.Implemented() {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("%s is not supported yet", kind))
		return
	}
	if err := s.store.Put(o); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, s.viewWithStatus(o))
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request, kind object.Kind) {
	o, err := s.store.Get(kind, r.PathValue("name"))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, s.viewWithStatus(o))
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request, kind object.Kind) {
	objs, err := s.store.List(kind)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	resp := api.ListResponse{Items: []api.ObjectView{}}
	for _, o := range objs {
		resp.Items = append(resp.Items, s.viewWithStatus(o))
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request, kind object.Kind) {
	name := r.PathValue("name")
	err := s.store.Delete(kind, name)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	// The clone is the object's; removing one without the other would leave a
	// checkout nothing explains.
	if kind == object.KindRepository {
		if s.sync != nil {
			s.sync.Stop(name)
		}
		if err := s.repo.Remove(name); err != nil {
			writeErr(w, http.StatusInternalServerError, fmt.Errorf("object deleted but its checkout remains at %s: %w", s.repo.Path(name), err))
			return
		}
	}
	_ = s.store.DeleteStatus(kind, name)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSyncOne(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, err := s.store.Get(object.KindRepository, name); err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if s.sync == nil {
		writeErr(w, http.StatusBadRequest, errors.New("this engine was started without sync"))
		return
	}
	if err := s.sync.SyncNow(name); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleSyncAll(w http.ResponseWriter, r *http.Request) {
	if s.sync == nil {
		writeErr(w, http.StatusBadRequest, errors.New("this engine was started without sync"))
		return
	}
	s.sync.SyncAll()
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.DashboardState(s.opts.Listen, s.started))
}

// handleReload re-reads the config and applies it, for `drover dash` and for
// anyone who would rather curl it than press a key.
func (s *Server) handleReload(w http.ResponseWriter, r *http.Request) {
	if s.opts.ConfigPath == "" {
		writeErr(w, http.StatusBadRequest, errors.New("this engine was started without a config file"))
		return
	}
	msg, _, err := s.Reload(s.opts.ConfigPath)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, api.ReloadResponse{Message: msg})
}

// --- file tools ---
//
// These live on the engine rather than in the MCP process because the engine
// owns the checkouts. Keeping them here is what lets `drover mcp` be a thin
// bridge and keeps "the engine can run anywhere" true.

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("malformed request: %w", err))
		return false
	}
	return true
}

// fileErrStatus maps a jail violation to 403 rather than 400, so a caller can
// tell "you may not look there" from "you typed it wrong".
func fileErrStatus(err error) int {
	if isForbidden(err) {
		return http.StatusForbidden
	}
	return http.StatusBadRequest
}

func (s *Server) handleLs(w http.ResponseWriter, r *http.Request) {
	var req api.LsRequest
	if !decodeBody(w, r, &req) {
		return
	}
	res, err := s.files.List(req.Path)
	if err != nil {
		writeErr(w, fileErrStatus(err), err)
		return
	}
	out := api.LsResponse{Path: res.Path, Entries: []api.FileEntry{}, Truncated: res.Truncated}
	for _, e := range res.Entries {
		out.Entries = append(out.Entries, api.FileEntry{Name: e.Name, Path: e.Path, Type: e.Type, Size: e.Size})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleRead(w http.ResponseWriter, r *http.Request) {
	var req api.ReadRequest
	if !decodeBody(w, r, &req) {
		return
	}
	res, err := s.files.Read(req.Path, req.Offset, req.Limit)
	if err != nil {
		writeErr(w, fileErrStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, api.ReadResponse{
		Path:       res.Path,
		Content:    res.Content,
		StartLine:  res.StartLine,
		EndLine:    res.EndLine,
		TotalLines: res.TotalLines,
		Truncated:  res.Truncated,
	})
}

func (s *Server) handleGrep(w http.ResponseWriter, r *http.Request) {
	var req api.GrepRequest
	if !decodeBody(w, r, &req) {
		return
	}
	res, err := s.files.Grep(req.Pattern, files.GrepOptions{
		Path:          req.Path,
		Include:       req.Include,
		CaseSensitive: req.CaseSensitive,
		MaxResults:    req.MaxResults,
	})
	if err != nil {
		writeErr(w, fileErrStatus(err), err)
		return
	}
	out := api.GrepResponse{Matches: []api.GrepMatch{}, Files: res.Files, Truncated: res.Truncated}
	for _, m := range res.Matches {
		out.Matches = append(out.Matches, api.GrepMatch{Path: m.Path, Line: m.Line, Text: m.Text})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleFind(w http.ResponseWriter, r *http.Request) {
	var req api.FindRequest
	if !decodeBody(w, r, &req) {
		return
	}
	res, err := s.files.Find(req.Pattern, req.Path, req.MaxResults)
	if err != nil {
		writeErr(w, fileErrStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, api.FindResponse{Paths: res.Paths, Truncated: res.Truncated})
}

// handleGit answers one history question about a checkout.
func (s *Server) handleGit(w http.ResponseWriter, r *http.Request) {
	var req api.GitRequest
	if !decodeBody(w, r, &req) {
		return
	}
	res, err := s.gitQuery(r.Context(), req)
	if err != nil {
		writeErr(w, gitErrStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleLSP answers one navigation question.
func (s *Server) handleLSP(w http.ResponseWriter, r *http.Request) {
	var req api.LSPRequest
	if !decodeBody(w, r, &req) {
		return
	}
	res, err := s.lspQuery(r.Context(), req)
	if err != nil {
		writeErr(w, lspErrStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleCall executes one HTTPRequest.
func (s *Server) handleCall(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	var req api.CallRequest
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("malformed request: %w", err))
			return
		}
	}

	resp, err := s.callRequest(r.Context(), name, req)
	if err != nil {
		writeErr(w, callErrStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// callErrStatus separates "no such object" from "your call was wrong".
func callErrStatus(err error) int {
	if isNotFound(err) {
		return http.StatusNotFound
	}
	return http.StatusBadRequest
}

// handleQuery runs one statement against a SQLConnection.
func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	spec, ok := s.sqlSpec(w, name)
	if !ok {
		return
	}

	var req api.QueryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("malformed request: %w", err))
		return
	}
	if strings.TrimSpace(req.Query) == "" {
		writeErr(w, http.StatusBadRequest, errors.New("no query"))
		return
	}

	res, err := s.sql.Query(r.Context(), name, spec, req.Query)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, api.QueryResponse{
		Columns:   res.Columns,
		Rows:      res.Rows,
		RowCount:  res.RowCount,
		Truncated: res.Truncated,
		Provider:  res.Provider,
		ElapsedMS: res.Elapsed,
	})
}

// handleHealth re-runs the health gate on demand.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	spec, ok := s.sqlSpec(w, name)
	if !ok {
		return
	}
	if err := s.checkSQLHealth(r.Context(), name, spec); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) sqlSpec(w http.ResponseWriter, name string) (*object.SQLConnectionSpec, bool) {
	spec, err := s.sqlSpecFor(name)
	if err != nil {
		if isNotFound(err) {
			writeErr(w, http.StatusNotFound, err)
			return nil, false
		}
		writeErr(w, http.StatusInternalServerError, err)
		return nil, false
	}
	return spec, true
}

// checkSQLHealth runs the gate and records the outcome, so `get
// sqlconnection` can say whether a tool would be offered.
func (s *Server) checkSQLHealth(ctx context.Context, name string, spec *object.SQLConnectionSpec) error {
	err := s.sql.HealthCheck(ctx, name, spec)
	if err != nil {
		_ = s.store.MarkFailed(object.KindSQLConnection, name, err)
		return err
	}
	_ = s.store.SetStatus(object.KindSQLConnection, name, &store.Status{
		Phase:       store.PhaseReady,
		LastAttempt: time.Now().UTC().Format(time.RFC3339),
		LastSuccess: time.Now().UTC().Format(time.RFC3339),
	})
	return nil
}

// viewWithStatus adds observed state, so `get repository` can answer whether
// the checkout is actually there rather than only echoing desired state.
func (s *Server) viewWithStatus(o *object.Object) api.ObjectView {
	v := view(o)
	if st, err := s.store.GetStatus(o.Kind, o.Metadata.Name); err == nil {
		v.Status = string(st.Phase)
		v.Error = st.Error
		v.Commit = st.Commit
		v.LastSync = st.LastSuccess
	}
	return v
}

// view flattens an object into the shape the client prints.
func view(o *object.Object) api.ObjectView {
	v := api.ObjectView{
		Kind:      string(o.Kind),
		Name:      o.Metadata.Name,
		Source:    o.Metadata.Source,
		AppliedAt: o.Metadata.AppliedAt,
	}
	if data, err := yaml.Marshal(o); err == nil {
		v.YAML = string(data)
	}
	switch o.Kind {
	case object.KindRepository:
		if spec, err := o.Repository(); err == nil {
			v.URL = spec.URL
			v.Branch = spec.Branch
			v.RefreshInterval = spec.RefreshInterval.String()
		}

	case object.KindEnvironment:
		if spec, err := o.Environment(); err == nil {
			for _, name := range sortedVarNames(spec) {
				v.Variables = append(v.Variables, name)
			}
			// Secret values never leave the engine -- only which process
			// variable backs each one, and whether it is set.
			for _, st := range spec.SecretStatuses(nil) {
				v.Secrets = append(v.Secrets, api.SecretStatus{Name: st.Name, FromEnv: st.FromEnv, Set: st.Set})
			}
		}

	case object.KindHTTPRequest:
		if spec, err := o.HTTPRequest(); err == nil {
			v.Method = spec.NormalizedMethod()
			v.URL = spec.URL
			v.Environments = spec.Environments
			v.DefaultEnvironment = spec.DefaultEnvironment
			v.Safe = spec.IsSafe()
			v.Params = spec.RequiredParams()
		}

	case object.KindSQLConnection:
		if spec, err := o.SQLConnection(); err == nil {
			if p, err := spec.ResolveProvider(); err == nil {
				v.Provider = string(p)
			}
			v.ReadOnly = spec.IsReadOnly()
			v.MaxRows = spec.RowLimit()
		}
	}
	return v
}

func sortedVarNames(spec *object.EnvironmentSpec) []string {
	out := make([]string, 0, len(spec.Variables))
	for k := range spec.Variables {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
