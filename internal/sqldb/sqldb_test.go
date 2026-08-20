package sqldb

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/notshekhar/drover/internal/object"
)

func specFrom(t *testing.T, body string) *object.SQLConnectionSpec {
	t.Helper()
	doc := "apiVersion: drover/v1\nkind: SQLConnection\nmetadata:\n  name: db\nspec:\n" + body
	objs, err := object.Parse("sql.yaml", []byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	spec, err := objs[0].SQLConnection()
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

// A ${ENV} url is followed to the process environment; that is the whole
// reason the reference form exists.
func TestResolveDSNFollowsEnvReference(t *testing.T) {
	p := NewPool()
	p.Getenv = func(n string) (string, bool) {
		if n == "DATABASE_URL" {
			return "postgres://user@host/db", true
		}
		return "", false
	}
	spec := specFrom(t, "  provider: postgres\n  url: ${DATABASE_URL}\n")

	dsn, err := p.ResolveDSN(spec)
	if err != nil {
		t.Fatal(err)
	}
	if dsn != "postgres://user@host/db" {
		t.Errorf("dsn = %q", dsn)
	}
}

func TestResolveDSNMissingEnvVarFails(t *testing.T) {
	p := NewPool()
	p.Getenv = func(string) (string, bool) { return "", false }
	spec := specFrom(t, "  provider: postgres\n  url: ${NOT_SET}\n")

	_, err := p.ResolveDSN(spec)
	if err == nil {
		t.Fatal("a missing environment variable resolved anyway")
	}
	if !strings.Contains(err.Error(), "NOT_SET") {
		t.Errorf("error = %q, want it to name the variable", err)
	}
}

// redshift:// is drover's own spelling; no driver knows it, so it must be
// rewritten to what pgx understands.
func TestRedshiftURLIsRewrittenForPGX(t *testing.T) {
	got, err := normalizeDSN(object.ProviderRedshift, "redshift://user@cluster.abc.eu-west-1.redshift.amazonaws.com:5439/dev")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "postgres://") {
		t.Errorf("dsn = %q, want it rewritten to postgres://", got)
	}
	if !strings.Contains(got, ":5439/dev") {
		t.Errorf("dsn = %q, want the rest of the url preserved", got)
	}
}

// go-sql-driver wants a DSN, not a URL, but a URL is what people write.
func TestMySQLURLToDSN(t *testing.T) {
	cases := map[string]string{
		"mysql://user:pw@db.example.com:3306/shop":        "user:pw@tcp(db.example.com:3306)/shop",
		"mysql://user@db.example.com/shop":                "user@tcp(db.example.com:3306)/shop",
		"mysql://root@127.0.0.1:3307/test?parseTime=true": "root@tcp(127.0.0.1:3307)/test?parseTime=true",
	}
	for in, want := range cases {
		got, err := mysqlURLToDSN(in)
		if err != nil {
			t.Errorf("mysqlURLToDSN(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("mysqlURLToDSN(%q) = %q, want %q", in, got, want)
		}
	}
}

// A DSN already in driver form is left alone.
func TestMySQLDSNPassesThrough(t *testing.T) {
	in := "user:pw@tcp(host:3306)/db"
	got, err := normalizeDSN(object.ProviderMySQL, in)
	if err != nil {
		t.Fatal(err)
	}
	if got != in {
		t.Errorf("got %q, want it untouched", got)
	}
}

// Read-only is enforced in the query path, not only at the tool boundary, so
// every route into the database goes through the same gate.
func TestQueryRefusesWritesOnReadOnlyConnection(t *testing.T) {
	p := NewPool()
	p.Getenv = func(string) (string, bool) { return "postgres://user@127.0.0.1:1/db", true }
	spec := specFrom(t, "  provider: postgres\n  url: ${DATABASE_URL}\n")

	// This must fail on the read-only check, before any connection attempt.
	_, err := p.Query(context.Background(), "db", spec, "DROP TABLE users")
	if err == nil {
		t.Fatal("a write was accepted on a read-only connection")
	}
	if !strings.Contains(err.Error(), "read-only") {
		t.Errorf("error = %q, want the read-only refusal rather than a connection error", err)
	}

	// Stacked statements are refused for the same reason.
	if _, err := p.Query(context.Background(), "db", spec, "SELECT 1; DROP TABLE users"); err == nil {
		t.Error("stacked statements were accepted")
	}
}

// No health query means no sql tool, which is the gate the plan asks for.
func TestHealthCheckRequiresAHealthQuery(t *testing.T) {
	p := NewPool()
	p.Getenv = func(string) (string, bool) { return "postgres://user@127.0.0.1:1/db", true }
	spec := specFrom(t, "  provider: postgres\n  url: ${DATABASE_URL}\n")

	err := p.HealthCheck(context.Background(), "db", spec)
	if err == nil {
		t.Fatal("a connection with no health query passed the gate")
	}
	if !strings.Contains(err.Error(), "no spec.health") {
		t.Errorf("error = %q", err)
	}
}

// --- live database, opt-in ---

// TestLiveDatabase runs against a real server when one is offered through
// DROVER_TEST_DSN, e.g.
//
//	DROVER_TEST_DSN=postgres://postgres:pw@127.0.0.1:5432/postgres \
//	DROVER_TEST_PROVIDER=postgres go test ./internal/sqldb/
//
// It is skipped otherwise, so the suite still runs with no database around.
func TestLiveDatabase(t *testing.T) {
	dsn := os.Getenv("DROVER_TEST_DSN")
	if dsn == "" {
		t.Skip("set DROVER_TEST_DSN to run against a real database")
	}
	provider := os.Getenv("DROVER_TEST_PROVIDER")
	if provider == "" {
		provider = "postgres"
	}

	p := NewPool()
	defer p.Close()
	p.Getenv = func(n string) (string, bool) {
		if n == "DATABASE_URL" {
			return dsn, true
		}
		return os.LookupEnv(n)
	}
	spec := specFrom(t, "  provider: "+provider+"\n  url: ${DATABASE_URL}\n  health: SELECT 1\n  maxRows: 3\n")

	if err := p.HealthCheck(context.Background(), "live", spec); err != nil {
		t.Fatalf("health check failed: %v", err)
	}

	res, err := p.Query(context.Background(), "live", spec, "SELECT 1 AS one")
	if err != nil {
		t.Fatal(err)
	}
	if res.RowCount != 1 || len(res.Columns) != 1 || res.Rows[0][0] != "1" {
		t.Errorf("result = %+v", res)
	}
	if res.Provider != provider {
		t.Errorf("provider = %q, want %q", res.Provider, provider)
	}

	// The row cap must actually bite, and say so.
	res, err = p.Query(context.Background(), "live", spec, selectManyRows(provider))
	if err != nil {
		t.Fatal(err)
	}
	if res.RowCount != 3 {
		t.Errorf("row count = %d, want the cap of 3", res.RowCount)
	}
	if !res.Truncated {
		t.Error("a capped result did not report itself as truncated")
	}

	// And a write is still refused.
	if _, err := p.Query(context.Background(), "live", spec, "CREATE TABLE drover_should_not_exist (a int)"); err == nil {
		t.Error("a CREATE was accepted on a read-only connection")
	}
}

func selectManyRows(provider string) string {
	if provider == "mysql" {
		return "SELECT 1 UNION ALL SELECT 2 UNION ALL SELECT 3 UNION ALL SELECT 4 UNION ALL SELECT 5"
	}
	return "SELECT * FROM generate_series(1, 10)"
}
