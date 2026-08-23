// Package activity is drover's ledger of tool calls.
//
// It lives at ~/.drover/activity.db (or <data-dir>/activity.db). It is not a
// SQLConnection and it is not something an agent queries: it is what the
// engine writes so a person can see what ran.
package activity

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotFound is returned by Get when the id is not in the ledger.
var ErrNotFound = errors.New("activity record not found")

// FileName is the ledger's name inside the data directory.
const FileName = "activity.db"

const schema = `
CREATE TABLE IF NOT EXISTS calls (
	id            TEXT PRIMARY KEY,
	at            TEXT NOT NULL,
	duration_ms   INTEGER NOT NULL DEFAULT 0,
	tool          TEXT NOT NULL,
	op            TEXT NOT NULL DEFAULT '',
	args          TEXT,
	reason        TEXT NOT NULL DEFAULT '',
	source        TEXT NOT NULL DEFAULT '',
	client        TEXT NOT NULL DEFAULT '',
	session       TEXT NOT NULL DEFAULT '',
	seq_in_sess   INTEGER NOT NULL DEFAULT 0,
	since_prev_ms INTEGER NOT NULL DEFAULT 0,
	repository    TEXT NOT NULL DEFAULT '',
	object        TEXT NOT NULL DEFAULT '',
	outcome       TEXT NOT NULL,
	error         TEXT NOT NULL DEFAULT '',
	summary       TEXT NOT NULL DEFAULT '',
	bytes         INTEGER NOT NULL DEFAULT 0,
	truncated     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS calls_at ON calls(at DESC);
CREATE INDEX IF NOT EXISTS calls_session ON calls(session, seq_in_sess);
CREATE INDEX IF NOT EXISTS calls_tool ON calls(tool);
CREATE INDEX IF NOT EXISTS calls_repo ON calls(repository);
CREATE INDEX IF NOT EXISTS calls_outcome ON calls(outcome);
`

// Record is one tool call.
type Record struct {
	ID       string        `json:"id"`
	At       time.Time     `json:"at"`
	Duration time.Duration `json:"-"`

	Tool   string         `json:"tool"`
	Op     string         `json:"op,omitempty"`
	Args   map[string]any `json:"args,omitempty"`
	Reason string         `json:"reason,omitempty"`

	Source    string        `json:"source,omitempty"`
	Client    string        `json:"client,omitempty"`
	Session   string        `json:"session,omitempty"`
	SeqInSess int           `json:"seqInSess,omitempty"`
	SincePrev time.Duration `json:"-"`

	Repository string `json:"repository,omitempty"`
	Object     string `json:"object,omitempty"`

	Outcome   string `json:"outcome"`
	Error     string `json:"error,omitempty"`
	Summary   string `json:"summary,omitempty"`
	Bytes     int    `json:"bytes,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`

	DurationMS  int64 `json:"durationMs"`
	SincePrevMS int64 `json:"sincePrevMs,omitempty"`
}

// Filter is what List accepts. Empty fields are ignored.
type Filter struct {
	Limit      int
	Tool       string
	Source     string
	Session    string
	Repository string
	Outcome    string

	// Object narrows to one HTTPRequest or SQLConnection by name. Without it
	// an object page has to fetch the whole log and sieve it in the browser,
	// which quietly shows nothing once the log is longer than the page size.
	Object string

	// Q is a free-text match over summary, reason, error and the recorded
	// arguments. It is a LIKE, not an index: the ledger is small and this is
	// a human typing into a box.
	Q string

	// Before is a record id. Everything strictly older than that record is
	// returned, so paging is a cursor and not an offset -- the log grows at
	// the head while you read it.
	Before string

	// Sort is "recent" (default, newest first) or "slow" (longest first).
	// "what is making this feel bad" is a question the log can answer and a
	// scroll cannot.
	Sort string
}

// Ledger is the sqlite file.
type Ledger struct {
	path string
	db   *sql.DB

	mu      sync.Mutex
	lastSeq map[string]int
	lastAt  map[string]time.Time
}

