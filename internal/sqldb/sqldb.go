// Package sqldb connects to a SQLConnection and runs queries.
//
// Three providers: postgres, mysql and redshift. Redshift speaks the Postgres
// wire protocol, so it shares that driver while keeping its own name and
// dialect note, because the two are not the same thing to a person writing
// SQL against them.
package sqldb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/notshekhar/drover/internal/object"
)

// DefaultTimeout caps one query when the document does not.
const DefaultTimeout = 30 * time.Second

// Pool holds open connections, keyed by object name, so a health check and a
// query do not each pay a fresh handshake.
type Pool struct {
	mu     sync.Mutex
	dbs    map[string]*sql.DB
	Getenv func(string) (string, bool)
}

// NewPool returns an empty pool.
func NewPool() *Pool {
	return &Pool{dbs: map[string]*sql.DB{}, Getenv: os.LookupEnv}
}

// ResolveDSN turns spec.url into a connection string, following a ${ENV}
// reference when that is what it is.
func (p *Pool) ResolveDSN(spec *object.SQLConnectionSpec) (string, error) {
	getenv := p.Getenv
	if getenv == nil {
		getenv = os.LookupEnv
	}
	r := &object.Resolver{Process: getenv}
	dsn, err := r.Resolve(spec.URL)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(dsn) == "" {
		return "", errors.New("the connection url resolved to an empty string")
	}
	return dsn, nil
}

// Open returns a pooled connection for one object.
func (p *Pool) Open(name string, spec *object.SQLConnectionSpec) (*sql.DB, object.Provider, error) {
	provider, err := spec.ResolveProvider()
	if err != nil {
		return nil, "", err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if db, ok := p.dbs[name]; ok {
		return db, provider, nil
	}

	dsn, err := p.ResolveDSN(spec)
	if err != nil {
		return nil, provider, err
	}
	dsn, err = normalizeDSN(provider, dsn)
	if err != nil {
		return nil, provider, err
	}

	db, err := sql.Open(provider.Driver(), dsn)
	if err != nil {
		return nil, provider, fmt.Errorf("open %s: %w", provider, err)
	}
	// A context tool should hold a couple of connections, not a hundred.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	db.SetConnMaxIdleTime(5 * time.Minute)

	p.dbs[name] = db
	return db, provider, nil
}

// normalizeDSN adapts a url to what the driver expects.
//
// pgx takes a postgres:// url as-is, but redshift:// is drover's own spelling
// and no driver knows it, so it is rewritten. MySQL's driver wants a DSN
// rather than a url, so a mysql:// url is converted.
func normalizeDSN(provider object.Provider, dsn string) (string, error) {
	switch provider {
	case object.ProviderRedshift:
		if strings.HasPrefix(dsn, "redshift://") {
			return "postgres://" + strings.TrimPrefix(dsn, "redshift://"), nil
		}
		return dsn, nil
	case object.ProviderMySQL:
		if strings.HasPrefix(dsn, "mysql://") {
			converted, err := mysqlURLToDSN(dsn)
			if err != nil {
				return "", err
			}
			return converted, nil
		}
		return dsn, nil
	default:
		return dsn, nil
	}
}

// Close shuts every connection down.
func (p *Pool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for name, db := range p.dbs {
		_ = db.Close()
		delete(p.dbs, name)
	}
}

// Forget drops one connection, for when its object is deleted or changed.
func (p *Pool) Forget(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if db, ok := p.dbs[name]; ok {
		_ = db.Close()
		delete(p.dbs, name)
	}
}

// Pooled reports whether a connection is currently open for name.
//
// Exported for the tests that pin when the pool is dropped: a re-applied or
// deleted SQLConnection must not go on answering from the connection its
// previous document opened.
func (p *Pool) Pooled(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.dbs[name]
	return ok
}

// HealthCheck runs spec.health.
//
// This is the gate: no health query, or one that fails, means no sql tool is
// advertised. A database an agent cannot reach is worse than one it was never
// told about, because it will keep trying.
func (p *Pool) HealthCheck(ctx context.Context, name string, spec *object.SQLConnectionSpec) error {
	if strings.TrimSpace(spec.Health) == "" {
		return errors.New("no spec.health query, so no sql tool is offered for this connection")
	}
	db, _, err := p.Open(name, spec)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, p.timeout(spec))
	defer cancel()

	rows, err := db.QueryContext(ctx, spec.Health)
	if err != nil {
		return fmt.Errorf("health query failed: %w", err)
	}
	defer rows.Close()
	// Draining matters: a health query that errors mid-stream has not really
	// succeeded, and rows.Err only reports that after iteration.
	for rows.Next() {
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("health query failed: %w", err)
	}
	return nil
}

func (p *Pool) timeout(spec *object.SQLConnectionSpec) time.Duration {
	if spec.TimeoutSeconds > 0 {
		return time.Duration(spec.TimeoutSeconds) * time.Second
	}
	return DefaultTimeout
}

// Result is one query's output.
type Result struct {
	Columns   []string   `json:"columns"`
	Rows      [][]string `json:"rows"`
	RowCount  int        `json:"rowCount"`
	Truncated bool       `json:"truncated,omitempty"`
	Provider  string     `json:"provider"`
	Elapsed   int64      `json:"elapsedMs"`
}

// Query runs a statement and returns rows.
//
// Read-only is enforced here rather than only at the tool boundary, so every
// path into the database goes through the same gate.
func (p *Pool) Query(ctx context.Context, name string, spec *object.SQLConnectionSpec, query string, args ...any) (*Result, error) {
	if spec.IsReadOnly() {
		if err := object.CheckReadOnly(query); err != nil {
			return nil, err
		}
	}

	db, provider, err := p.Open(name, spec)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, p.timeout(spec))
	defer cancel()

	start := time.Now()
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query failed: %w", err)
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}

	limit := spec.RowLimit()
	out := &Result{Columns: cols, Provider: string(provider)}

	for rows.Next() {
		if len(out.Rows) >= limit {
			// One more row exists, so say so rather than quietly cutting the
			// answer short.
			out.Truncated = true
			break
		}
		holders := make([]any, len(cols))
		values := make([]sql.RawBytes, len(cols))
		for i := range holders {
			holders[i] = &values[i]
		}
		if err := rows.Scan(holders...); err != nil {
			return nil, fmt.Errorf("read row: %w", err)
		}
		row := make([]string, len(cols))
		for i, v := range values {
			if v == nil {
				row[i] = "NULL"
				continue
			}
			row[i] = string(v)
		}
		out.Rows = append(out.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read rows: %w", err)
	}

	out.RowCount = len(out.Rows)
	out.Elapsed = time.Since(start).Milliseconds()
	return out, nil
}
