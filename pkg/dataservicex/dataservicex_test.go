package dataservicex

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"github.com/acme/order-engine/pkg/xsql"
)

type widget struct {
	ID     string `db:"id"`
	Name   string `db:"name"`
	Count  int64  `db:"count"`
	Hidden string `db:"-"`
	NoTag  string
}

func (widget) TableName() string { return "widgets" }
func (widget) IDColumn() string  { return "id" }

type badTableEntity struct {
	ID string `db:"id"`
}

func (badTableEntity) TableName() string { return `widgets"; DROP TABLE widgets; --` }
func (badTableEntity) IDColumn() string  { return "id" }

type badColumnEntity struct {
	ID string `db:"id"`
	V  string `db:"value; DROP"`
}

func (badColumnEntity) TableName() string { return "widgets" }
func (badColumnEntity) IDColumn() string  { return "id" }

type badIDColumnEntity struct {
	ID string `db:"id"`
}

func (badIDColumnEntity) TableName() string { return "widgets" }
func (badIDColumnEntity) IDColumn() string  { return `id" OR "1` }

type missingIDEntity struct {
	Name string `db:"name"`
}

func (missingIDEntity) TableName() string { return "widgets" }
func (missingIDEntity) IDColumn() string  { return "id" }

type emptyTagEntity struct {
	ID string `db:"id"`
	V  string `db:""`
}

func (emptyTagEntity) TableName() string { return "widgets" }
func (emptyTagEntity) IDColumn() string  { return "id" }

type dupTagEntity struct {
	A string `db:"id"`
	B string `db:"id"`
}

func (dupTagEntity) TableName() string { return "widgets" }
func (dupTagEntity) IDColumn() string  { return "id" }

type noFieldsEntity struct {
	Ignored string `db:"-"`
}

func (noFieldsEntity) TableName() string { return "widgets" }
func (noFieldsEntity) IDColumn() string  { return "id" }

const (
	wantSelect = `SELECT "id", "name", "count" FROM "widgets" WHERE "id" = $1 LIMIT 1`
	wantInsert = `INSERT INTO "widgets" ("id", "name", "count") VALUES ($1, $2, $3)`
)

func newExactMockDB(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, mock
}

func expectationsMet(t *testing.T, mock sqlmock.Sqlmock) {
	t.Helper()
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestNewDataServiceRejectsInvalidConfigurations(t *testing.T) {
	db, _ := newExactMockDB(t)

	if _, err := NewDataService[widget](nil); err == nil {
		t.Error("nil db: want error")
	}
	if _, err := NewDataService[*widget](db); err == nil || !strings.Contains(err.Error(), "non-pointer struct") {
		t.Errorf("pointer entity type: want non-pointer struct error, got %v", err)
	}
	if _, err := NewDataService[badTableEntity](db); err == nil || !strings.Contains(err.Error(), "invalid table name") {
		t.Errorf("bad table: want invalid table name error, got %v", err)
	}
	if _, err := NewDataService[badColumnEntity](db); err == nil || !strings.Contains(err.Error(), "invalid column name") {
		t.Errorf("bad column: want invalid column name error, got %v", err)
	}
	if _, err := NewDataService[badIDColumnEntity](db); err == nil || !strings.Contains(err.Error(), "invalid id column") {
		t.Errorf("bad id column: want invalid id column error, got %v", err)
	}
	if _, err := NewDataService[missingIDEntity](db); err == nil || !strings.Contains(err.Error(), "not a persisted field") {
		t.Errorf("missing id field: want not-persisted error, got %v", err)
	}
	if _, err := NewDataService[emptyTagEntity](db); err == nil || !strings.Contains(err.Error(), "empty db tag") {
		t.Errorf("empty tag: want empty tag error, got %v", err)
	}
	if _, err := NewDataService[dupTagEntity](db); err == nil || !strings.Contains(err.Error(), "duplicate db tag") {
		t.Errorf("duplicate tag: want duplicate tag error, got %v", err)
	}
	if _, err := NewDataService[noFieldsEntity](db); err == nil || !strings.Contains(err.Error(), "no persisted fields") {
		t.Errorf("no fields: want no persisted fields error, got %v", err)
	}
}

func TestFindByIDUsesExplicitDeterministicSelect(t *testing.T) {
	db, mock := newExactMockDB(t)
	ds, err := NewDataService[widget](db)
	if err != nil {
		t.Fatalf("NewDataService: %v", err)
	}

	// QueryMatcherEqual makes this an exact-SQL assertion: no SELECT *, quoted
	// identifiers, declaration-order columns, LIMIT 1.
	mock.ExpectQuery(wantSelect).WithArgs("w-1").WillReturnRows(
		sqlmock.NewRows([]string{"id", "name", "count"}).AddRow("w-1", "gadget", int64(3)),
	)

	got, err := ds.FindByID(context.Background(), "w-1")
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if got.ID != "w-1" || got.Name != "gadget" || got.Count != 3 {
		t.Errorf("FindByID = %+v, want w-1/gadget/3", got)
	}
	expectationsMet(t, mock)
}

func TestFindByIDMapsNoRowsToErrEntityNotFound(t *testing.T) {
	db, mock := newExactMockDB(t)
	ds, err := NewDataService[widget](db)
	if err != nil {
		t.Fatalf("NewDataService: %v", err)
	}
	mock.ExpectQuery(wantSelect).WithArgs("missing").WillReturnRows(
		sqlmock.NewRows([]string{"id", "name", "count"}),
	)

	_, err = ds.FindByID(context.Background(), "missing")
	if !errors.Is(err, ErrEntityNotFound) {
		t.Fatalf("FindByID error = %v, want ErrEntityNotFound", err)
	}
	if errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("FindByID must translate sql.ErrNoRows, got %v", err)
	}
	expectationsMet(t, mock)
}

