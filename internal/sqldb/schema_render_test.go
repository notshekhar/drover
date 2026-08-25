package sqldb

import (
	"strings"
	"testing"
	"time"
)

func sampleSchema() *Schema {
	return &Schema{
		Connection: "analytics",
		Version:    "PostgreSQL 16.3",
		DumpedAt:   time.Date(2026, 8, 25, 10, 2, 11, 0, time.UTC),
		Tables: []Table{{
			Schema: "public", Name: "events", Kind: "table", Rows: 48200000,
			Columns: []Column{
				{Name: "id", Type: "bigint"},
				{Name: "user_id", Type: "bigint", References: "users(id)"},
				{Name: "created_at", Type: "timestamp with time zone", Default: "now()"},
				{Name: "note", Type: "text", Nullable: true},
			},
			Indexes: []Index{{Name: "events_user_id", Definition: "CREATE INDEX events_user_id ON events (user_id)"}},
		}},
	}
}

func TestSQLRendersGreppableDDL(t *testing.T) {
	got := string(sampleSchema().SQL())
	for _, want := range []string{
		"CREATE TABLE events (",
		"REFERENCES users(id)",
		"NOT NULL",
		"DEFAULT now()",
		"CREATE INDEX events_user_id",
		"~48,200,000 rows",
		"ESTIMATES",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the dump is missing %q:\n%s", want, got)
		}
	}
	// The point of DDL over a listing: this grep is the question a model
	// actually has, and it has to work on one line.
	var found bool
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "REFERENCES users(id)") && strings.Contains(line, "user_id") {
			found = true
		}
	}
	if !found {
		t.Error("`grep 'REFERENCES users'` would not show which column it is")
	}
}

// Row estimates move constantly. A dump whose only change is the estimate has
// not had a schema change, and reporting one would train people to ignore it.
func TestDiffIgnoresRowCounts(t *testing.T) {
	a, b := sampleSchema(), sampleSchema()
	b.Tables[0].Rows = 51000000
	b.DumpedAt = a.DumpedAt.Add(time.Hour)
	if a.Diff(b) {
		t.Error("a changed row estimate was reported as a schema change")
	}

	b.Tables[0].Columns = append(b.Tables[0].Columns, Column{Name: "kind", Type: "text"})
	if !a.Diff(b) {
		t.Error("a new column was not reported as a schema change")
	}
}

func TestEmptySchemaSaysSo(t *testing.T) {
	s := &Schema{Connection: "x", DumpedAt: time.Now()}
	if !strings.Contains(string(s.SQL()), "no tables visible") {
		t.Error("an empty dump does not explain itself")
	}
}
