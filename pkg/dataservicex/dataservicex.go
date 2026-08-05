// Package dataservicex provides a small generic repository base on top of
// database/sql and pkg/xsql. It generates deterministic, identifier-safe
// PostgreSQL statements from `db` struct tags and always resolves its
// executor from the context, so operations transparently join transactions
// managed by xsql.ExecInTx.
package dataservicex

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"

	"github.com/acme/order-engine/pkg/xsql"
)

// ErrEntityNotFound reports that no row matched the requested identifier.
var ErrEntityNotFound = errors.New("dataservicex: entity not found")

// Entity describes a persisted struct. TableName and IDColumn must be
// value-receiver methods returning constant metadata.
type Entity interface {
	TableName() string
	IDColumn() string
}

// identifierPattern is the only shape accepted for table and column names.
// Everything matching it is safe to double-quote; schema qualification and
// raw SQL fragments are rejected by construction.
var identifierPattern = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// immutableMetadata is computed once per DataService and never mutated.
type immutableMetadata struct {
	table        string
	idColumn     string
	columns      []string
	fieldIndexes []int
	selectByID   string
	insert       string
}

// DataService is a generic repository base for one entity type.
type DataService[T Entity] struct {
	db   *sql.DB
	meta immutableMetadata
}

// NewDataService validates T's metadata and prepares the immutable SQL
// statements for it. T must be a non-pointer struct whose exported,
// db-tagged fields all carry valid PostgreSQL identifiers.
func NewDataService[T Entity](db *sql.DB) (*DataService[T], error) {
	if db == nil {
		return nil, errors.New("dataservicex: NewDataService requires a non-nil db")
	}

	typ := reflect.TypeFor[T]()
	if typ.Kind() != reflect.Struct {
		return nil, fmt.Errorf("dataservicex: entity type must be a non-pointer struct, got %s", typ)
	}

	var zero T
	table := zero.TableName()
	idColumn := zero.IDColumn()
	if !identifierPattern.MatchString(table) {
		return nil, fmt.Errorf("dataservicex: invalid table name %q", table)
	}
	if !identifierPattern.MatchString(idColumn) {
		return nil, fmt.Errorf("dataservicex: invalid id column %q", idColumn)
	}

	var (
		columns      []string
		fieldIndexes []int
	)
	seen := make(map[string]struct{})
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if !field.IsExported() {
			continue
		}
		tag, ok := field.Tag.Lookup("db")
		if !ok {
			continue
		}
		if tag == "-" {
			continue
		}
		if tag == "" {
			return nil, fmt.Errorf("dataservicex: field %s.%s has an empty db tag", typ, field.Name)
		}
		if !identifierPattern.MatchString(tag) {
			return nil, fmt.Errorf("dataservicex: field %s.%s has invalid column name %q", typ, field.Name, tag)
		}
		if _, dup := seen[tag]; dup {
			return nil, fmt.Errorf("dataservicex: duplicate db tag %q on %s", tag, typ)
		}
		seen[tag] = struct{}{}
		columns = append(columns, tag)
		fieldIndexes = append(fieldIndexes, i)
	}
	if len(columns) == 0 {
		return nil, fmt.Errorf("dataservicex: %s has no persisted fields", typ)
	}
	if _, ok := seen[idColumn]; !ok {
		return nil, fmt.Errorf("dataservicex: id column %q is not a persisted field of %s", idColumn, typ)
	}

	quoted := make([]string, len(columns))
	placeholders := make([]string, len(columns))
	for i, column := range columns {
		quoted[i] = quoteIdentifier(column)
		placeholders[i] = fmt.Sprintf("$%d", i+1)
	}

	meta := immutableMetadata{
		table:        table,
		idColumn:     idColumn,
		columns:      columns,
		fieldIndexes: fieldIndexes,
		selectByID: fmt.Sprintf(
			"SELECT %s FROM %s WHERE %s = $1 LIMIT 1",
			strings.Join(quoted, ", "), quoteIdentifier(table), quoteIdentifier(idColumn),
		),
		insert: fmt.Sprintf(
			"INSERT INTO %s (%s) VALUES (%s)",
			quoteIdentifier(table), strings.Join(quoted, ", "), strings.Join(placeholders, ", "),
		),
	}
	return &DataService[T]{db: db, meta: meta}, nil
}

// FindByID loads the entity whose ID column equals id. It returns
// ErrEntityNotFound when no row matches.
func (ds *DataService[T]) FindByID(ctx context.Context, id any) (*T, error) {
	exec := xsql.GetExecutor(ctx, ds.db)
	entity, err := xsql.QuerySingle[T](ctx, exec, ds.meta.selectByID, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("dataservicex: find %s by id: %w", ds.meta.table, ErrEntityNotFound)
		}
		return nil, fmt.Errorf("dataservicex: find %s by id: %w", ds.meta.table, err)
	}
	return entity, nil
}

// Insert persists entity. All persisted values, including the ID and any
// timestamps, must be populated by the application beforehand; DB-generated
// fields are not supported by this contract.
func (ds *DataService[T]) Insert(ctx context.Context, entity *T) error {
	if entity == nil {
		return errors.New("dataservicex: Insert requires a non-nil entity")
	}

	value := reflect.ValueOf(entity).Elem()
	args := make([]any, len(ds.meta.fieldIndexes))
	for i, fieldIndex := range ds.meta.fieldIndexes {
		args[i] = value.Field(fieldIndex).Interface()
	}

	exec := xsql.GetExecutor(ctx, ds.db)
	result, err := exec.ExecContext(ctx, ds.meta.insert, args...)
	if err != nil {
		return fmt.Errorf("dataservicex: insert %s: %w", ds.meta.table, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("dataservicex: insert %s: rows affected: %w", ds.meta.table, err)
	}
	if affected != 1 {
		return fmt.Errorf("dataservicex: insert %s: affected %d rows, want 1", ds.meta.table, affected)
	}
	return nil
}

func quoteIdentifier(name string) string {
	// identifierPattern excludes quotes entirely, so plain wrapping is safe.
	return `"` + name + `"`
}
