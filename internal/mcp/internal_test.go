package mcp

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/notshekhar/drover/internal/api"
	"github.com/notshekhar/drover/internal/object"
)

func TestStringifyCoercesArguments(t *testing.T) {
	// JSON has one number type, and an id of 42 must not become "42.0" in a URL.
	if got := stringify(float64(42)); got != "42" {
		t.Errorf("stringify(42) = %q", got)
	}
	if got := stringify(1.5); got != "1.5" {
		t.Errorf("stringify(1.5) = %q", got)
	}
	if got := stringify("x"); got != "x" {
		t.Errorf("stringify(\"x\") = %q", got)
	}
	if got := stringify(true); got != "true" {
		t.Errorf("stringify(true) = %q", got)
	}
	if got := stringify(nil); got != "" {
		t.Errorf("stringify(nil) = %q", got)
	}
}

// The search has to find a request by anything the document says, not only by
// its name, or a large collection is unusable.
func TestSearchRanking(t *testing.T) {
	items := []searchable{
		{Name: "list-issues", Fields: []string{"get", "{{baseurl}}/repos/{owner}/{repo}/issues", "github", "owner repo state"}},
		{Name: "get-repo", Fields: []string{"get", "{{baseurl}}/repos/{owner}/{repo}", "github", "fetch one github repository by owner and name"}},
		{Name: "get-user", Fields: []string{"get", "{{baseurl}}/users/{id}", "prod", "fetch one user by id"}},
	}
	key := func(s searchable) searchable { return s }

	cases := map[string]string{
		"issues":        "list-issues", // in the name
		"list-issues":   "list-issues", // exact
		"repository":    "get-repo",    // only in the description
		"user":          "get-user",
		"lsiss":         "list-issues", // an abbreviation, via subsequence
		"github issues": "list-issues", // two terms, both must hit
	}
	for query, want := range cases {
		got := rank(items, query, key)
		if len(got) == 0 {
			t.Errorf("search %q found nothing", query)
			continue
		}
		if got[0].Name != want {
			t.Errorf("search %q ranked %q first, want %q", query, got[0].Name, want)
		}
	}

	// Every term must match: this is an AND, not an OR.
	if got := rank(items, "issues nonexistentword", key); len(got) != 0 {
		t.Errorf("search with an unmatched term returned %d results, want 0", len(got))
	}

	// No search returns everything, in the original order.
	all := rank(items, "", key)
	if len(all) != len(items) || all[0].Name != "list-issues" {
		t.Errorf("an empty search changed the listing: %+v", all)
	}
	if all := rank(items, "   ", key); len(all) != len(items) {
		t.Errorf("a whitespace search filtered the listing")
	}
}

func TestIsSubsequence(t *testing.T) {
	if !isSubsequence("lsiss", "list-issues") {
		t.Error("lsiss should be a subsequence of list-issues")
	}
	if !isSubsequence("", "anything") {
		t.Error("the empty string is a subsequence of everything")
	}
	if isSubsequence("zzz", "list-issues") {
		t.Error("zzz is not a subsequence of list-issues")
	}
	// Order matters, or the match means nothing.
	if isSubsequence("seussi", "list-issues") {
		t.Error("characters out of order should not match")
	}
}

// stubBackend answers only what the tool list asks for. Everything else is
// left nil on the embedded interface, so a test that reaches further than it
// meant to panics rather than passing on a zero value.
type stubBackend struct {
	Backend

	mu   sync.Mutex
	sql  []api.ObjectView
	down bool
}

func (b *stubBackend) List(kind object.Kind) ([]api.ObjectView, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.down {
		return nil, errors.New("connect: connection refused")
	}
	if kind == object.KindSQLConnection {
		return append([]api.ObjectView(nil), b.sql...), nil
	}
	return nil, nil
}

func (b *stubBackend) LSP(api.LSPRequest) (*api.LSPResponse, error) {
	return &api.LSPResponse{}, nil
}

func (b *stubBackend) set(fn func(*stubBackend)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	fn(b)
}

