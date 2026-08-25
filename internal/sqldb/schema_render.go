package sqldb

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// SQL renders a schema as DDL.
//
// DDL rather than a table listing, for two reasons a model cares about: it is
// the form every model has read a million examples of, and it greps well.
// `grep -n 'REFERENCES users' docs/schema` answers "what points at users"
// without opening a connection.
func (s *Schema) SQL() []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "-- %s", s.Connection)
	if s.Version != "" {
		fmt.Fprintf(&b, " (%s)", s.Version)
	}
	fmt.Fprintf(&b, "\n-- dumped by drover at %s\n", s.DumpedAt.Format(time.RFC3339))
	b.WriteString("-- Row counts are ESTIMATES from the database's own statistics. They are\n")
	b.WriteString("-- good enough to choose a join order or refuse a SELECT *, and wrong\n")
	b.WriteString("-- enough that you should not report one as a fact.\n")
	if len(s.Tables) == 0 {
		b.WriteString("\n-- no tables visible to these credentials\n")
		return []byte(b.String())
	}
	if s.Truncated {
		fmt.Fprintf(&b, "-- TRUNCATED at %d tables; narrow it with spec.schemas.\n", maxTables)
	}

	for _, t := range s.Tables {
		b.WriteString("\n")
		keyword := "CREATE TABLE"
		if t.Kind == "view" {
			keyword = "CREATE VIEW"
		}
		fmt.Fprintf(&b, "%s %s (\n", keyword, t.Qualified())

		width := 0
		for _, c := range t.Columns {
			if len(c.Name) > width {
				width = len(c.Name)
			}
		}
		for i, c := range t.Columns {
			fmt.Fprintf(&b, "  %-*s  %s", width, c.Name, c.Type)
			if !c.Nullable {
				b.WriteString(" NOT NULL")
			}
			if c.Default != "" {
				fmt.Fprintf(&b, " DEFAULT %s", collapse(c.Default))
			}
			if c.References != "" {
				fmt.Fprintf(&b, " REFERENCES %s", c.References)
			}
			if i < len(t.Columns)-1 {
				b.WriteString(",")
			}
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, ");  -- ~%s rows\n", humanRows(t.Rows))
		for _, idx := range t.Indexes {
			fmt.Fprintf(&b, "%s;\n", strings.TrimSuffix(strings.TrimSpace(idx.Definition), ";"))
		}
	}
	return []byte(b.String())
}

// collapse keeps a default expression on one line. A postgres default can be
// a whole function body, and a DDL line that wraps stops being greppable.
func collapse(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 80 {
		return s[:77] + "..."
	}
	return s
}

// humanRows groups digits, because 48200000 and 4820000 are indistinguishable
// at a glance and the difference decides whether a query is safe to run.
func humanRows(n int64) string {
	s := strconv.FormatInt(n, 10)
	if len(s) <= 3 {
		return s
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}

// Diff reports whether two dumps describe the same shape, ignoring the
// timestamp and the row estimates -- which change constantly and are not what
// anyone means by "the schema changed".
func (s *Schema) Diff(other *Schema) bool {
	if other == nil {
		return true
	}
	return !strings.EqualFold(s.shape(), other.shape())
}

func (s *Schema) shape() string {
	var b strings.Builder
	for _, t := range s.Tables {
		fmt.Fprintf(&b, "%s|%s\n", t.Kind, t.Qualified())
		for _, c := range t.Columns {
			fmt.Fprintf(&b, "  %s|%s|%v|%s\n", c.Name, c.Type, c.Nullable, c.References)
		}
		for _, i := range t.Indexes {
			fmt.Fprintf(&b, "  idx %s\n", i.Name)
		}
	}
	return b.String()
}
