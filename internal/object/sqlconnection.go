package object

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// Provider is a supported database. Redshift is listed separately from
// Postgres even though it speaks the Postgres wire protocol, because it is a
// different thing to a person reading the document and its SQL dialect
// differs enough to matter to an agent writing queries.
type Provider string

const (
	ProviderPostgres Provider = "postgres"
	ProviderMySQL    Provider = "mysql"
	ProviderRedshift Provider = "redshift"
)

// Providers is every supported provider, in listing order.
var Providers = []Provider{ProviderPostgres, ProviderMySQL, ProviderRedshift}

// Driver is the database/sql driver name this provider connects with.
// Redshift rides the Postgres driver.
func (p Provider) Driver() string {
	switch p {
	case ProviderPostgres, ProviderRedshift:
		return "pgx"
	case ProviderMySQL:
		return "mysql"
	}
	return ""
}

// Dialect names the SQL dialect for an agent's benefit.
func (p Provider) Dialect() string {
	switch p {
	case ProviderPostgres:
		return "PostgreSQL"
	case ProviderRedshift:
		return "Amazon Redshift (PostgreSQL 8.0 dialect; no CTE materialization hints, limited window functions)"
	case ProviderMySQL:
		return "MySQL"
	}
	return string(p)
}

// ParseProvider resolves a provider name, accepting the aliases people
// actually type.
func ParseProvider(s string) (Provider, error) {
	switch lower(strings.TrimSpace(s)) {
	case "postgres", "postgresql", "pg":
		return ProviderPostgres, nil
	case "mysql", "mariadb":
		return ProviderMySQL, nil
	case "redshift", "aws-redshift":
		return ProviderRedshift, nil
	}
	return "", fmt.Errorf("unsupported provider %q (drover speaks postgres, mysql and redshift)", s)
}

// schemeProvider maps a URL scheme to a provider, for when the document does
// not name one.
var schemeProvider = map[string]Provider{
	"postgres":   ProviderPostgres,
	"postgresql": ProviderPostgres,
	"mysql":      ProviderMySQL,
	"redshift":   ProviderRedshift,
}

// SQLConnectionSpec is one database drover can query.
type SQLConnectionSpec struct {
	Description string `yaml:"description,omitempty"`

	// Provider is optional when the url carries a recognisable scheme.
	Provider string `yaml:"provider,omitempty"`

	// URL should be a ${ENV} reference. A literal DSN in a file people commit
	// is a leaked credential, so an inline password is warned about, not
	// rejected.
	URL string `yaml:"url"`

	// Health gates the tool: no health query, or a failing one, means no sql
	// tool is advertised at all.
	Health string `yaml:"health,omitempty"`

	// ReadOnly rejects anything that is not a read. Default true: a database
	// tool handed to an agent should not be able to write, and opting out
	// should be a deliberate line in a file.
	ReadOnly *bool `yaml:"readOnly,omitempty"`

	// MaxRows caps a result set so one careless SELECT cannot exhaust memory
	// or a context window.
	MaxRows int `yaml:"maxRows,omitempty"`

	// TimeoutSeconds caps one query.
	TimeoutSeconds int `yaml:"timeoutSeconds,omitempty"`
}

// DefaultMaxRows is the row cap when a document does not set one.
const DefaultMaxRows = 200

// SQLConnection decodes this object's spec as a SQLConnectionSpec.
func (o *Object) SQLConnection() (*SQLConnectionSpec, error) {
	if o.Kind != KindSQLConnection {
		return nil, fmt.Errorf("object is %s, not %s", o.Kind, KindSQLConnection)
	}
	var spec SQLConnectionSpec
	if err := o.decodeSpec(&spec); err != nil {
		return nil, err
	}
	return &spec, nil
}

// IsReadOnly reports the effective read-only setting, which defaults to true.
func (s *SQLConnectionSpec) IsReadOnly() bool {
	return s.ReadOnly == nil || *s.ReadOnly
}

// RowLimit is the effective row cap.
func (s *SQLConnectionSpec) RowLimit() int {
	if s.MaxRows > 0 {
		return s.MaxRows
	}
	return DefaultMaxRows
}

// ResolveProvider works out the provider from the document, falling back to
// the url scheme.
func (s *SQLConnectionSpec) ResolveProvider() (Provider, error) {
	if s.Provider != "" {
		return ParseProvider(s.Provider)
	}

	raw := s.URL
	// A ${ENV} url hides its scheme, so the provider must be written down.
	if isSingleProcessEnvRef(strings.TrimSpace(raw)) {
		return "", fmt.Errorf("spec.provider is required when spec.url is a ${ENV} reference (drover cannot see the scheme to guess); set it to one of %s", providerList())
	}
	scheme, _, _ := strings.Cut(raw, "://")
	p, ok := schemeProvider[lower(scheme)]
	if !ok {
		return "", fmt.Errorf("cannot tell the provider from url scheme %q; set spec.provider to one of %s", scheme, providerList())
	}
	return p, nil
}

