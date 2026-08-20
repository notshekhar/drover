package lsp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/notshekhar/drover/internal/files"
)

// Lifecycle defaults.
//
// drover is a daemon, which changes the arithmetic completely. In a CLI every
// session pays gopls's indexing again; here it is paid once and every agent
// that connects afterwards asks a warm server. The cost of that is resident
// memory -- gopls holds a gigabyte on a large repository -- so servers are
// reaped when nobody has asked them anything, and capped so a drover holding
// ten checkouts cannot hold ten servers at once.
const (
	DefaultIdleTTL    = 30 * time.Minute
	DefaultMaxServers = 6
	startTimeout      = 90 * time.Second
	openSettleDelay   = 150 * time.Millisecond
)

// Manager owns the language-server processes.
type Manager struct {
	Files    *files.Root
	Acquirer *Acquirer

	IdleTTL    time.Duration
	MaxServers int

	mu      sync.Mutex
	servers map[string]*entry
	failed  map[string]*failure
	stop    chan struct{}
	once    sync.Once
}

// entry is one running server.
type entry struct {
	client   *Client
	def      *Definition
	repo     string
	root     string // absolute
	resolved *Resolved
	started  time.Time
	lastUsed time.Time
	requests int

	// opened remembers which documents have been sent, so a repeat call does
	// not re-read and re-send a file the server already has.
	opened map[string]bool
}

// failure remembers why a server would not start.
//
// Without it, every tool call retries a ninety-second spawn that is going to
// fail the same way, and the agent waits ninety seconds each time to be told
// the same thing.
type failure struct {
	err  error
	when time.Time
}

const failureTTL = 5 * time.Minute

// NewManager builds a manager over the checkouts.
func NewManager(root *files.Root, acquirer *Acquirer) *Manager {
	return &Manager{
		Files:      root,
		Acquirer:   acquirer,
		IdleTTL:    DefaultIdleTTL,
		MaxServers: DefaultMaxServers,
		servers:    map[string]*entry{},
		failed:     map[string]*failure{},
		stop:       make(chan struct{}),
	}
}

// Start begins the idle reaper.
func (m *Manager) Start() {
	m.once.Do(func() { go m.reap() })
}

// Close shuts every server down.
//
// A language server that outlives the engine is a gigabyte of resident memory
// with nobody left to own it.
func (m *Manager) Close() {
	select {
	case <-m.stop:
	default:
		close(m.stop)
	}
	m.mu.Lock()
	entries := make([]*entry, 0, len(m.servers))
	for _, e := range m.servers {
		entries = append(entries, e)
	}
	m.servers = map[string]*entry{}
	m.mu.Unlock()

	for _, e := range entries {
		e.client.Close()
	}
}

// Target is a resolved place to ask a question about.
type Target struct {
	Repo string // repository name
	Rel  string // repo-prefixed path, as the file tools spell it
	Abs  string // absolute path on disk
}

// Resolve turns a repository-prefixed path into a target, through exactly the
// same jail the file tools use.
func (m *Manager) Resolve(path string) (*Target, error) {
	path = strings.TrimSpace(filepath.ToSlash(path))
	if path == "" {
		return nil, fmt.Errorf("a path is required, e.g. api/internal/server.go")
	}
	abs, err := m.Files.Resolve(path)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(abs); err != nil {
		return nil, fmt.Errorf("%s does not exist in the checkouts", path)
	}
	repo := path
	if i := strings.IndexByte(path, '/'); i > 0 {
		repo = path[:i]
	}
	return &Target{Repo: repo, Rel: m.Files.Display(abs), Abs: abs}, nil
}

// ClientFor returns a server that can answer about this file, starting one if
// necessary.
func (m *Manager) ClientFor(ctx context.Context, target *Target) (*Client, *Definition, error) {
	def := DefinitionFor(target.Abs)
	if def == nil {
		return nil, nil, fmt.Errorf("no language server for %s; drover speaks %s",
			filepath.Ext(target.Abs), strings.Join(Languages(), ", "))
	}
	root, err := m.projectRoot(def, target)
	if err != nil {
		return nil, nil, err
	}
	key := def.Key + "@" + root

	m.mu.Lock()
	if e, ok := m.servers[key]; ok {
		if e.client.Alive() {
			e.lastUsed = time.Now()
			e.requests++
			m.mu.Unlock()
			m.ensureOpen(e, target.Abs)
			return e.client, def, nil
		}
		// It died. Forget it and start again rather than answering from a
		// process that is not there.
		delete(m.servers, key)
	}
	if f, ok := m.failed[key]; ok && time.Since(f.when) < failureTTL {
		m.mu.Unlock()
		return nil, nil, f.err
	}
	m.mu.Unlock()

	client, err := m.start(ctx, def, target.Repo, root, key)
	if err != nil {
		m.mu.Lock()
		m.failed[key] = &failure{err: err, when: time.Now()}
		m.mu.Unlock()
		return nil, nil, err
	}
	m.mu.Lock()
	e := m.servers[key]
	m.mu.Unlock()
	if e != nil {
		m.ensureOpen(e, target.Abs)
	}
	return client, def, nil
}

