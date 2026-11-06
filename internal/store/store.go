// Package store keeps all runplane state in one SQLite database.
//
// Times are UTC and stored as INTEGER unix milliseconds. Text and number
// columns are NOT NULL, with an empty string or 0 for "unknown". Only times,
// a few foreign keys, and the columns that the schema documents as NULL can
// be NULL; the Go models give these as pointers or as empty strings.
package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

var (
	ErrNotFound  = errors.New("not found")
	ErrConflict  = errors.New("already exists")
	ErrBadCursor = errors.New("bad cursor")
)

//go:embed migrations/*.sql
var migrations embed.FS

// Store has one writer connection, so writes never wait on each other inside
// SQLite, and a pool of reader connections. All writes go to the writer.
type Store struct {
	path string
	w    *sql.DB
	r    *sql.DB
}

const pragmas = "_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"

// Open opens or makes the database at path and applies the migrations.
func Open(path string) (*Store, error) {
	// SQLite gives the WAL and SHM files the mode of the database file.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	f.Close()

	// BEGIN IMMEDIATE takes the write lock at the start, so a transaction
	// never fails to upgrade its lock when another process (the setup-code
	// command) writes at the same time.
	w, err := sql.Open("sqlite", path+"?"+pragmas+"&_pragma=journal_mode(WAL)&_txlock=immediate")
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	w.SetMaxOpenConns(1)
	s := &Store{path: path, w: w}
	if err := s.migrate(context.Background()); err != nil {
		w.Close()
		return nil, err
	}

	s.r, err = sql.Open("sqlite", path+"?"+pragmas+"&_pragma=query_only(1)")
	if err != nil {
		w.Close()
		return nil, fmt.Errorf("open database: %w", err)
	}
	n := max(4, runtime.NumCPU())
	s.r.SetMaxOpenConns(n)
	s.r.SetMaxIdleConns(n)
	return s, nil
}

func (s *Store) Close() error {
	return errors.Join(s.r.Close(), s.w.Close())
}

// VacuumInto writes a copy of the database to path. The copy includes the
// WAL, and writes do not wait for it. It fails if path exists.
func (s *Store) VacuumInto(ctx context.Context, path string) error {
	// A connection of its own: the readers are query_only, and the writer
	// would stop all writes during the copy.
	db, err := sql.Open("sqlite", s.path+"?"+pragmas)
	if err != nil {
		return fmt.Errorf("vacuum into: %w", err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", path); err != nil {
		return fmt.Errorf("vacuum into: %w", err)
	}
	return nil
}

func (s *Store) migrate(ctx context.Context) error {
	// Each file name starts with its number. The numbers start at 1 and have no gaps.
	files, _ := fs.Glob(migrations, "migrations/*.sql")
	list := make([]string, len(files))
	for _, name := range files {
		base := strings.TrimPrefix(name, "migrations/")
		num, _, _ := strings.Cut(base, "_")
		n, err := strconv.Atoi(num)
		if err != nil || n < 1 || n > len(files) || list[n-1] != "" {
			return fmt.Errorf("migration %s: bad number", base)
		}
		b, _ := migrations.ReadFile(name)
		list[n-1] = string(b)
	}
	return s.tx(ctx, "migrate database", func(tx *sql.Tx) error {
		var version int
		if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
			return fmt.Errorf("read schema version: %w", err)
		}
		if version > len(list) {
			return fmt.Errorf("database schema %d is newer than this runplane (%d)", version, len(list))
		}
		for i, m := range list[version:] {
			n := version + i + 1
			if _, err := tx.ExecContext(ctx, m); err != nil {
				return fmt.Errorf("apply migration %d: %w", n, err)
			}
			if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", n)); err != nil {
				return fmt.Errorf("apply migration %d: %w", n, err)
			}
		}
		return nil
	})
}

type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type scanner interface{ Scan(dest ...any) error }

// tx runs fn in one write transaction. Errors get op as a prefix.
func (s *Store) tx(ctx context.Context, op string, fn func(tx *sql.Tx) error) error {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	return nil
}

// queryOne scans the row of a query, or returns ErrNotFound. Errors get op
// as a prefix.
func queryOne[T any](ctx context.Context, q querier, op string, scan func(scanner) (T, error), query string, args ...any) (T, error) {
	v, err := scan(q.QueryRowContext(ctx, query, args...))
	if err != nil {
		var zero T
		return zero, fmt.Errorf("%s: %w", op, dbErr(err))
	}
	return v, nil
}

// queryAll scans all rows of a query. Errors get op as a prefix.
func queryAll[T any](ctx context.Context, q querier, op string, scan func(scanner) (T, error), query string, args ...any) ([]T, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	defer rows.Close()
	var list []T
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", op, err)
		}
		list = append(list, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	return list, nil
}

// queryMap scans all rows of a query into a map. Errors get op as a prefix.
func queryMap[K comparable, V any](ctx context.Context, q querier, op string, scan func(scanner) (K, V, error), query string, args ...any) (map[K]V, error) {
	m := map[K]V{}
	_, err := queryAll(ctx, q, op, func(sc scanner) (K, error) {
		k, v, err := scan(sc)
		m[k] = v
		return k, err
	}, query, args...)
	if err != nil {
		return nil, err
	}
	return m, nil
}

// scanValue scans a row of one column.
func scanValue[T any](sc scanner) (T, error) {
	var v T
	err := sc.Scan(&v)
	return v, err
}