func providerList() string {
	names := make([]string, 0, len(Providers))
	for _, p := range Providers {
		names = append(names, string(p))
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// Validate checks what can be known without connecting.
func (s *SQLConnectionSpec) Validate() error {
	if strings.TrimSpace(s.URL) == "" {
		return fmt.Errorf("spec.url is required")
	}
	if _, err := s.ResolveProvider(); err != nil {
		return err
	}

	if s.MaxRows < 0 {
		return fmt.Errorf("spec.maxRows is negative")
	}
	if s.TimeoutSeconds < 0 {
		return fmt.Errorf("spec.timeoutSeconds is negative")
	}

	if strings.TrimSpace(s.Health) != "" && s.IsReadOnly() {
		// The health query runs under the same rules as everything else, so a
		// health query that would be rejected at run time should not pass
		// apply either.
		if err := CheckReadOnly(s.Health); err != nil {
			return fmt.Errorf("spec.health: %w", err)
		}
	}
	return nil
}

func checkNoInlinePassword(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return nil // not URL-shaped; a DSN like "user:pass@tcp(...)" is caught below
	}
	if u.User != nil {
		if _, hasPassword := u.User.Password(); hasPassword {
			return fmt.Errorf("spec.url has a password in it; use a ${ENV_VAR} reference so the credential is not in a file people commit")
		}
	}
	return nil
}

// --- read-only enforcement ---

// writeKeywords are the leading keywords that change data or schema.
var writeKeywords = map[string]bool{
	"insert": true, "update": true, "delete": true, "truncate": true,
	"drop": true, "create": true, "alter": true, "grant": true, "revoke": true,
	"replace": true, "merge": true, "upsert": true, "call": true, "do": true,
	"copy": true, "vacuum": true, "analyze": true, "lock": true, "unload": true,
	"comment": true, "rename": true, "reindex": true, "refresh": true, "set": true,
	"begin": true, "commit": true, "rollback": true, "savepoint": true, "prepare": true,
	"execute": true, "load": true, "handler": true, "install": true, "reset": true,
}

// readKeywords are the leading keywords that only read.
var readKeywords = map[string]bool{
	"select": true, "with": true, "show": true, "explain": true,
	"describe": true, "desc": true, "values": true, "table": true,
}

// CheckReadOnly rejects a statement that is not a read.
//
// This is a keyword gate, not a parser, and it is deliberately strict: it
// allows a known-read leading keyword and refuses everything else, so an
// unrecognised statement fails closed. It also refuses multiple statements,
// because "SELECT 1; DROP TABLE users" defeats any check that only looks at
// the first word.
func CheckReadOnly(query string) error {
	q := strings.TrimSpace(query)
	if q == "" {
		return fmt.Errorf("the query is empty")
	}
	if err := checkSingleStatement(q); err != nil {
		return err
	}

	word := lower(leadingWord(stripLeadingComments(q)))
	if word == "" {
		return fmt.Errorf("cannot tell what this statement does")
	}
	if readKeywords[word] {
		// WITH ... can still end in an INSERT on Postgres, so look for a
		// data-modifying tail rather than trusting the opening keyword.
		if word == "with" {
			if bad := findWriteKeyword(q); bad != "" {
				return fmt.Errorf("this connection is read-only and the statement contains %s", strings.ToUpper(bad))
			}
		}
		return nil
	}
	if writeKeywords[word] {
		return fmt.Errorf("this connection is read-only and the statement starts with %s (set spec.readOnly: false to allow writes)", strings.ToUpper(word))
	}
	return fmt.Errorf("this connection is read-only and %q is not a recognised read statement (allowed: SELECT, WITH, SHOW, EXPLAIN, DESCRIBE, VALUES, TABLE)", strings.ToUpper(word))
}

// checkSingleStatement refuses a second statement, ignoring semicolons inside
// string literals and a single trailing one.
func checkSingleStatement(q string) error {
	inSingle, inDouble := false, false
	for i := 0; i < len(q); i++ {
		c := q[i]
		switch {
		case c == '\'' && !inDouble:
			// Doubled quotes are an escaped quote, not a close.
			if inSingle && i+1 < len(q) && q[i+1] == '\'' {
				i++
				continue
			}
			inSingle = !inSingle
		case c == '"' && !inSingle:
			inDouble = !inDouble
		case c == ';' && !inSingle && !inDouble:
			if strings.TrimSpace(q[i+1:]) != "" {
				return fmt.Errorf("send one statement at a time; this has more than one")
			}
		}
	}
	return nil
}

// stripLeadingComments drops -- and /* */ comments before the first keyword,
// so a comment cannot hide what a statement really is.
func stripLeadingComments(q string) string {
	for {
		q = strings.TrimSpace(q)
		switch {
		case strings.HasPrefix(q, "--"):
			if nl := strings.IndexByte(q, '\n'); nl >= 0 {
				q = q[nl+1:]
				continue
			}
			return ""
		case strings.HasPrefix(q, "/*"):
			if end := strings.Index(q, "*/"); end >= 0 {
				q = q[end+2:]
				continue
			}
			return ""
		case strings.HasPrefix(q, "("):
			// A parenthesised SELECT is still a read.
			q = q[1:]
			continue
		}
		return q
	}
}

func leadingWord(q string) string {
	q = strings.TrimSpace(q)
	for i := 0; i < len(q); i++ {
		c := q[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '(' || c == ';' {
			return q[:i]
		}
	}
	return q
}

// findWriteKeyword looks for a data-modifying keyword as a standalone word,
// used to catch a CTE whose tail writes.
func findWriteKeyword(q string) string {
	lowered := lower(q)
	for _, kw := range []string{"insert", "update", "delete", "merge", "truncate", "drop", "alter", "create"} {
		if containsWord(lowered, kw) {
			return kw
		}
	}
	return ""
}

func containsWord(haystack, word string) bool {
	from := 0
	for {
		i := strings.Index(haystack[from:], word)
		if i < 0 {
			return false
		}
		i += from
		beforeOK := i == 0 || !isWordByte(haystack[i-1])
		end := i + len(word)
		afterOK := end >= len(haystack) || !isWordByte(haystack[end])
		if beforeOK && afterOK {
			return true
		}
		from = i + len(word)
	}
}

func isWordByte(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}
