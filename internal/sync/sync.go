// Package sync keeps checkouts fresh.
//
// Every repository gets its own goroutine and its own timer, because the
// refresh interval is per object: a monorepo someone is working in can pull
// every few minutes while a vendored reference repo sits at a day. It also
// means one slow or failing fetch cannot delay anybody else's tick, which a
// single shared loop could not promise.
package sync

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/notshekhar/drover/internal/object"
	"github.com/notshekhar/drover/internal/repo"
	"github.com/notshekhar/drover/internal/store"
)

// Manager owns one worker per repository.
type Manager struct {
	store   *store.Store
	rec     *repo.Reconciler
	def     time.Duration
	log     io.Writer
	baseCtx context.Context

	mu      sync.Mutex
	workers map[string]*worker
	started bool
}

type worker struct {
	name     string
	interval time.Duration
	ticking  bool
	cancel   context.CancelFunc
	trigger  chan struct{}
	done     chan struct{}
}

// Options configure a Manager.
type Options struct {
	Store   *store.Store
	Repo    *repo.Reconciler
	Default time.Duration // server-wide default for objects that set none
	Log     io.Writer
}

// New builds a Manager.
func New(opts Options) *Manager {
	if opts.Log == nil {
		opts.Log = io.Discard
	}
	return &Manager{
		store:   opts.Store,
		rec:     opts.Repo,
		def:     opts.Default,
		log:     opts.Log,
		workers: map[string]*worker{},
	}
}

func (m *Manager) logf(format string, args ...any) {
	fmt.Fprintf(m.log, format+"\n", args...)
}

// Start brings up a worker for every stored repository. Reconciles run in the
// background so a slow clone does not hold the listener shut.
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	m.baseCtx = ctx
	m.started = true
	m.mu.Unlock()

	objs, err := m.store.List(object.KindRepository)
	if err != nil {
		return err
	}
	for _, o := range objs {
		if err := m.Ensure(o); err != nil {
			return err
		}
	}
	return nil
}

// Ensure starts or reschedules the worker for one repository.
//
// Changing refreshInterval reschedules immediately rather than waiting out
// the old interval, which is what someone dropping 24h to 5m expects.
func (m *Manager) Ensure(o *object.Object) error {
	if o.Kind != object.KindRepository {
		return nil
	}
	spec, err := o.Repository()
	if err != nil {
		return err
	}
	interval, ticking := spec.RefreshInterval.Resolve(m.def)

	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.started {
		return nil
	}

	name := o.Metadata.Name
	if w, ok := m.workers[name]; ok {
		if w.interval == interval && w.ticking == ticking {
			// Same cadence: just reconcile now, since the spec may have
			// changed url or branch.
			m.kick(w)
			return nil
		}
		m.stopLocked(name)
	}

	ctx, cancel := context.WithCancel(m.baseCtx)
	w := &worker{
		name:     name,
		interval: interval,
		ticking:  ticking,
		cancel:   cancel,
		trigger:  make(chan struct{}, 1),
		done:     make(chan struct{}),
	}
	m.workers[name] = w
	go m.run(ctx, w)

	// Reconcile as soon as it is applied, not at the first tick.
	m.kick(w)
	return nil
}

// kick asks a worker to reconcile now. The trigger channel is buffered to one
// because a second request while one is pending is the same request.
func (m *Manager) kick(w *worker) {
	select {
	case w.trigger <- struct{}{}:
	default:
	}
}

// SyncNow forces a reconcile of one repository.
func (m *Manager) SyncNow(name string) error {
	m.mu.Lock()
	w, ok := m.workers[name]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("no worker for %q", name)
	}
	m.kick(w)
	return nil
}

// SyncAll forces a reconcile of every repository.
func (m *Manager) SyncAll() {
	m.mu.Lock()
	workers := make([]*worker, 0, len(m.workers))
	for _, w := range m.workers {
		workers = append(workers, w)
	}
	m.mu.Unlock()
	for _, w := range workers {
		m.kick(w)
	}
}

// Stop shuts down one repository's worker, for when its object is deleted.
func (m *Manager) Stop(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopLocked(name)
}

func (m *Manager) stopLocked(name string) {
	w, ok := m.workers[name]
	if !ok {
		return
	}
	w.cancel()
	delete(m.workers, name)
}

// StopAll shuts every worker down and waits for them, so a test or a shutdown
// does not leave reconciles writing into a directory that is going away.
func (m *Manager) StopAll() {
	m.mu.Lock()
	workers := make([]*worker, 0, len(m.workers))
	for name, w := range m.workers {
		workers = append(workers, w)
		delete(m.workers, name)
	}
	m.mu.Unlock()

	for _, w := range workers {
		w.cancel()
	}
	for _, w := range workers {
		<-w.done
	}
}

// run is one repository's loop.
func (m *Manager) run(ctx context.Context, w *worker) {
	defer close(w.done)

	var tick <-chan time.Time
	if w.ticking && w.interval > 0 {
		t := time.NewTicker(w.interval)
		defer t.Stop()
		tick = t.C
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-w.trigger:
		case <-tick:
		}
		m.reconcile(ctx, w.name)
	}
}

// reconcile runs one attempt and records what happened. A failure is stored
// and retried on the next tick rather than killing the worker -- a remote
// that is down for an hour should not need a restart to recover.
func (m *Manager) reconcile(ctx context.Context, name string) {
	o, err := m.store.Get(object.KindRepository, name)
	if err != nil {
		// The object was deleted while this attempt was queued.
		return
	}
	spec, err := o.Repository()
	if err != nil {
		_ = m.store.MarkFailed(object.KindRepository, name, err)
		return
	}

	_ = m.store.MarkSyncing(object.KindRepository, name)
	res, err := m.rec.Reconcile(ctx, name, spec)
	if err != nil {
		if ctx.Err() != nil {
			return // shutting down; not a repository failure
		}
		m.logf("repository %s: %v", name, err)
		_ = m.store.MarkFailed(object.KindRepository, name, err)
		return
	}

	_ = m.store.MarkReady(object.KindRepository, name, res.Commit, res.Branch)
	switch {
	case res.Cloned:
		m.logf("repository %s: cloned %s at %s", name, spec.Branch, short(res.Commit))
	default:
		m.logf("repository %s: up to date at %s", name, short(res.Commit))
	}
}

func short(commit string) string {
	if len(commit) > 8 {
		return commit[:8]
	}
	return commit
}
