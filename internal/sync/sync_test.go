package sync

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/notshekhar/drover/internal/object"
	"github.com/notshekhar/drover/internal/repo"
	"github.com/notshekhar/drover/internal/store"
)

func requireGit(t *testing.T) {
	t.Helper()
	if err := repo.Available(""); err != nil {
		t.Skip("git is not available")
	}
}

func gitEnv() []string {
	return append(os.Environ(),
		"GIT_AUTHOR_NAME=drover", "GIT_AUTHOR_EMAIL=drover@example.com",
		"GIT_COMMITTER_NAME=drover", "GIT_COMMITTER_EMAIL=drover@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	)
}

func originRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = gitEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-qm", "first")
	return dir
}

func repoDoc(name, url, branch, interval string) string {
	doc := "apiVersion: drover/v1\nkind: Repository\nmetadata:\n  name: " + name +
		"\nspec:\n  url: " + url + "\n  branch: " + branch + "\n"
	if interval != "" {
		doc += "  refreshInterval: " + interval + "\n"
	}
	return doc
}

func setup(t *testing.T) (*Manager, *store.Store, string) {
	t.Helper()
	dataDir := t.TempDir()
	st, err := store.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	rec := repo.New(dataDir)
	rec.Timeout = 60 * time.Second

	m := New(Options{Store: st, Repo: rec, Default: time.Hour})
	return m, st, dataDir
}

