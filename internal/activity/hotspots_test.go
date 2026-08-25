package activity

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func newLedger(t *testing.T) *Ledger {
	t.Helper()
	l, err := Open(filepath.Join(t.TempDir(), FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

func record(t *testing.T, l *Ledger, r Record) {
	t.Helper()
	if r.At.IsZero() {
		r.At = time.Now()
	}
	if r.Outcome == "" {
		r.Outcome = "ok"
	}
	if err := l.Record(r); err != nil {
		t.Fatal(err)
	}
}

func TestHotspotsRankWhatAgentsActuallyRead(t *testing.T) {
	l := newLedger(t)
	for i := 0; i < 3; i++ {
		record(t, l, Record{Tool: "read", Repository: "api", Args: map[string]any{"path": "api/internal/db.go"}})
	}
	record(t, l, Record{Tool: "read", Repository: "api", Args: map[string]any{"path": "api/cmd/main.go"}})
	record(t, l, Record{Tool: "read", Repository: "web", Args: map[string]any{"path": "web/app.ts"}})
	// A failed read is not evidence that anyone found anything there.
	record(t, l, Record{Tool: "read", Repository: "api", Outcome: "error", Args: map[string]any{"path": "api/missing.go"}})

	got, err := l.Hotspots(context.Background(), 24*time.Hour, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d hotspots: %+v", len(got), got)
	}
	if got[0].Path != "api/internal/db.go" || got[0].Reads != 3 {
		t.Errorf("the most-read file is %+v", got[0])
	}
	for _, h := range got {
		if h.Path == "api/missing.go" {
			t.Error("a failed read was counted")
		}
	}
}

func TestHotspotsAreCappedPerRepository(t *testing.T) {
	l := newLedger(t)
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		record(t, l, Record{Tool: "read", Repository: "api", Args: map[string]any{"path": "api/" + name + ".go"}})
	}
	got, err := l.Hotspots(context.Background(), 24*time.Hour, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("the cap did not hold: %+v", got)
	}
}

// A recurring empty grep is usually a repository that should be in the
// warehouse and is not. Once is a typo; twice is a gap.
func TestEmptySearchesNeedRepeating(t *testing.T) {
	l := newLedger(t)
	record(t, l, Record{Tool: "grep", Summary: "0 matches in 40 files", Args: map[string]any{"pattern": "ChargeIntent"}})
	record(t, l, Record{Tool: "grep", Summary: "0 matches in 40 files", Args: map[string]any{"pattern": "ChargeIntent"}})
	record(t, l, Record{Tool: "grep", Summary: "0 matches in 40 files", Args: map[string]any{"pattern": "typo"}})
	record(t, l, Record{Tool: "grep", Summary: "12 matches in 40 files", Args: map[string]any{"pattern": "Server"}})

	got, err := l.EmptySearches(context.Background(), 24*time.Hour, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Pattern != "ChargeIntent" || got[0].Times != 2 {
		t.Fatalf("got %+v", got)
	}
}

func TestHotspotsOnAnEmptyLedger(t *testing.T) {
	got, err := newLedger(t).Hotspots(context.Background(), time.Hour, 5)
	if err != nil || len(got) != 0 {
		t.Fatalf("got %+v, %v", got, err)
	}
}
