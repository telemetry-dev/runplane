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
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("already exists")
)

//go:embed migrations/*.sql
var migrations embed.FS

// Store has one writer connection, so writes never wait on each other inside
// SQLite, and a pool of reader connections. All writes go to the writer.
type Store struct {
	w *sql.DB
	r *sql.DB
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
	w.SetMaxIdleConns(1)
	w.SetConnMaxIdleTime(0)
	w.SetConnMaxLifetime(0)
	if err := migrate(context.Background(), w); err != nil {
		w.Close()
		return nil, err
	}

	// The readers are not query_only, because VACUUM INTO needs write access
	// to its target file.
	r, err := sql.Open("sqlite", path+"?"+pragmas)
	if err != nil {
		w.Close()
		return nil, fmt.Errorf("open database: %w", err)
	}
	n := max(4, runtime.NumCPU())
	r.SetMaxOpenConns(n)
	r.SetMaxIdleConns(n)
	return &Store{w: w, r: r}, nil
}

func (s *Store) Close() error {
	return errors.Join(s.r.Close(), s.w.Close())
}

// DB returns the reader pool for VACUUM INTO, which then does not stop writes.
func (s *Store) DB() *sql.DB { return s.r }

func migrate(ctx context.Context, db *sql.DB) error {
	files, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return fmt.Errorf("list migrations: %w", err)
	}
	// Each file name starts with its number. The numbers start at 1 and have no gaps.
	type migration struct {
		n   int
		sql string
	}
	list := make([]migration, len(files))
	for _, name := range files {
		base := strings.TrimPrefix(name, "migrations/")
		num, _, _ := strings.Cut(base, "_")
		n, err := strconv.Atoi(num)
		if err != nil || n < 1 || n > len(files) || list[n-1].sql != "" {
			return fmt.Errorf("migration %s: bad number", base)
		}
		b, err := migrations.ReadFile(name)
		if err != nil {
			return fmt.Errorf("read migration: %w", err)
		}
		list[n-1] = migration{n, string(b)}
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version > len(list) {
		return fmt.Errorf("database schema %d is newer than this runplane (%d)", version, len(list))
	}
	for _, m := range list[version:] {
		if _, err := tx.ExecContext(ctx, m.sql); err != nil {
			return fmt.Errorf("apply migration %d: %w", m.n, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", m.n)); err != nil {
			return fmt.Errorf("apply migration %d: %w", m.n, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}
	return nil
}

type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// tx runs fn in one write transaction.
func (s *Store) tx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// exec1 runs a write that must change one row, or returns ErrNotFound.
func exec1(ctx context.Context, q querier, query string, args ...any) error {
	res, err := q.ExecContext(ctx, query, args...)
	if err != nil {
		return dbErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
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

// scanAll reads all rows with scan, which scans one row into a new T.
func scanAll[T any](rows *sql.Rows, err error, scan func(sc scanner) (T, error)) ([]T, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []T
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, v)
	}
	return list, rows.Err()
}

type scanner interface{ Scan(dest ...any) error }

func ms(t time.Time) int64 { return t.UnixMilli() }

func msPtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UnixMilli()
}

func fromMS(v int64) time.Time { return time.UnixMilli(v).UTC() }

// at scans a NOT NULL time column.
func at(dst *time.Time) sql.Scanner { return timeCol{dst} }

// atPtr scans a time column that can be NULL.
func atPtr(dst **time.Time) sql.Scanner { return timePtrCol{dst} }

type timeCol struct{ dst *time.Time }

func (c timeCol) Scan(v any) error {
	n, ok := v.(int64)
	if !ok {
		return fmt.Errorf("scan time: got %T", v)
	}
	*c.dst = fromMS(n)
	return nil
}

type timePtrCol struct{ dst **time.Time }

func (c timePtrCol) Scan(v any) error {
	if v == nil {
		*c.dst = nil
		return nil
	}
	n, ok := v.(int64)
	if !ok {
		return fmt.Errorf("scan time: got %T", v)
	}
	t := fromMS(n)
	*c.dst = &t
	return nil
}

// jsonArg writes v as JSON text. A nil slice becomes [].
func jsonArg(v any) driver.Valuer { return jsonValue{v} }

type jsonValue struct{ v any }

func (j jsonValue) Value() (driver.Value, error) {
	if rv := reflect.ValueOf(j.v); rv.Kind() == reflect.Slice && rv.IsNil() {
		return "[]", nil
	}
	b, err := json.Marshal(j.v)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

// jsonCol scans JSON text into dst.
func jsonCol(dst any) sql.Scanner { return jsonScan{dst} }

type jsonScan struct{ dst any }

func (j jsonScan) Scan(v any) error {
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		return json.Unmarshal([]byte(x), j.dst)
	case []byte:
		return json.Unmarshal(x, j.dst)
	}
	return fmt.Errorf("scan json: got %T", v)
}

// nullStr writes "" as NULL.
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// placeholders returns "?, ?, ?" for n values.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}