func (m *Manager) start(ctx context.Context, def *Definition, repo, root, key string) (*Client, error) {
	resolved, err := m.Acquirer.Ensure(ctx, def)
	if err != nil {
		return nil, err
	}

	startCtx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()

	client, err := Start(startCtx, def.Key, root, resolved.Bin, resolved.Args, resolved.Env)
	if err != nil {
		return nil, err
	}
	if err := client.Initialize(startCtx, def.InitOptions); err != nil {
		client.Close()
		detail := ""
		if s := client.Stderr(); s != "" {
			detail = ": " + lastLine(s)
		}
		return nil, fmt.Errorf("the %s server failed to start%s", def.Language, detail)
	}

	now := time.Now()
	e := &entry{
		client: client, def: def, repo: repo, root: root, resolved: resolved,
		started: now, lastUsed: now, requests: 1, opened: map[string]bool{},
	}

	m.mu.Lock()
	m.servers[key] = e
	delete(m.failed, key)
	over := len(m.servers) - m.MaxServers
	m.mu.Unlock()

	if over > 0 {
		m.evict(over)
	}
	return client, nil
}

// ensureOpen sends a document the first time it is asked about.
//
// A server builds its view of a project from the documents it has been given.
// Asking about a file it has never seen is how a perfectly healthy server
// returns nothing at all.
func (m *Manager) ensureOpen(e *entry, abs string) {
	m.mu.Lock()
	already := e.opened[abs]
	if !already {
		e.opened[abs] = true
	}
	m.mu.Unlock()
	if already {
		return
	}
	if err := e.client.OpenDocument(abs); err != nil {
		return
	}
	// didOpen is a notification, so there is nothing to wait for. A short
	// settle beats the alternative, which is a first call that races the
	// server's own parse and comes back empty.
	time.Sleep(openSettleDelay)
}

// projectRoot finds the nearest ancestor holding a root marker, stopping at
// the repository. A monorepo therefore gets one server per package rather than
// one server holding all of them.
func (m *Manager) projectRoot(def *Definition, target *Target) (string, error) {
	repoRoot := filepath.Join(m.Files.Dir, target.Repo)
	dir := filepath.Dir(target.Abs)

	best := ""
	for {
		for _, marker := range def.DisqualifyMarkers {
			if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
				return "", fmt.Errorf("%s has a %s, so its TypeScript belongs to another toolchain; drover does not start a server for it", m.Files.Display(dir), marker)
			}
		}
		if best == "" {
			for _, marker := range def.RootMarkers {
				if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
					best = dir
					break
				}
			}
		}
		if dir == repoRoot || len(dir) <= len(repoRoot) {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	if best != "" {
		return best, nil
	}
	// No marker anywhere: the repository root is still a defensible answer,
	// and a server that indexes a bit too much beats no server at all.
	return repoRoot, nil
}

// --- lifecycle ---

// Restart stops every server for a repository.
//
// THE drover-SPECIFIC ONE. Reconcile does `git reset --hard`, rewriting the
// working tree under a server that has already parsed it. That server will
// then answer confidently and wrongly. workspace/didChangeWatchedFiles is the
// polite fix and is unevenly implemented; restarting is cheap, and the daemon
// is what makes it affordable.
func (m *Manager) Restart(repo string) {
	m.mu.Lock()
	var closing []*entry
	for key, e := range m.servers {
		if e.repo != repo {
			continue
		}
		closing = append(closing, e)
		delete(m.servers, key)
	}
	for key := range m.failed {
		// A failure may have been "this file does not exist"; a sync is a
		// reason to try again.
		if strings.Contains(key, "@"+filepath.Join(m.Files.Dir, repo)) {
			delete(m.failed, key)
		}
	}
	m.mu.Unlock()

	for _, e := range closing {
		go e.client.Close()
	}
}