// exec runs a write. Errors get op as a prefix.
func exec(ctx context.Context, q querier, op, query string, args ...any) error {
	_, err := changed(ctx, q, op, query, args...)
	return err
}

// exec1 runs a write that must change a row, or returns ErrNotFound.
func exec1(ctx context.Context, q querier, op, query string, args ...any) error {
	ok, err := changed(ctx, q, op, query, args...)
	if err == nil && !ok {
		return fmt.Errorf("%s: %w", op, ErrNotFound)
	}
	return err
}

// changed runs a write and reports whether it changed a row.
func changed(ctx context.Context, q querier, op, query string, args ...any) (bool, error) {
	res, err := q.ExecContext(ctx, query, args...)
	if err != nil {
		return false, fmt.Errorf("%s: %w", op, dbErr(err))
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("%s: %w", op, err)
	}
	return n > 0, nil
}

// dbErr maps "no rows" to ErrNotFound and unique violations to ErrConflict.
func dbErr(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	var se *sqlite.Error
	if errors.As(err, &se) {
		switch se.Code() {
		case sqlite3.SQLITE_CONSTRAINT_UNIQUE, sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY:
			return fmt.Errorf("%w: %w", ErrConflict, err)
		}
	}
	return err
}

func insert(table, cols string) string {
	return "INSERT INTO " + table + " (" + cols + ") VALUES (" + placeholders(strings.Count(cols, ",")+1) + ")"
}

// upsert returns an insert that updates the row on a conflict on the key
// columns. The update sets each column except the key columns and created_at.
func upsert(table, cols, key string) string {
	keep := append(splitCols(key), "created_at")
	var set []string
	for _, c := range splitCols(cols) {
		if !slices.Contains(keep, c) {
			set = append(set, c+" = excluded."+c)
		}
	}
	return insert(table, cols) + " ON CONFLICT (" + key + ") DO UPDATE SET " + strings.Join(set, ", ")
}

// assign returns "a = ?, b = ?" for the columns "a, b".
func assign(cols string) string {
	return strings.Join(splitCols(cols), " = ?, ") + " = ?"
}

func splitCols(cols string) []string {
	list := strings.Split(cols, ",")
	for i, c := range list {
		list[i] = strings.TrimSpace(c)
	}
	return list
}

// placeholders returns "?, ?, ?" for n values.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

// where returns " WHERE a AND b" for the conditions a and b, or "" for none.
func where(conds []string) string {
	if len(conds) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(conds, " AND ")
}

// encodeCursor returns the cursor of a page that ends with the row at t with id.
func encodeCursor(t int64, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(t, 10) + "." + id))
}

func decodeCursor(c string) (int64, string, error) {
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return 0, "", ErrBadCursor
	}
	n, id, ok := strings.Cut(string(b), ".")
	t, err := strconv.ParseInt(n, 10, 64)
	if !ok || err != nil || id == "" {
		return 0, "", ErrBadCursor
	}
	return t, id, nil
}

// row is a pointer to a model whose cols method returns its fields in the
// order of its columns.
type row[T any] interface {
	*T
	cols() []any
}

func scanRow[T any, P row[T]](sc scanner) (T, error) {
	var v T
	err := sc.Scan(P(&v).cols()...)
	return v, err
}

func ms(t time.Time) int64 { return t.UnixMilli() }

func msPtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UnixMilli()
}

// nullStr writes "" as NULL.
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// col maps a field to a column that needs a conversion. It is a scan
// destination and a query argument.
type col struct {
	scan  func(v any) error
	value func() (driver.Value, error)
}

func (c col) Scan(v any) error             { return c.scan(v) }
func (c col) Value() (driver.Value, error) { return c.value() }

// at maps a time to a NOT NULL column.
func at(t *time.Time) col {
	return col{
		scan: func(v any) error {
			n, ok := v.(int64)
			if !ok {
				return fmt.Errorf("scan time: got %T", v)
			}
			*t = time.UnixMilli(n).UTC()
			return nil
		},
		value: func() (driver.Value, error) { return ms(*t), nil },
	}
}

// atPtr maps a time to a column that can be NULL.
func atPtr(t **time.Time) col {
	return col{
		scan: func(v any) error {
			if v == nil {
				*t = nil
				return nil
			}
			*t = new(time.Time)
			return at(*t).Scan(v)
		},
		value: func() (driver.Value, error) { return msPtr(*t), nil },
	}
}

// text maps a string to a text column that can be NULL. NULL is "".
func text(s *string) col {
	return col{
		scan: func(v any) error {
			switch x := v.(type) {
			case nil:
				*s = ""
			case string:
				*s = x
			default:
				return fmt.Errorf("scan text: got %T", v)
			}
			return nil
		},
		value: func() (driver.Value, error) { return nullStr(*s), nil },
	}
}

// jsonCol maps the value that p points to to a column of JSON text. A nil
// slice becomes [].
func jsonCol(p any) col {
	return col{
		scan: func(v any) error {
			switch x := v.(type) {
			case nil:
				return nil
			case string:
				return json.Unmarshal([]byte(x), p)
			case []byte:
				return json.Unmarshal(x, p)
			}
			return fmt.Errorf("scan json: got %T", v)
		},
		value: func() (driver.Value, error) {
			if rv := reflect.ValueOf(p).Elem(); rv.Kind() == reflect.Slice && rv.IsNil() {
				return "[]", nil
			}
			b, err := json.Marshal(p)
			if err != nil {
				return nil, err
			}
			return string(b), nil
		},
	}
}
