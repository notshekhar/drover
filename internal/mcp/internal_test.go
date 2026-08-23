package mcp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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

func (b *stubBackend) List(_ context.Context, kind object.Kind) ([]api.ObjectView, error) {
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

func (b *stubBackend) LSP(context.Context, api.LSPRequest) (*api.LSPResponse, error) {
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

// blockingBackend hangs in Grep until its context is done, and reports which
// way it ended.
type blockingBackend struct {
	Backend
	entered chan struct{}
	ended   chan error
}

func (b *blockingBackend) List(context.Context, object.Kind) ([]api.ObjectView, error) {
	return nil, nil
}

func (b *blockingBackend) Grep(ctx context.Context, _ api.GrepRequest) (*api.GrepResponse, error) {
	close(b.entered)
	select {
	case <-ctx.Done():
		b.ended <- ctx.Err()
		return nil, ctx.Err()
	case <-time.After(5 * time.Second):
		b.ended <- errors.New("never cancelled")
		return nil, errors.New("never cancelled")
	}
}

// A tool call must be cancellable end to end. Before the context was threaded
// through the router, a client that hung up left the work running: the HTTP
// request context existed but stopped at the transport, and every Backend call
// below it was made with context.Background().
func TestHTTPToolCallIsCancelledWhenTheClientHangsUp(t *testing.T) {
	b := &blockingBackend{entered: make(chan struct{}), ended: make(chan error, 1)}
	h := (&Server{Backend: b, Version: "test"}).HTTPHandler()

	ctx, cancel := context.WithCancel(context.Background())
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"grep","arguments":{"pattern":"x"}}}`
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}()

	select {
	case <-b.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the grep never reached the backend")
	}
	cancel()

	select {
	case err := <-b.ended:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("backend ended with %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelling the request did not reach the backend")
	}
	<-done
}

// The half of listChanged that was missing. The stdio bridge has announced a
// changed tool list since the tool set was built; over HTTP, GET was answered
// with 405 and the docs said the transport had no channel for a
// server-initiated message. It does -- the client opens a GET and the server
// writes events down it -- and this is that path end to end.
func TestHTTPStreamDeliversToolChange(t *testing.T) {
	restore := toolChangePoll
	toolChangePoll = 5 * time.Millisecond
	t.Cleanup(func() { toolChangePoll = restore })

	backend := &stubBackend{}
	srv := &Server{Backend: backend, Version: "test"}
	h := srv.HTTPHandler()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req := httptest.NewRequest(http.MethodGet, "/mcp", nil).WithContext(ctx)
	req.Header.Set("Accept", "text/event-stream")
	// Not httptest.ResponseRecorder: the handler writes from its own goroutine
	// while the test reads, and the recorder is not safe for that.
	rec := newStreamRecorder()

	streamed := make(chan struct{})
	go func() {
		defer close(streamed)
		h.ServeHTTP(rec, req)
	}()

	// Wait for the subscription, so the change cannot land before anyone is
	// listening -- which would make this test pass or fail on scheduling.
	waitFor(t, func() bool { return srv.hub.listeners() == 1 })

	backend.set(func(b *stubBackend) {
		b.sql = []api.ObjectView{{Kind: "SQLConnection", Name: "analytics", Provider: "postgres", Status: "ready"}}
	})

	waitFor(t, func() bool {
		return strings.Contains(rec.body(), "notifications/tools/list_changed")
	})

	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if !strings.HasPrefix(rec.body(), "data: ") {
		t.Errorf("frame is not SSE-shaped: %q", rec.body())
	}

	cancel()
	<-streamed

	// The watcher exists to tell somebody. With the last stream gone there is
	// nobody, and it must stop rather than poll the store forever.
	waitFor(t, func() bool {
		srv.hub.mu.Lock()
		defer srv.hub.mu.Unlock()
		return !srv.hub.watching
	})
}

// A GET that did not ask for the stream keeps the old refusal: it is a client
// that does not know about it, or a person with a browser, and neither wants a
// connection that hangs open forever.
func TestHTTPPlainGetIsStillRefused(t *testing.T) {
	h := (&Server{Backend: &stubBackend{}, Version: "test"}).HTTPHandler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/mcp", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status %d, want 405", rec.Code)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition never became true")
}

// streamRecorder is a ResponseWriter that may be read while it is being
// written, which an open event stream requires and httptest's does not allow.
type streamRecorder struct {
	mu  sync.Mutex
	buf strings.Builder
	hdr http.Header
}

func newStreamRecorder() *streamRecorder { return &streamRecorder{hdr: http.Header{}} }

func (r *streamRecorder) Header() http.Header { return r.hdr }
func (r *streamRecorder) WriteHeader(int)     {}
func (r *streamRecorder) Flush()              {}

func (r *streamRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.Write(p)
}

func (r *streamRecorder) body() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.String()
}
