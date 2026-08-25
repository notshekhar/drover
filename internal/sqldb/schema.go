package sqldb

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/notshekhar/drover/internal/object"
)

// Schema is one database's shape, as drover observed it.
type Schema struct {
	Connection string
	Provider   object.Provider
	Version    string
	DumpedAt   time.Time
	Tables     []Table
	Truncated  bool
}

// Table is one relation.
type Table struct {
	Schema  string
	Name    string
	Kind    string // table, view
	Rows    int64  // estimate, and labelled as one
	Columns []Column
	Indexes []Index
}

// Qualified is the name a query would use.
func (t Table) Qualified() string {
	if t.Schema == "" || t.Schema == "public" {
		return t.Name
	}
	return t.Schema + "." + t.Name
}

// Column is one field.
type Column struct {
	Name       string
	Type       string
	Nullable   bool
	Default    string
	References string // "users(id)", when a foreign key says so
}

// Index is one index, rendered rather than modelled: nobody greps for an
// index's internals, they grep for whether one exists on a column.
type Index struct {
	Name       string
	Definition string
}

// maxTables bounds a dump. A warehouse with ten thousand tables would produce
// a file nobody can read and no model can hold; the allowlist in the spec is
// the real answer, and this is the backstop.
const maxTables = 2000

// DumpSchema reads a database's shape through the same read-only path a query
// takes.
//
// This exists because the sql tool's description used to tell the model to go
// discover tables through information_schema itself. That is a wasted round
// trip at the start of every session, and the result was thrown away when the
// session ended. Written to disk it is greppable, diffable across syncs, and
// free after the first time.
func (p *Pool) DumpSchema(ctx context.Context, name string, spec *object.SQLConnectionSpec) (*Schema, error) {
	db, provider, err := p.Open(name, spec)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, p.timeout(spec)*4) // catalogs are slow on a big database
	defer cancel()

	out := &Schema{Connection: name, Provider: provider, DumpedAt: time.Now().UTC()}
	// Both dialects answer version(); the string differs, and the first few
	// words of it are what a model needs to pick dialect-correct SQL.
	out.Version = scalar(ctx, db, "SELECT version()")

	switch provider {
	case object.ProviderMySQL:
		err = dumpMySQL(ctx, db, spec, out)
	default:
		err = dumpPostgres(ctx, db, spec, out)
	}
	if err != nil {
		return nil, err
	}
	sort.Slice(out.Tables, func(i, j int) bool {
		if out.Tables[i].Schema != out.Tables[j].Schema {
			return out.Tables[i].Schema < out.Tables[j].Schema
		}
		return out.Tables[i].Name < out.Tables[j].Name
	})
	if len(out.Tables) > maxTables {
		out.Tables, out.Truncated = out.Tables[:maxTables], true
	}
	return out, nil
}

func scalar(ctx context.Context, db *sql.DB, query string) string {
	var s sql.NullString
	if err := db.QueryRowContext(ctx, query).Scan(&s); err != nil {
		return ""
	}
	return firstWords(s.String, 4)
}

func firstWords(s string, n int) string {
	fields := strings.Fields(s)
	if len(fields) > n {
		fields = fields[:n]
	}
	return strings.Join(fields, " ")
}

// schemaFilter builds the WHERE clause that keeps a dump to the schemas
// somebody asked for, and out of the catalogs nobody did.
func schemaFilter(spec *object.SQLConnectionSpec, column string, systemSchemas []string) (string, []any) {
	if len(spec.Schemas) > 0 {
		marks := make([]string, len(spec.Schemas))
		args := make([]any, len(spec.Schemas))
		for i, s := range spec.Schemas {
			marks[i] = "?"
			args[i] = s
		}
		return fmt.Sprintf("%s IN (%s)", column, strings.Join(marks, ",")), args
	}
	marks := make([]string, len(systemSchemas))
	args := make([]any, len(systemSchemas))
	for i, s := range systemSchemas {
		marks[i] = "?"
		args[i] = s
	}
	return fmt.Sprintf("%s NOT IN (%s)", column, strings.Join(marks, ",")), args
}

// numbered rewrites ? placeholders into $1, $2 for the pgx driver, so the
// filter above can be written once for both dialects.
func numbered(query string) string {
	var b strings.Builder
	n := 0
	for i := 0; i < len(query); i++ {
		if query[i] == '?' {
			n++
			b.WriteString("$" + strconv.Itoa(n))
			continue
		}
		b.WriteByte(query[i])
	}
	return b.String()
}
