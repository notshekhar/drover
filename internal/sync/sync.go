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

	"github.com/notshekhar/drover/internal/mirror"
	"github.com/notshekhar/drover/internal/object"
	"github.com/notshekhar/drover/internal/repo"
	"github.com/notshekhar/drover/internal/repoconfig"
	"github.com/notshekhar/drover/internal/store"
)

// Mirrorer keeps the discussion around a checkout beside it. It is an
// interface so the sync package does not depend on GitHub, and so a test can
// run the loop without one.
type Mirrorer interface {
	Sync(ctx context.Context, name, repoURL, checkout string, spec *object.MirrorSpec) (*mirror.Result, error)
}

// SelfDescriber applies the objects a repository declares about itself. It
// is an interface for the same reason Mirrorer is: the sync loop should not
// know how the store works.
type SelfDescriber interface {
	Apply(ctx context.Context, name string, spec *object.RepositorySpec) (string, error)
}

// Manager owns one worker per repository.
type Manager struct {
	store    *store.Store
	rec      *repo.Reconciler
	mirror   Mirrorer
	selfdesc SelfDescriber
	onChange func(string)
	def      time.Duration
	log      io.Writer
	baseCtx  context.Context

	mu      sync.Mutex
	workers map[string]*worker
	started bool

	// wg covers every worker ever started, including ones retired by a
	// reschedule. StopAll waits on it, so shutdown cannot leave a reconcile
	// writing into a directory that is going away.
	wg sync.WaitGroup
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

	// Mirror, when set, refreshes a repository's issues and pull requests
	// after its checkout reconciles.
	Mirror Mirrorer

	// SelfDescribe, when set, reads the checkout's own .drover.yaml.
	SelfDescribe SelfDescriber

	// OnCommitChanged is called after a reconcile that moved the checkout to a
	// different commit.
	//
	// It exists for the language servers: reconcile rewrites the working tree
	// with `git reset --hard`, and a server that has already parsed the old
	// tree will go on answering confidently and wrongly. Nothing else needs
	// to know, so this is a callback rather than an event bus.
	OnCommitChanged func(repository string)
}

// New builds a Manager.
func New(opts Options) *Manager {
	if opts.Log == nil {
		opts.Log = io.Discard
	}
	return &Manager{
		store:    opts.Store,
		rec:      opts.Repo,
		mirror:   opts.Mirror,
		selfdesc: opts.SelfDescribe,
		def:      opts.Default,
		log:      opts.Log,
		onChange: opts.OnCommitChanged,
		workers:  map[string]*worker{},
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
		m.retireLocked(name)
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
	m.wg.Add(1)
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

// Stop shuts down one repository's worker and waits for it to exit.
//
// The wait is the point. Delete calls this and then removes the checkout, so
// returning while a clone is still in flight would let the reconcile recreate
// the directory moments after it was deleted.
func (m *Manager) Stop(name string) {
	m.mu.Lock()
	w, ok := m.workers[name]
	if ok {
		delete(m.workers, name)
	}
	m.mu.Unlock()

	if !ok {
		return
	}
	// Cancel and wait off the lock: an in-flight fetch can take a while, and
	// holding the mutex through it would block every other operation.
	w.cancel()
	<-w.done
}

// retireLocked cancels a worker without waiting, for the reschedule path.
//
// A reschedule only needs the old cadence to stop; the replacement reconciles
// immediately anyway. Waiting here would hold the lock for the length of a
// clone. The retired goroutine is still tracked by the WaitGroup, so StopAll
// accounts for it.
func (m *Manager) retireLocked(name string) {
	w, ok := m.workers[name]
	if !ok {
		return
	}
	w.cancel()
	delete(m.workers, name)
}

// StopAll shuts every worker down and waits for them, so a shutdown does not
// leave reconciles writing into a directory that is going away.
func (m *Manager) StopAll() {
	m.mu.Lock()
	for name, w := range m.workers {
		w.cancel()
		delete(m.workers, name)
	}
	m.mu.Unlock()

	// Waits for retired workers too, not just the currently registered ones.
	m.wg.Wait()
}

// run is one repository's loop.
func (m *Manager) run(ctx context.Context, w *worker) {
	defer m.wg.Done()
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

// mirrorDiscussion refreshes the issues and pull requests beside a checkout.
//
// A mirror failure is recorded against the mirror, never against the
// repository. GitHub being unreachable, or a token being absent, does not
// make the checkout any less searchable, and marking the repository failed
// would tell an agent to stop reading a tree that is perfectly fine.
func (m *Manager) mirrorDiscussion(ctx context.Context, name string, spec *object.RepositorySpec) {
	if m.mirror == nil || !spec.Mirror.Enabled() {
		return
	}
	res, err := m.mirror.Sync(ctx, name, spec.URL, m.rec.Path(name), spec.Mirror)
	if ctx.Err() != nil {
		return // shutting down
	}
	if err != nil {
		m.logf("repository %s: mirror: %v", name, err)
		_ = m.store.SetMirror(object.KindRepository, name, "", err)
		return
	}
	if res == nil {
		return
	}
	_ = m.store.SetMirror(object.KindRepository, name, res.Summary(), nil)
}

// readSelfDescription applies what the checkout says about itself.
//
// Like the mirror, a failure here is recorded against the thing that failed
// and never against the repository: a malformed .drover.yaml is somebody
// else's mistake in somebody else's repository, and the checkout is still
// perfectly searchable.
func (m *Manager) readSelfDescription(ctx context.Context, name string, spec *object.RepositorySpec) {
	if m.selfdesc == nil {
		return
	}
	summary, err := m.selfdesc.Apply(ctx, name, spec)
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		m.logf("repository %s: %s: %v", name, repoconfig.FileName, err)
		_ = m.store.SetConfig(object.KindRepository, name, "", err)
		return
	}
	_ = m.store.SetConfig(object.KindRepository, name, summary, nil)
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

	// Remembered before the reset, so the callback below can tell a real
	// change from a reconcile that found nothing new.
	before := ""
	if st, err := m.store.GetStatus(object.KindRepository, name); err == nil {
		before = st.Commit
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
	m.mirrorDiscussion(ctx, name, spec)
	m.readSelfDescription(ctx, name, spec)
	if m.onChange != nil && res.Commit != before {
		m.onChange(name)
	}
	switch {
	case res.Cloned:
		m.logf("repository %s: cloned %s at %s", name, spec.Branch, short(res.Commit))
	case res.Updated:
		m.logf("repository %s: updated %s to %s", name, spec.Branch, short(res.Commit))
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