// Open creates or opens the ledger at path, chmod 0600.
func Open(path string) (*Ledger, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open activity ledger: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("activity ledger schema: %w", err)
	}
	_ = os.Chmod(path, 0o600)

	return &Ledger{
		path:    path,
		db:      db,
		lastSeq: map[string]int{},
		lastAt:  map[string]time.Time{},
	}, nil
}

// Path is the file on disk.
func (l *Ledger) Path() string { return l.path }

// Close releases the file. Safe to call twice.
func (l *Ledger) Close() error {
	if l == nil || l.db == nil {
		return nil
	}
	err := l.db.Close()
	l.db = nil
	return err
}

// Record writes one call. A tool path should not fail because the ledger
// could not write, so the wrapper ignores the error; tests call this
// directly and check it.
func (l *Ledger) Record(r Record) error {
	if l == nil || l.db == nil {
		return nil
	}
	if r.At.IsZero() {
		r.At = time.Now()
	}
	if r.ID == "" {
		r.ID = newID(r.At)
	}
	if r.DurationMS == 0 && r.Duration > 0 {
		r.DurationMS = r.Duration.Milliseconds()
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if r.Session != "" {
		l.lastSeq[r.Session]++
		r.SeqInSess = l.lastSeq[r.Session]
		if prev, ok := l.lastAt[r.Session]; ok {
			r.SincePrev = r.At.Sub(prev)
			r.SincePrevMS = r.SincePrev.Milliseconds()
		}
		l.lastAt[r.Session] = r.At
	}

	var args any
	if r.Args != nil {
		b, err := json.Marshal(r.Args)
		if err != nil {
			return err
		}
		args = string(b)
	}

	truncated := 0
	if r.Truncated {
		truncated = 1
	}

	_, err := l.db.ExecContext(context.Background(),
		`INSERT INTO calls (
			id, at, duration_ms, tool, op, args, reason,
			source, client, session, seq_in_sess, since_prev_ms,
			repository, object, outcome, error, summary, bytes, truncated
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.At.UTC().Format("2006-01-02T15:04:05.000000000Z07:00"), r.DurationMS,
		r.Tool, r.Op, args, r.Reason,
		r.Source, r.Client, r.Session, r.SeqInSess, r.SincePrevMS,
		r.Repository, r.Object, r.Outcome, r.Error, r.Summary, r.Bytes, truncated,
	)
	return err
}

// where builds the shared WHERE clause. List and Stats must agree on what a
// filter means, so they read it from one place: a facet count that disagrees
// with the rows under it is worse than no facet count.
func (f Filter) where() (string, []any) {
	q := strings.Builder{}
	var args []any
	eq := func(col, val string) {
		if val == "" {
			return
		}
		q.WriteString(" AND " + col + " = ?")
		args = append(args, val)
	}
	eq("tool", f.Tool)
	eq("source", f.Source)
	eq("session", f.Session)
	eq("repository", f.Repository)
	eq("outcome", f.Outcome)
	eq("object", f.Object)
	if f.Q != "" {
		// LIKE with the wildcards supplied here, so a % a person typed is a
		// literal percent sign and not a match-everything.
		like := "%" + escapeLike(f.Q) + "%"
		q.WriteString(` AND (summary LIKE ? ESCAPE '\' OR reason LIKE ? ESCAPE '\'` +
			` OR error LIKE ? ESCAPE '\' OR args LIKE ? ESCAPE '\'` +
			` OR tool LIKE ? ESCAPE '\' OR repository LIKE ? ESCAPE '\'` +
			` OR object LIKE ? ESCAPE '\')`)
		for i := 0; i < 7; i++ {
			args = append(args, like)
		}
	}
	if f.Before != "" {
		// A tuple compare against the cursor row, so two calls in the same
		// nanosecond cannot make paging loop.
		q.WriteString(" AND (at, id) < (SELECT at, id FROM calls WHERE id = ?)")
		args = append(args, f.Before)
	}
	return q.String(), args
}

func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`)
	return r.Replace(s)
}

// List returns records newest first, or slowest first when Sort is "slow".
func (l *Ledger) List(ctx context.Context, f Filter) ([]Record, error) {
	if l == nil || l.db == nil {
		return nil, nil
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}

	where, args := f.where()
	order := " ORDER BY at DESC, id DESC"
	if f.Sort == "slow" {
		order = " ORDER BY duration_ms DESC, at DESC, id DESC"
	}
	args = append(args, limit)

	rows, err := l.db.QueryContext(ctx,
		`SELECT `+recordCols+` FROM calls WHERE 1=1`+where+order+` LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Record
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Bucket is one facet value and how many records carry it.
type Bucket struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

// Stats is what the filter bar draws above the rows: the same query, counted
// by the columns you would filter on next.
type Stats struct {
	Total    int      `json:"total"`
	Tools    []Bucket `json:"tools"`
	Outcomes []Bucket `json:"outcomes"`
	Sources  []Bucket `json:"sources"`
	Repos    []Bucket `json:"repositories"`

	// SlowestMS is the longest call in the filtered set, so the table can
	// scale its duration bars against something real.
	SlowestMS int64 `json:"slowestMs"`
}

// Stats counts the filtered set by tool, outcome, source and repository.
//
// Every facet is counted against the *whole* filter, including the facet's
// own column. A chip that says "grep 12" while grep is selected is telling
// you the size of what you are looking at, which is the honest reading.
func (l *Ledger) Stats(ctx context.Context, f Filter) (Stats, error) {
	var st Stats
	if l == nil || l.db == nil {
		return st, nil
	}
	// Paging and ordering are about a page of rows, never about the totals.
	f.Before, f.Limit, f.Sort = "", 0, ""
	where, args := f.where()

	row := l.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(MAX(duration_ms), 0) FROM calls WHERE 1=1`+where, args...)
	if err := row.Scan(&st.Total, &st.SlowestMS); err != nil {
		return st, err
	}

	for _, facet := range []struct {
		col string
		out *[]Bucket
	}{
		{"tool", &st.Tools},
		{"outcome", &st.Outcomes},
		{"source", &st.Sources},
		{"repository", &st.Repos},
	} {
		b, err := l.buckets(ctx, facet.col, where, args)
		if err != nil {
			return st, err
		}
		*facet.out = b
	}
	return st, nil
}

func (l *Ledger) buckets(ctx context.Context, col, where string, args []any) ([]Bucket, error) {
	rows, err := l.db.QueryContext(ctx,
		`SELECT `+col+`, COUNT(*) c FROM calls WHERE 1=1`+where+
			` AND `+col+` != '' GROUP BY `+col+` ORDER BY c DESC, `+col+` ASC LIMIT 20`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Bucket{}
	for rows.Next() {
		var b Bucket
		if err := rows.Scan(&b.Value, &b.Count); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

const recordCols = `id, at, duration_ms, tool, op, args, reason,
		source, client, session, seq_in_sess, since_prev_ms,
		repository, object, outcome, error, summary, bytes, truncated`

// Get returns one record by id.
func (l *Ledger) Get(ctx context.Context, id string) (*Record, error) {
	if l == nil || l.db == nil || id == "" {
		return nil, ErrNotFound
	}
	row := l.db.QueryRowContext(ctx, `SELECT `+recordCols+` FROM calls WHERE id = ?`, id)
	r, err := scanRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanRecord(s scanner) (Record, error) {
	var r Record
	var at string
	var argsJSON sql.NullString
	var truncated int
	if err := s.Scan(
		&r.ID, &at, &r.DurationMS, &r.Tool, &r.Op, &argsJSON, &r.Reason,
		&r.Source, &r.Client, &r.Session, &r.SeqInSess, &r.SincePrevMS,
		&r.Repository, &r.Object, &r.Outcome, &r.Error, &r.Summary, &r.Bytes, &truncated,
	); err != nil {
		return Record{}, err
	}
	r.At, _ = time.Parse("2006-01-02T15:04:05.000000000Z07:00", at)
	r.Duration = time.Duration(r.DurationMS) * time.Millisecond
	r.SincePrev = time.Duration(r.SincePrevMS) * time.Millisecond
	r.Truncated = truncated != 0
	if argsJSON.Valid && argsJSON.String != "" {
		_ = json.Unmarshal([]byte(argsJSON.String), &r.Args)
	}
	return r, nil
}

func newID(at time.Time) string {
	var b [2]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%d-%s", at.UnixNano(), hex.EncodeToString(b[:]))
}
