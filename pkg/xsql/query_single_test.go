package xsql

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

type accountRow struct {
	ID        int64          `db:"id"`
	Name      string         `db:"name"`
	Email     sql.NullString `db:"email"`
	CreatedAt time.Time      `db:"created_at"`
	Internal  string         `db:"-"`
	Untagged  string
}

type duplicateTagRow struct {
	A int64 `db:"id"`
	B int64 `db:"id"`
}

func TestQuerySingleScansReorderedColumns(t *testing.T) {
	db, mock := newMockDB(t)
	created := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	mock.ExpectQuery("SELECT .+ FROM accounts").WillReturnRows(
		sqlmock.NewRows([]string{"name", "created_at", "id"}).AddRow("alice", created, int64(7)),
	)

	got, err := QuerySingle[accountRow](context.Background(), db, "SELECT name, created_at, id FROM accounts")
	if err != nil {
		t.Fatalf("QuerySingle: %v", err)
	}
	if got.ID != 7 || got.Name != "alice" || !got.CreatedAt.Equal(created) {
		t.Errorf("scanned %+v, want id=7 name=alice created_at=%v", got, created)
	}
	if got.Email.Valid || got.Internal != "" || got.Untagged != "" {
		t.Errorf("fields outside the result set must stay zero, got %+v", got)
	}
	expectationsMet(t, mock)
}

func TestQuerySingleScansNullableAndScannerFields(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectQuery("SELECT .+").WillReturnRows(
		sqlmock.NewRows([]string{"id", "email"}).AddRow(int64(1), "a@example.com"),
	)
	withEmail, err := QuerySingle[accountRow](context.Background(), db, "SELECT id, email FROM accounts")
	if err != nil {
		t.Fatalf("QuerySingle: %v", err)
	}
	if !withEmail.Email.Valid || withEmail.Email.String != "a@example.com" {
		t.Errorf("email = %+v, want valid a@example.com", withEmail.Email)
	}

	mock.ExpectQuery("SELECT .+").WillReturnRows(
		sqlmock.NewRows([]string{"id", "email"}).AddRow(int64(2), nil),
	)
	withNull, err := QuerySingle[accountRow](context.Background(), db, "SELECT id, email FROM accounts")
	if err != nil {
		t.Fatalf("QuerySingle: %v", err)
	}
	if withNull.Email.Valid {
		t.Errorf("email = %+v, want NULL", withNull.Email)
	}
	expectationsMet(t, mock)
}

func TestQuerySingleReturnsErrNoRows(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectQuery("SELECT .+").WillReturnRows(sqlmock.NewRows([]string{"id"}))

	_, err := QuerySingle[accountRow](context.Background(), db, "SELECT id FROM accounts")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("QuerySingle error = %v, want unwrappable to sql.ErrNoRows", err)
	}
	expectationsMet(t, mock)
}

func TestQuerySingleReportsIterationErrorInsteadOfNoRows(t *testing.T) {
	db, mock := newMockDB(t)
	iterErr := errors.New("connection torn down")
	mock.ExpectQuery("SELECT .+").WillReturnRows(
		sqlmock.NewRows([]string{"id"}).AddRow(int64(1)).RowError(0, iterErr),
	)

	_, err := QuerySingle[accountRow](context.Background(), db, "SELECT id FROM accounts")
	if errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("iteration failure must not masquerade as sql.ErrNoRows: %v", err)
	}
	if !errors.Is(err, iterErr) {
		t.Fatalf("QuerySingle error = %v, want unwrappable to iteration error", err)
	}
	expectationsMet(t, mock)
}

func TestQuerySingleSurfacesCloseError(t *testing.T) {
	db, mock := newMockDB(t)
	closeErr := errors.New("close exploded")
	mock.ExpectQuery("SELECT .+").WillReturnRows(
		sqlmock.NewRows([]string{"id"}).AddRow(int64(1)).CloseError(closeErr),
	)

	_, err := QuerySingle[accountRow](context.Background(), db, "SELECT id FROM accounts")
	if !errors.Is(err, closeErr) {
		t.Fatalf("QuerySingle error = %v, want unwrappable to close error", err)
	}
	expectationsMet(t, mock)
}