func TestFindByIDPassesThroughOtherErrors(t *testing.T) {
	db, mock := newExactMockDB(t)
	ds, err := NewDataService[widget](db)
	if err != nil {
		t.Fatalf("NewDataService: %v", err)
	}
	queryErr := errors.New("connection refused")
	mock.ExpectQuery(wantSelect).WithArgs("w-1").WillReturnError(queryErr)

	_, err = ds.FindByID(context.Background(), "w-1")
	if !errors.Is(err, queryErr) {
		t.Fatalf("FindByID error = %v, want unwrappable to query error", err)
	}
	if errors.Is(err, ErrEntityNotFound) {
		t.Fatalf("infrastructure errors must not map to ErrEntityNotFound: %v", err)
	}
	expectationsMet(t, mock)
}

func TestInsertUsesDeterministicStatementAndArgs(t *testing.T) {
	db, mock := newExactMockDB(t)
	ds, err := NewDataService[widget](db)
	if err != nil {
		t.Fatalf("NewDataService: %v", err)
	}
	mock.ExpectExec(wantInsert).WithArgs("w-9", "sprocket", int64(5)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	err = ds.Insert(context.Background(), &widget{ID: "w-9", Name: "sprocket", Count: 5, Hidden: "x", NoTag: "y"})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	expectationsMet(t, mock)
}

func TestInsertRejectsNilEntity(t *testing.T) {
	db, _ := newExactMockDB(t)
	ds, err := NewDataService[widget](db)
	if err != nil {
		t.Fatalf("NewDataService: %v", err)
	}
	if err := ds.Insert(context.Background(), nil); err == nil {
		t.Fatal("Insert(nil): want error")
	}
}

func TestInsertRequiresExactlyOneAffectedRow(t *testing.T) {
	db, mock := newExactMockDB(t)
	ds, err := NewDataService[widget](db)
	if err != nil {
		t.Fatalf("NewDataService: %v", err)
	}
	mock.ExpectExec(wantInsert).WithArgs("w-9", "s", int64(1)).
		WillReturnResult(sqlmock.NewResult(0, 0))

	err = ds.Insert(context.Background(), &widget{ID: "w-9", Name: "s", Count: 1})
	if err == nil || !strings.Contains(err.Error(), "affected 0 rows") {
		t.Fatalf("Insert error = %v, want affected-rows failure", err)
	}
	expectationsMet(t, mock)
}

func TestInsertWrapsExecErrors(t *testing.T) {
	db, mock := newExactMockDB(t)
	ds, err := NewDataService[widget](db)
	if err != nil {
		t.Fatalf("NewDataService: %v", err)
	}
	execErr := errors.New("unique violation")
	mock.ExpectExec(wantInsert).WithArgs("w-9", "s", int64(1)).WillReturnError(execErr)

	err = ds.Insert(context.Background(), &widget{ID: "w-9", Name: "s", Count: 1})
	if !errors.Is(err, execErr) {
		t.Fatalf("Insert error = %v, want unwrappable to exec error", err)
	}
	expectationsMet(t, mock)
}

// TestOperationsJoinContextTransaction asserts both operations resolve their
// executor from the context: with ordered expectations, the select and insert
// must happen between Begin and Commit of the xsql-managed transaction. The
// definitive proof against a real database lives in the repository
// integration tests.
func TestOperationsJoinContextTransaction(t *testing.T) {
	db, mock := newExactMockDB(t)
	ds, err := NewDataService[widget](db)
	if err != nil {
		t.Fatalf("NewDataService: %v", err)
	}

	mock.ExpectBegin()
	mock.ExpectQuery(wantSelect).WithArgs("w-1").WillReturnRows(
		sqlmock.NewRows([]string{"id", "name", "count"}).AddRow("w-1", "gadget", int64(3)),
	)
	mock.ExpectExec(wantInsert).WithArgs("w-2", "copy", int64(3)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	err = xsql.ExecInTx(context.Background(), db, func(txCtx context.Context) error {
		found, err := ds.FindByID(txCtx, "w-1")
		if err != nil {
			return err
		}
		return ds.Insert(txCtx, &widget{ID: "w-2", Name: "copy", Count: found.Count})
	})
	if err != nil {
		t.Fatalf("ExecInTx: %v", err)
	}
	expectationsMet(t, mock)
}
