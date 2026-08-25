package sqldb

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/notshekhar/drover/internal/object"
)

// pgSystemSchemas are the catalogs a dump walks past. Redshift adds its own.
var pgSystemSchemas = []string{"pg_catalog", "information_schema", "pg_toast", "pg_internal", "catalog_history"}

// dumpPostgres reads shape from the catalogs, in four queries rather than one
// per table.
//
// Four round trips against a database with 900 tables, not 900. The joins are
// wide but the catalogs are small, and a per-table walk is how a schema dump
// turns into a five-minute stall on a real warehouse.
func dumpPostgres(ctx context.Context, db *sql.DB, spec *object.SQLConnectionSpec, out *Schema) error {
	filter, args := schemaFilter(spec, "n.nspname", pgSystemSchemas)

	// reltuples is an estimate maintained by ANALYZE, and is reported as one.
	// The exact count is a sequential scan of every table in the database,
	// which is not something a background dump gets to do.
	tableQuery := numbered(`
		SELECT n.nspname, c.relname,
		       CASE c.relkind WHEN 'v' THEN 'view' WHEN 'm' THEN 'view' ELSE 'table' END,
		       GREATEST(c.reltuples, 0)::bigint
		  FROM pg_class c
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE c.relkind IN ('r','p','v','m') AND ` + filter + `
		 ORDER BY n.nspname, c.relname`)

	index := map[string]*Table{}
	rows, err := db.QueryContext(ctx, tableQuery, args...)
	if err != nil {
		return fmt.Errorf("read tables: %w", err)
	}
	for rows.Next() {
		var t Table
		if err := rows.Scan(&t.Schema, &t.Name, &t.Kind, &t.Rows); err != nil {
			rows.Close()
			return err
		}
		out.Tables = append(out.Tables, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range out.Tables {
		index[out.Tables[i].Schema+"."+out.Tables[i].Name] = &out.Tables[i]
	}
	if len(out.Tables) == 0 {
		return nil
	}

	colQuery := numbered(`
		SELECT c.table_schema, c.table_name, c.column_name,
		       COALESCE(c.data_type, ''), c.is_nullable, COALESCE(c.column_default, '')
		  FROM information_schema.columns c
		  JOIN pg_namespace n ON n.nspname = c.table_schema
		 WHERE ` + filter + `
		 ORDER BY c.table_schema, c.table_name, c.ordinal_position`)
	crows, err := db.QueryContext(ctx, colQuery, args...)
	if err != nil {
		return fmt.Errorf("read columns: %w", err)
	}
	for crows.Next() {
		var schema, table, nullable string
		var col Column
		if err := crows.Scan(&schema, &table, &col.Name, &col.Type, &nullable, &col.Default); err != nil {
			crows.Close()
			return err
		}
		col.Nullable = nullable == "YES"
		if t := index[schema+"."+table]; t != nil {
			t.Columns = append(t.Columns, col)
		}
	}
	crows.Close()
	if err := crows.Err(); err != nil {
		return err
	}

	// Foreign keys are the most valuable thing in a schema dump and the thing
	// a model most often guesses wrong: `grep 'REFERENCES users' docs/schema`
	// answers "what points at users" without a query.
	fkQuery := numbered(`
		SELECT n.nspname, c.relname, a.attname, tn.nspname, tc.relname, ta.attname
		  FROM pg_constraint con
		  JOIN pg_class c        ON c.oid = con.conrelid
		  JOIN pg_namespace n    ON n.oid = c.relnamespace
		  JOIN pg_class tc       ON tc.oid = con.confrelid
		  JOIN pg_namespace tn   ON tn.oid = tc.relnamespace
		  JOIN unnest(con.conkey)  WITH ORDINALITY AS k(attnum, ord)  ON true
		  JOIN unnest(con.confkey) WITH ORDINALITY AS fk(attnum, ord) ON fk.ord = k.ord
		  JOIN pg_attribute a  ON a.attrelid = c.oid  AND a.attnum = k.attnum
		  JOIN pg_attribute ta ON ta.attrelid = tc.oid AND ta.attnum = fk.attnum
		 WHERE con.contype = 'f' AND ` + filter)
	frows, err := db.QueryContext(ctx, fkQuery, args...)
	if err == nil {
		for frows.Next() {
			var schema, table, column, targetSchema, targetTable, targetColumn string
			if err := frows.Scan(&schema, &table, &column, &targetSchema, &targetTable, &targetColumn); err != nil {
				break
			}
			t := index[schema+"."+table]
			if t == nil {
				continue
			}
			ref := targetTable + "(" + targetColumn + ")"
			if targetSchema != "" && targetSchema != "public" {
				ref = targetSchema + "." + ref
			}
			for i := range t.Columns {
				if t.Columns[i].Name == column {
					t.Columns[i].References = ref
				}
			}
		}
		frows.Close()
	}

	// Redshift has no pg_indexes, so a failure here is not fatal: the dump is
	// still worth having without them.
	idxQuery := numbered(`SELECT schemaname, tablename, indexname, indexdef FROM pg_indexes WHERE ` +
		strings.ReplaceAll(filter, "n.nspname", "schemaname"))
	irows, err := db.QueryContext(ctx, idxQuery, args...)
	if err == nil {
		for irows.Next() {
			var schema, table string
			var idx Index
			if err := irows.Scan(&schema, &table, &idx.Name, &idx.Definition); err != nil {
				break
			}
			if t := index[schema+"."+table]; t != nil {
				t.Indexes = append(t.Indexes, idx)
			}
		}
		irows.Close()
	}
	return nil
}