func (m *Manager) reap() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-ticker.C:
			m.reapIdle()
		}
	}
}

func (m *Manager) reapIdle() {
	m.mu.Lock()
	var closing []*entry
	for key, e := range m.servers {
		if time.Since(e.lastUsed) > m.IdleTTL || !e.client.Alive() {
			closing = append(closing, e)
			delete(m.servers, key)
		}
	}
	m.mu.Unlock()
	for _, e := range closing {
		e.client.Close()
	}
}

// evict closes the least recently used servers when the cap is exceeded.
func (m *Manager) evict(n int) {
	m.mu.Lock()
	type aged struct {
		key string
		e   *entry
	}
	all := make([]aged, 0, len(m.servers))
	for key, e := range m.servers {
		all = append(all, aged{key, e})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].e.lastUsed.Before(all[j].e.lastUsed) })

	var closing []*entry
	for i := 0; i < n && i < len(all); i++ {
		closing = append(closing, all[i].e)
		delete(m.servers, all[i].key)
	}
	m.mu.Unlock()

	for _, e := range closing {
		go e.client.Close()
	}
}

// --- status ---

// ServerStatus is one server, running or merely possible.
type ServerStatus struct {
	Key      string
	Language string
	State    string // running, indexing, available, unavailable
	Repo     string
	Root     string
	Bin      string
	Source   string
	Version  string
	Uptime   time.Duration
	IdleFor  time.Duration
	Requests int
	Detail   string
}

// Status reports what is running and, for what is not, why not.
//
// The operation loop's LSP has no equivalent, and it matters: a server that
// silently failed to start is otherwise indistinguishable from a language
// nobody has asked about yet.
func (m *Manager) Status(ctx context.Context) []ServerStatus {
	m.mu.Lock()
	running := make([]ServerStatus, 0, len(m.servers))
	seen := map[string]bool{}
	for _, e := range m.servers {
		seen[e.def.Key] = true
		s := ServerStatus{
			Key: e.def.Key, Language: e.def.Language, State: "running",
			Repo: e.repo, Root: m.Files.Display(e.root), Bin: e.resolved.Bin,
			Source: e.resolved.Source, Version: e.resolved.Version,
			Uptime: time.Since(e.started), IdleFor: time.Since(e.lastUsed),
			Requests: e.requests,
		}
		if what := e.client.Indexing(); what != "" {
			s.State = "indexing"
			s.Detail = what
		}
		if !e.client.Alive() {
			s.State = "unavailable"
			s.Detail = "the process exited"
		}
		running = append(running, s)
	}
	failures := map[string]*failure{}
	for key, f := range m.failed {
		failures[key] = f
	}
	m.mu.Unlock()

	for _, def := range Definitions {
		if seen[def.Key] {
			continue
		}
		s := ServerStatus{Key: def.Key, Language: def.Language}

		// Asked in pieces rather than through Ensure, because "could this
		// start" and "start it" are different questions and answering the
		// first one must not pull 50MB down the wire.
		switch {
		case m.Acquirer.CheckRequirements(ctx, def) != nil:
			// A missing toolchain is not something drover can install its way
			// out of, so it must not promise to.
			s.State = "unavailable"
			s.Detail = m.Acquirer.CheckRequirements(ctx, def).Error()

		default:
			if resolved := m.Acquirer.Find(ctx, def); resolved != nil {
				s.State = "available"
				s.Bin, s.Source, s.Version = resolved.Bin, resolved.Source, resolved.Version
				s.Detail = "installed, not started yet"
			} else if def.NPM != nil || def.GoInstall != "" || def.Download != nil {
				s.State = "available"
				s.Detail = "not installed yet; drover fetches it the first time it is needed"
			} else {
				s.State = "unavailable"
				s.Detail = "no server is installed and drover has no way to install one"
			}
		}
		running = append(running, s)
	}

	for key, f := range failures {
		lang := key
		if i := strings.IndexByte(key, '@'); i > 0 {
			lang = key[:i]
		}
		running = append(running, ServerStatus{
			Key: lang, Language: lang, State: "unavailable",
			Root: key, Detail: f.err.Error(),
		})
	}

	sort.Slice(running, func(i, j int) bool {
		if running[i].Key != running[j].Key {
			return running[i].Key < running[j].Key
		}
		return running[i].Repo < running[j].Repo
	})
	return running
}
