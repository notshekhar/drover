package activity

import (
	"path/filepath"
	"testing"
	"time"
)

func TestLedgerRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	t0 := time.Now()
	first := Record{
		At:         t0,
		Tool:       "grep",
		Source:     "mcp-http",
		Client:     "claude-code/2.1.4",
		Session:    "abc",
		Repository: "api",
		Outcome:    "ok",
		Summary:    "17 matches in 6 files",
		Args:       map[string]any{"pattern": "Connect"},
		Duration:   12 * time.Millisecond,
	}
	if err := l.Record(first); err != nil {
		t.Fatal(err)
	}
	if err := l.Record(Record{
		At:   t0.Add(15 * time.Millisecond),
		Tool: "read", Source: "mcp-http", Session: "abc",
		Repository: "api", Outcome: "ok", Summary: "api/auth.go:1-40",
	}); err != nil {
		t.Fatal(err)
	}
	if err := l.Record(Record{
		At:   t0.Add(30 * time.Millisecond),
		Tool: "grep", Source: "cli", Outcome: "empty", Summary: "0 matches",
	}); err != nil {
		t.Fatal(err)
	}

	all, err := l.List(t.Context(), Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("len = %d, want 3", len(all))
	}
	// Newest first: the empty grep, then read, then the first grep.
	if all[0].Outcome != "empty" || all[1].Tool != "read" || all[2].Tool != "grep" {
		t.Errorf("order = %s/%s, %s/%s, %s/%s",
			all[0].Tool, all[0].Outcome, all[1].Tool, all[1].Outcome, all[2].Tool, all[2].Outcome)
	}

	// Session sequencing is assigned by the ledger, not the caller.
	sess, err := l.List(t.Context(), Filter{Session: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	if len(sess) != 2 {
		t.Fatalf("session len = %d", len(sess))
	}
	// Newest first: read is seq 2, grep is seq 1.
	if sess[0].SeqInSess != 2 || sess[1].SeqInSess != 1 {
		t.Errorf("seq = %d, %d, want 2 then 1", sess[0].SeqInSess, sess[1].SeqInSess)
	}
	if sess[0].SincePrevMS <= 0 {
		t.Error("the second call in a session should carry the gap since the first")
	}

	onlyEmpty, err := l.List(t.Context(), Filter{Outcome: "empty"})
	if err != nil {
		t.Fatal(err)
	}
	if len(onlyEmpty) != 1 || onlyEmpty[0].Tool != "grep" {
		t.Errorf("empty filter = %+v", onlyEmpty)
	}

	if all[2].Args["pattern"] != "Connect" {
		t.Errorf("args = %#v, want the pattern stored as received", all[2].Args)
	}

	got, err := l.Get(t.Context(), all[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != all[0].ID || got.Outcome != "empty" {
		t.Errorf("Get = %+v", got)
	}
	if _, err := l.Get(t.Context(), "no-such"); err != ErrNotFound {
		t.Errorf("missing id: %v, want ErrNotFound", err)
	}
}

func TestLedgerSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Record(Record{Tool: "ls", Outcome: "ok", Summary: "3 entries"}); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	l, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	all, err := l.List(t.Context(), Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Tool != "ls" {
		t.Errorf("after reopen: %+v", all)
	}
}

// The object filter is what an HTTPRequest or SQLConnection page runs on.
// Before it existed the page pulled the newest N calls and sieved them in the
// browser, so a busy engine showed an object no history at all.
func TestObjectFilterAndSearch(t *testing.T) {
	l := seeded(t)

	byObject, err := l.List(t.Context(), Filter{Object: "get-user"})
	if err != nil {
		t.Fatal(err)
	}
	if len(byObject) != 2 {
		t.Fatalf("object filter returned %d, want 2", len(byObject))
	}
	for _, r := range byObject {
		if r.Object != "get-user" {
			t.Errorf("object filter let through %q", r.Object)
		}
	}

	// Free text reaches summary, reason, error and the recorded arguments.
	for _, probe := range []struct {
		q    string
		want int
	}{
		{"matches", 1},  // summary
		{"minted", 1},   // reason
		{"refused", 1},  // error
		{"Connect", 1},  // args
		{"get-user", 2}, // object column
		{"nothing here", 0},
	} {
		got, err := l.List(t.Context(), Filter{Q: probe.q})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != probe.want {
			t.Errorf("q=%q returned %d, want %d", probe.q, len(got), probe.want)
		}
	}

	// A wildcard someone typed is a literal, not a match-everything.
	all, err := l.List(t.Context(), Filter{})
	if err != nil {
		t.Fatal(err)
	}
	pct, err := l.List(t.Context(), Filter{Q: "%"})
	if err != nil {
		t.Fatal(err)
	}
	if len(pct) == len(all) {
		t.Errorf(`q="%%" matched all %d records; the wildcard was not escaped`, len(all))
	}
}

func TestSortAndCursorPaging(t *testing.T) {
	l := seeded(t)

	slow, err := l.List(t.Context(), Filter{Sort: "slow"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(slow); i++ {
		if slow[i-1].DurationMS < slow[i].DurationMS {
			t.Fatalf("slow sort is not descending: %d before %d",
				slow[i-1].DurationMS, slow[i].DurationMS)
		}
	}

	// Paging by cursor: page two starts strictly after page one ends, and
	// the two together are the whole log with nothing repeated or skipped.
	first, err := l.List(t.Context(), Filter{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 {
		t.Fatalf("page one len = %d", len(first))
	}
	rest, err := l.List(t.Context(), Filter{Before: first[len(first)-1].ID})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, r := range append(append([]Record{}, first...), rest...) {
		if seen[r.ID] {
			t.Errorf("paging repeated %s", r.ID)
		}
		seen[r.ID] = true
	}
	all, err := l.List(t.Context(), Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != len(all) {
		t.Errorf("paging covered %d of %d records", len(seen), len(all))
	}
}

// Stats must count the same set List returns, or a chip contradicts the rows
// beneath it.
func TestStatsAgreeWithList(t *testing.T) {
	l := seeded(t)

	st, err := l.Stats(t.Context(), Filter{})
	if err != nil {
		t.Fatal(err)
	}
	all, err := l.List(t.Context(), Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if st.Total != len(all) {
		t.Errorf("stats total = %d, list len = %d", st.Total, len(all))
	}
	if st.SlowestMS != 900 {
		t.Errorf("slowest = %dms, want 900", st.SlowestMS)
	}
	for _, b := range st.Tools {
		rows, err := l.List(t.Context(), Filter{Tool: b.Value})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != b.Count {
			t.Errorf("tool %q: chip says %d, list says %d", b.Value, b.Count, len(rows))
		}
	}

	// Paging and ordering are about a page of rows, never about the totals.
	paged, err := l.Stats(t.Context(), Filter{Limit: 1, Before: all[0].ID, Sort: "slow"})
	if err != nil {
		t.Fatal(err)
	}
	if paged.Total != st.Total {
		t.Errorf("a cursor changed the totals: %d then %d", st.Total, paged.Total)
	}

	// A filter narrows the facets with it.
	narrowed, err := l.Stats(t.Context(), Filter{Outcome: "error"})
	if err != nil {
		t.Fatal(err)
	}
	if narrowed.Total != 1 {
		t.Errorf("error total = %d, want 1", narrowed.Total)
	}
	if len(narrowed.Outcomes) != 1 || narrowed.Outcomes[0].Value != "error" {
		t.Errorf("outcome facet under an outcome filter = %+v", narrowed.Outcomes)
	}
}

func seeded(t *testing.T) *Ledger {
	t.Helper()
	l, err := Open(filepath.Join(t.TempDir(), FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })

	t0 := time.Now().Add(-time.Hour)
	for i, r := range []Record{
		{
			Tool: "grep", Source: "mcp-http", Session: "abc", Repository: "api",
			Outcome: "ok", Summary: "17 matches in 6 files",
			Reason: "finding where sessions are minted",
			Args:   map[string]any{"pattern": "handshake"}, DurationMS: 124,
		},
		{
			Tool: "read", Source: "mcp-http", Session: "abc", Repository: "api",
			Outcome: "ok", Summary: "api/auth.go:1-40", DurationMS: 3,
		},
		{
			Tool: "api_call", Source: "cli", Object: "get-user",
			Outcome: "error", Error: "the remote refused the connection",
			DurationMS: 900,
		},
		{
			Tool: "api_call", Source: "web", Object: "get-user",
			Outcome: "ok", Summary: "GET -> 200", DurationMS: 41,
		},
	} {
		r.At = t0.Add(time.Duration(i) * time.Second)
		if err := l.Record(r); err != nil {
			t.Fatal(err)
		}
	}
	return l
}