func TestQuerySingleRejectsQueryError(t *testing.T) {
	db, mock := newMockDB(t)
	queryErr := errors.New("syntax error")
	mock.ExpectQuery("SELECT .+").WillReturnError(queryErr)

	_, err := QuerySingle[accountRow](context.Background(), db, "SELECT nope FROM accounts")
	if !errors.Is(err, queryErr) {
		t.Fatalf("QuerySingle error = %v, want unwrappable to query error", err)
	}
	expectationsMet(t, mock)
}

func TestQuerySingleRejectsDuplicateResultColumn(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectQuery("SELECT .+").WillReturnRows(
		sqlmock.NewRows([]string{"id", "id"}).AddRow(int64(1), int64(1)),
	)

	_, err := QuerySingle[accountRow](context.Background(), db, "SELECT id, id FROM accounts")
	if err == nil || !containsAll(err.Error(), "duplicate column") {
		t.Fatalf("QuerySingle error = %v, want duplicate-column rejection", err)
	}
	expectationsMet(t, mock)
}

func TestQuerySingleRejectsUnmappedColumn(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectQuery("SELECT .+").WillReturnRows(
		sqlmock.NewRows([]string{"id", "mystery"}).AddRow(int64(1), "x"),
	)

	_, err := QuerySingle[accountRow](context.Background(), db, "SELECT id, mystery FROM accounts")
	if err == nil || !containsAll(err.Error(), "mystery", "no field") {
		t.Fatalf("QuerySingle error = %v, want unmapped-column rejection", err)
	}
	expectationsMet(t, mock)
}

func TestQuerySingleRejectsDuplicateTags(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectQuery("SELECT .+").WillReturnRows(
		sqlmock.NewRows([]string{"id"}).AddRow(int64(1)),
	)

	_, err := QuerySingle[duplicateTagRow](context.Background(), db, "SELECT id FROM t")
	if err == nil || !containsAll(err.Error(), "duplicate db tag") {
		t.Fatalf("QuerySingle error = %v, want duplicate-tag rejection", err)
	}
}

func TestQuerySingleRejectsInvalidTypeParameter(t *testing.T) {
	db, _ := newMockDB(t)
	if _, err := QuerySingle[int](context.Background(), db, "SELECT 1"); err == nil {
		t.Error("QuerySingle[int]: want error, got nil")
	}
	if _, err := QuerySingle[*accountRow](context.Background(), db, "SELECT 1"); err == nil {
		t.Error("QuerySingle[*accountRow]: want error, got nil")
	}
	if _, err := QuerySingle[accountRow](context.Background(), nil, "SELECT 1"); err == nil {
		t.Error("QuerySingle with nil executor: want error, got nil")
	}
}

func TestQuerySingleReportsScanError(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectQuery("SELECT .+").WillReturnRows(
		sqlmock.NewRows([]string{"id"}).AddRow("not-a-number"),
	)

	_, err := QuerySingle[accountRow](context.Background(), db, "SELECT id FROM accounts")
	if err == nil || !containsAll(err.Error(), "scan") {
		t.Fatalf("QuerySingle error = %v, want scan failure", err)
	}
	expectationsMet(t, mock)
}

func TestQuerySingleReadsOnlyFirstRow(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectQuery("SELECT .+").WillReturnRows(
		sqlmock.NewRows([]string{"id"}).AddRow(int64(1)).AddRow(int64(2)),
	)

	got, err := QuerySingle[accountRow](context.Background(), db, "SELECT id FROM accounts LIMIT 1")
	if err != nil {
		t.Fatalf("QuerySingle: %v", err)
	}
	if got.ID != 1 {
		t.Errorf("ID = %d, want the first row's value 1", got.ID)
	}
	expectationsMet(t, mock)
}

// TestQuerySingleConcurrentPlanCache exercises the sync.Map metadata caches
// under the race detector.
func TestQuerySingleConcurrentPlanCache(t *testing.T) {
	db, mock := newMockDB(t)
	mock.MatchExpectationsInOrder(false)
	const workers = 32
	for range workers {
		mock.ExpectQuery("SELECT .+").WillReturnRows(
			sqlmock.NewRows([]string{"id", "name"}).AddRow(int64(9), "bob"),
		)
	}

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := QuerySingle[accountRow](context.Background(), db, "SELECT id, name FROM accounts")
			if err != nil {
				t.Errorf("QuerySingle: %v", err)
				return
			}
			if got.ID != 9 || got.Name != "bob" {
				t.Errorf("scanned %+v, want id=9 name=bob", got)
			}
		}()
	}
	wg.Wait()
	expectationsMet(t, mock)
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
