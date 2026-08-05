package xsql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
)

// planKey identifies a cached scan plan: one struct type combined with one
// exact ordered result-column list. Keying on the type alone would let two
// queries over the same struct with different column sets collide.
type planKey struct {
	typ  reflect.Type
	cols string
}

var (
	// fieldCache caches per-type `db` tag metadata: tag -> field index.
	fieldCache sync.Map // reflect.Type -> map[string]int
	// planCache caches validated scan plans: result column i -> field index.
	planCache sync.Map // planKey -> []int
)

// QuerySingle executes query with args on exec and scans the first result row
// into a freshly allocated T, matching result columns to exported struct
// fields by their `db:"column"` tag (exact, case-sensitive). Fields tagged
// `db:"-"` or carrying no tag are ignored; fields whose column is not part of
// the result keep their zero value.
//
// It returns an error wrapping sql.ErrNoRows when the query yields no rows,
// and rejects duplicate tags on T, duplicate result columns, and result
// columns that map to no field. Only the first row is read: callers must use
// a unique predicate or LIMIT 1.
func QuerySingle[T any](ctx context.Context, exec SQLExecutor, query string, args ...any) (*T, error) {
	typ := reflect.TypeOf((*T)(nil)).Elem()
	if typ.Kind() != reflect.Struct {
		return nil, fmt.Errorf("xsql: QuerySingle requires a non-pointer struct type parameter, got %s", typ)
	}
	if exec == nil {
		return nil, errors.New("xsql: QuerySingle requires a non-nil executor")
	}

	rows, err := exec.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("xsql: query: %w", err)
	}
	// Guarantees release on every path (including panics); the explicit Close
	// below still surfaces the close error, which a deferred call would drop.
	defer rows.Close()

	result, scanErr := scanFirstRow[T](typ, rows)
	closeErr := rows.Close()
	if scanErr != nil {
		return nil, scanErr
	}
	if closeErr != nil {
		return nil, fmt.Errorf("xsql: close rows: %w", closeErr)
	}
	return result, nil
}

func scanFirstRow[T any](typ reflect.Type, rows *sql.Rows) (*T, error) {
	columns, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("xsql: read result columns: %w", err)
	}

	plan, err := scanPlan(typ, columns)
	if err != nil {
		return nil, err
	}

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("xsql: iterate rows: %w", err)
		}
		return nil, fmt.Errorf("xsql: no rows: %w", sql.ErrNoRows)
	}

	out := new(T)
	value := reflect.ValueOf(out).Elem()
	dest := make([]any, len(plan))
	for i, fieldIndex := range plan {
		// Scanning into the field address lets database/sql handle
		// primitives, pointers, time.Time, sql.Null* and sql.Scanner.
		dest[i] = value.Field(fieldIndex).Addr().Interface()
	}
	if err := rows.Scan(dest...); err != nil {
		return nil, fmt.Errorf("xsql: scan row: %w", err)
	}
	return out, nil
}

// scanPlan maps each result column, in order, to a field index of typ.
// Valid plans are cached; invalid combinations are rejected on every call.
func scanPlan(typ reflect.Type, columns []string) ([]int, error) {
	key := planKey{typ: typ, cols: strings.Join(columns, "\x00")}
	if cached, ok := planCache.Load(key); ok {
		return cached.([]int), nil
	}

	fields, err := fieldIndexesByTag(typ)
	if err != nil {
		return nil, err
	}

	plan := make([]int, len(columns))
	seen := make(map[string]struct{}, len(columns))
	for i, column := range columns {
		if _, dup := seen[column]; dup {
			return nil, fmt.Errorf("xsql: duplicate column %q in result set", column)
		}
		seen[column] = struct{}{}
		fieldIndex, ok := fields[column]
		if !ok {
			return nil, fmt.Errorf("xsql: result column %q has no field with matching db tag on %s", column, typ)
		}
		plan[i] = fieldIndex
	}

	planCache.Store(key, plan)
	return plan, nil
}

// fieldIndexesByTag returns the db tag -> field index mapping for typ,
// considering exported fields with a non-empty tag other than "-".
func fieldIndexesByTag(typ reflect.Type) (map[string]int, error) {
	if cached, ok := fieldCache.Load(typ); ok {
		return cached.(map[string]int), nil
	}

	fields := make(map[string]int)
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if !field.IsExported() {
			continue
		}
		tag, ok := field.Tag.Lookup("db")
		if !ok || tag == "" || tag == "-" {
			continue
		}
		if _, dup := fields[tag]; dup {
			return nil, fmt.Errorf("xsql: duplicate db tag %q on %s", tag, typ)
		}
		fields[tag] = i
	}

	fieldCache.Store(typ, fields)
	return fields, nil
}