func put(t *testing.T, st *store.Store, doc string) *object.Object {
	t.Helper()
	objs, err := object.Parse("/work/repo.yaml", []byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Put(objs[0]); err != nil {
		t.Fatal(err)
	}
	return objs[0]
}

// waitFor polls until cond holds, so a background reconcile can be observed
// without a fixed sleep that is either flaky or slow.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// Applying a repository reconciles it now, not at the first tick an hour out.
func TestStartReconcilesImmediately(t *testing.T) {
	requireGit(t)
	origin := originRepo(t)
	m, st, dataDir := setup(t)
	put(t, st, repoDoc("api", origin, "main", ""))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer m.StopAll()

	waitFor(t, "the checkout to appear", func() bool {
		_, err := os.Stat(filepath.Join(dataDir, "repos", "api", "README.md"))
		return err == nil
	})

	st2, err := st.GetStatus(object.KindRepository, "api")
	if err != nil {
		t.Fatal(err)
	}
	if st2.Phase != store.PhaseReady {
		t.Errorf("phase = %q, want ready", st2.Phase)
	}
	if st2.Commit == "" {
		t.Error("no commit recorded")
	}
	if st2.LastSuccess == "" {
		t.Error("no success timestamp recorded")
	}
}

// A failure is recorded and does not kill the worker: the remote may come
// back, and a restart should not be needed to notice.
func TestFailureIsRecordedAndRetried(t *testing.T) {
	requireGit(t)
	m, st, _ := setup(t)
	missing := filepath.Join(t.TempDir(), "no-such-repo")
	put(t, st, repoDoc("api", missing, "main", ""))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer m.StopAll()

	waitFor(t, "the failure to be recorded", func() bool {
		s, err := st.GetStatus(object.KindRepository, "api")
		return err == nil && s.Phase == store.PhaseFailed
	})

	s, _ := st.GetStatus(object.KindRepository, "api")
	if s.Error == "" {
		t.Error("no error message recorded; `get repository` would show a failure with no cause")
	}

	// The worker is still alive and will try again.
	if err := m.SyncNow("api"); err != nil {
		t.Errorf("the worker died after a failure: %v", err)
	}
}

// One repository failing must not stop another from syncing.
func TestOneFailureDoesNotBlockOthers(t *testing.T) {
	requireGit(t)
	good := originRepo(t)
	m, st, dataDir := setup(t)
	put(t, st, repoDoc("broken", filepath.Join(t.TempDir(), "nope"), "main", ""))
	put(t, st, repoDoc("good", good, "main", ""))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer m.StopAll()

	waitFor(t, "the healthy repository to clone", func() bool {
		_, err := os.Stat(filepath.Join(dataDir, "repos", "good", "README.md"))
		return err == nil
	})
	waitFor(t, "the broken repository to be marked failed", func() bool {
		s, err := st.GetStatus(object.KindRepository, "broken")
		return err == nil && s.Phase == store.PhaseFailed
	})
}

// refreshInterval: never means the ticker leaves it alone, but an explicit
// sync still works.
func TestNeverDoesNotTickButSyncsOnDemand(t *testing.T) {
	requireGit(t)
	origin := originRepo(t)
	m, st, dataDir := setup(t)
	put(t, st, repoDoc("api", origin, "main", "never"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer m.StopAll()

	// Applying still reconciles once -- "never" governs the ticker, not the
	// initial clone, or the checkout would never exist at all.
	waitFor(t, "the initial clone", func() bool {
		_, err := os.Stat(filepath.Join(dataDir, "repos", "api", "README.md"))
		return err == nil
	})

	m.mu.Lock()
	w := m.workers["api"]
	m.mu.Unlock()
	if w == nil {
		t.Fatal("no worker was created")
	}
	if w.ticking {
		t.Error("a repository set to never is on the ticker")
	}
}

// Changing the interval reschedules straight away rather than waiting out the
// old cadence.
func TestEnsureReschedulesOnIntervalChange(t *testing.T) {
	requireGit(t)
	origin := originRepo(t)
	m, st, _ := setup(t)
	put(t, st, repoDoc("api", origin, "main", "24h"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer m.StopAll()

	m.mu.Lock()
	first := m.workers["api"].interval
	m.mu.Unlock()
	if first != 24*time.Hour {
		t.Fatalf("interval = %v, want 24h", first)
	}

	updated := put(t, st, repoDoc("api", origin, "main", "5m"))
	if err := m.Ensure(updated); err != nil {
		t.Fatal(err)
	}

	m.mu.Lock()
	second := m.workers["api"].interval
	m.mu.Unlock()
	if second != 5*time.Minute {
		t.Errorf("interval = %v, want the new 5m without waiting out the old one", second)
	}
}

// Each repository gets its own worker, which is what keeps one slow fetch
// from delaying everyone else.
func TestOneWorkerPerRepository(t *testing.T) {
	requireGit(t)
	origin := originRepo(t)
	m, st, _ := setup(t)
	for _, name := range []string{"api", "web", "worker"} {
		put(t, st, repoDoc(name, origin, "main", "1h"))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer m.StopAll()

	m.mu.Lock()
	n := len(m.workers)
	m.mu.Unlock()
	if n != 3 {
		t.Errorf("got %d workers, want one per repository", n)
	}
}

// A deleted repository's worker must stop, or it keeps reconciling an object
// that no longer exists.
func TestStopRemovesWorker(t *testing.T) {
	requireGit(t)
	origin := originRepo(t)
	m, st, _ := setup(t)
	put(t, st, repoDoc("api", origin, "main", "1h"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer m.StopAll()

	m.Stop("api")
	m.mu.Lock()
	_, ok := m.workers["api"]
	m.mu.Unlock()
	if ok {
		t.Error("the worker survived Stop")
	}
	if err := m.SyncNow("api"); err == nil {
		t.Error("SyncNow worked on a stopped repository")
	}
}

// Delete removes the checkout right after calling Stop, so Stop returning
// while a clone is still in flight would let the reconcile recreate the
// directory moments after it was deleted.
func TestStopWaitsForTheWorkerToExit(t *testing.T) {
	requireGit(t)
	origin := originRepo(t)
	m, st, dataDir := setup(t)
	put(t, st, repoDoc("api", origin, "main", "1h"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer m.StopAll()

	checkout := filepath.Join(dataDir, "repos", "api")
	waitFor(t, "the initial clone", func() bool {
		_, err := os.Stat(checkout)
		return err == nil
	})

	// Queue more work, then stop. Once Stop returns, nothing may touch the
	// directory -- which is what makes the removal below safe.
	_ = m.SyncNow("api")
	m.Stop("api")

	if err := os.RemoveAll(checkout); err != nil {
		t.Fatal(err)
	}
	// Give any goroutine that outlived Stop a chance to misbehave.
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(checkout); err == nil {
		t.Error("the checkout came back after Stop returned; a reconcile outlived it")
	}
}

// A server-wide --sync 0 turns the ticker off for repositories that did not
// ask for their own cadence.
func TestZeroDefaultDisablesTicking(t *testing.T) {
	requireGit(t)
	origin := originRepo(t)
	dataDir := t.TempDir()
	st, err := store.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	rec := repo.New(dataDir)
	rec.Timeout = 60 * time.Second
	m := New(Options{Store: st, Repo: rec, Default: 0})

	put(t, st, repoDoc("api", origin, "main", ""))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer m.StopAll()

	m.mu.Lock()
	w := m.workers["api"]
	m.mu.Unlock()
	if w.ticking {
		t.Error("a zero server default still put the repository on the ticker")
	}
}

// StopAll must wait for workers, so nothing writes into a directory that is
// being torn down.
func TestStopAllWaits(t *testing.T) {
	requireGit(t)
	origin := originRepo(t)
	m, st, _ := setup(t)
	put(t, st, repoDoc("api", origin, "main", "1h"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() { m.StopAll(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("StopAll did not return")
	}

	m.mu.Lock()
	n := len(m.workers)
	m.mu.Unlock()
	if n != 0 {
		t.Errorf("%d workers left after StopAll", n)
	}
}
