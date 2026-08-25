package sqldb

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/notshekhar/drover/internal/object"
)

// mysqlSystemSchemas are the catalogs a dump walks past.
var mysqlSystemSchemas = []string{"information_schema", "performance_schema", "mysql", "sys"}

// dumpMySQL reads shape from information_schema.
//
// MySQL calls a schema a database, so "schema" here means what `USE` selects.
// TABLE_ROWS is an estimate on InnoDB and can be out by a factor of two; it
// is reported as an estimate for exactly that reason.
func dumpMySQL(ctx context.Context, db *sql.DB, spec *object.SQLConnectionSpec, out *Schema) error {
	filter, args := schemaFilter(spec, "TABLE_SCHEMA", mysqlSystemSchemas)

	index := map[string]*Table{}
	rows, err := db.QueryContext(ctx, `
		SELECT TABLE_SCHEMA, TABLE_NAME,
		       CASE WHEN TABLE_TYPE = 'VIEW' THEN 'view' ELSE 'table' END,
		       COALESCE(TABLE_ROWS, 0)
		  FROM information_schema.TABLES
		 WHERE `+filter+`
		 ORDER BY TABLE_SCHEMA, TABLE_NAME`, args...)
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

	crows, err := db.QueryContext(ctx, `
		SELECT TABLE_SCHEMA, TABLE_NAME, COLUMN_NAME, COLUMN_TYPE, IS_NULLABLE, COALESCE(COLUMN_DEFAULT, '')
		  FROM information_schema.COLUMNS
		 WHERE `+filter+`
		 ORDER BY TABLE_SCHEMA, TABLE_NAME, ORDINAL_POSITION`, args...)
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

	frows, err := db.QueryContext(ctx, `
		SELECT TABLE_SCHEMA, TABLE_NAME, COLUMN_NAME, REFERENCED_TABLE_NAME, REFERENCED_COLUMN_NAME
		  FROM information_schema.KEY_COLUMN_USAGE
		 WHERE REFERENCED_TABLE_NAME IS NOT NULL AND `+filter, args...)
	if err == nil {
		for frows.Next() {
			var schema, table, column, target, targetColumn string
			if err := frows.Scan(&schema, &table, &column, &target, &targetColumn); err != nil {
				break
			}
			t := index[schema+"."+table]
			if t == nil {
				continue
			}
			for i := range t.Columns {
				if t.Columns[i].Name == column {
					t.Columns[i].References = target + "(" + targetColumn + ")"
				}
			}
		}
		frows.Close()
	}

	irows, err := db.QueryContext(ctx, `
		SELECT TABLE_SCHEMA, TABLE_NAME, INDEX_NAME,
		       GROUP_CONCAT(COLUMN_NAME ORDER BY SEQ_IN_INDEX), MAX(NON_UNIQUE)
		  FROM information_schema.STATISTICS
		 WHERE `+filter+`
		 GROUP BY TABLE_SCHEMA, TABLE_NAME, INDEX_NAME`, args...)
	if err == nil {
		for irows.Next() {
			var schema, table, name, cols string
			var nonUnique int
			if err := irows.Scan(&schema, &table, &name, &cols, &nonUnique); err != nil {
				break
			}
			t := index[schema+"."+table]
			if t == nil {
				continue
			}
			def := "INDEX " + name + " (" + cols + ")"
			if nonUnique == 0 {
				def = "UNIQUE " + def
			}
			t.Indexes = append(t.Indexes, Index{Name: name, Definition: def})
		}
		irows.Close()
	}
	return nil
}