// A tool list that changes mid-session is the whole reason listChanged is
// advertised: drover applies yaml dropped in its data directory on its own, so
// sql_query can appear without anyone restarting anything.
func TestToolChangeIsAnnounced(t *testing.T) {
	restore := toolChangePoll
	toolChangePoll = 5 * time.Millisecond
	t.Cleanup(func() { toolChangePoll = restore })

	backend := &stubBackend{}
	s := &Server{Backend: backend, Version: "test"}

	notes := make(chan string, 8)
	done := make(chan struct{})
	defer close(done)

	s.notify = func(method string, _ any) { notes <- method }
	s.watchDone = done
	s.startToolWatch()

	// Nothing has changed yet, so nothing may be sent. A watcher that
	// announced its own baseline would make every client re-list on connect.
	select {
	case m := <-notes:
		t.Fatalf("announced %q before anything changed", m)
	case <-time.After(50 * time.Millisecond):
	}

	backend.set(func(b *stubBackend) {
		b.sql = []api.ObjectView{{Kind: "SQLConnection", Name: "analytics", Provider: "postgres", Status: "ready"}}
	})

	select {
	case m := <-notes:
		if m != "notifications/tools/list_changed" {
			t.Fatalf("announced %q", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sql_query appeared and no notification was sent")
	}

	// One change, one notification. A watcher that re-announced every tick
	// would make the client re-list forever.
	select {
	case m := <-notes:
		t.Fatalf("announced %q a second time for one change", m)
	case <-time.After(50 * time.Millisecond):
	}
}

// An engine that cannot be reached collapses the tool list to the file tools.
// Announcing that would tell the client to re-list and cache the degraded
// answer, so the watcher stays quiet until the engine answers again.
func TestUnreachableEngineIsNotAToolChange(t *testing.T) {
	restore := toolChangePoll
	toolChangePoll = 5 * time.Millisecond
	t.Cleanup(func() { toolChangePoll = restore })

	backend := &stubBackend{sql: []api.ObjectView{{Kind: "SQLConnection", Name: "analytics", Provider: "postgres", Status: "ready"}}}
	s := &Server{Backend: backend, Version: "test"}

	notes := make(chan string, 8)
	done := make(chan struct{})
	defer close(done)

	s.notify = func(method string, _ any) { notes <- method }
	s.watchDone = done
	s.startToolWatch()

	backend.set(func(b *stubBackend) { b.down = true })

	select {
	case m := <-notes:
		t.Fatalf("announced %q for an engine that was merely unreachable", m)
	case <-time.After(100 * time.Millisecond):
	}

	// And when it comes back unchanged, there is still nothing to announce.
	backend.set(func(b *stubBackend) { b.down = false })
	select {
	case m := <-notes:
		t.Fatalf("announced %q after the engine came back unchanged", m)
	case <-time.After(100 * time.Millisecond):
	}
}

// startToolWatch must be idempotent: a client is free to send initialized
// twice, and a second watcher would double every notification.
func TestToolWatchStartsOnce(t *testing.T) {
	restore := toolChangePoll
	toolChangePoll = 5 * time.Millisecond
	t.Cleanup(func() { toolChangePoll = restore })

	backend := &stubBackend{}
	s := &Server{Backend: backend, Version: "test"}

	notes := make(chan string, 8)
	done := make(chan struct{})
	defer close(done)

	s.notify = func(method string, _ any) { notes <- method }
	s.watchDone = done
	s.startToolWatch()
	s.startToolWatch()
	s.startToolWatch()

	backend.set(func(b *stubBackend) {
		b.sql = []api.ObjectView{{Kind: "SQLConnection", Name: "analytics", Provider: "postgres", Status: "ready"}}
	})

	select {
	case <-notes:
	case <-time.After(2 * time.Second):
		t.Fatal("no notification at all")
	}
	select {
	case <-notes:
		t.Fatal("three starts produced more than one watcher")
	case <-time.After(80 * time.Millisecond):
	}
}
