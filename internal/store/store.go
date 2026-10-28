// Package store is lasso's SQLite database (WAL mode).
//
// It holds only what can be lost: read cursors, answers to decisions and
// caches. Rules and the audit log live in files (ADR 0001 §2), so deleting
// the database is always safe.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // database/sql driver "sqlite"
)

// Store is an open database.
type Store struct {
	DB *sql.DB
}

// migrations run in order; the index+1 is the schema version. Append only.
var migrations = []string{
	`CREATE TABLE cursors (
		name TEXT PRIMARY KEY,
		value TEXT NOT NULL,
		updated_at INTEGER NOT NULL
	);
	CREATE TABLE answers (
		prompt_id TEXT PRIMARY KEY,
		bot TEXT NOT NULL,
		action TEXT NOT NULL,
		choice TEXT NOT NULL,
		answered_at INTEGER NOT NULL
	);
	CREATE TABLE cache (
		key TEXT PRIMARY KEY,
		value BLOB NOT NULL,
		expires_at INTEGER NOT NULL
	);`,
	`CREATE TABLE spend (
		feature TEXT NOT NULL,
		day TEXT NOT NULL,
		usd REAL NOT NULL,
		PRIMARY KEY (feature, day)
	);`,
}

// Open opens (creating if needed) the database at path and migrates it.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	s := &Store{DB: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("store %s: %w", path, err)
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.DB.Close() }

func (s *Store) migrate() error {
	var v int
	if err := s.DB.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	if v > len(migrations) {
		return fmt.Errorf("schema version %d is newer than this lasso (%d)", v, len(migrations))
	}
	for i := v; i < len(migrations); i++ {
		tx, err := s.DB.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// Version is the schema version.
func (s *Store) Version() (int, error) {
	var v int
	err := s.DB.QueryRow(`PRAGMA user_version`).Scan(&v)
	return v, err
}

// Cursor returns a named cursor, or "" when unset.
func (s *Store) Cursor(name string) (string, error) {
	var v string
	err := s.DB.QueryRow(`SELECT value FROM cursors WHERE name = ?`, name).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetCursor stores a named cursor.
func (s *Store) SetCursor(name, value string) error {
	_, err := s.DB.Exec(`INSERT INTO cursors (name, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		name, value, time.Now().Unix())
	return err
}

// CacheGet returns a cached value that hasn't expired at now.
func (s *Store) CacheGet(key string, now time.Time) ([]byte, bool, error) {
	var v []byte
	err := s.DB.QueryRow(`SELECT value FROM cache WHERE key = ? AND expires_at > ?`, key, now.Unix()).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	return v, err == nil, err
}

// CachePut stores a value until now+ttl.
func (s *Store) CachePut(key string, value []byte, now time.Time, ttl time.Duration) error {
	_, err := s.DB.Exec(`INSERT INTO cache (key, value, expires_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, expires_at = excluded.expires_at`,
		key, value, now.Add(ttl).Unix())
	return err
}

// Spent is what feature has spent on day (YYYY-MM-DD), in US dollars.
func (s *Store) Spent(feature, day string) (float64, error) {
	var usd float64
	err := s.DB.QueryRow(`SELECT usd FROM spend WHERE feature = ? AND day = ?`, feature, day).Scan(&usd)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return usd, err
}

// AddSpend adds usd to what feature has spent on day.
func (s *Store) AddSpend(feature, day string, usd float64) error {
	_, err := s.DB.Exec(`INSERT INTO spend (feature, day, usd) VALUES (?, ?, ?)
		ON CONFLICT(feature, day) DO UPDATE SET usd = usd + excluded.usd`,
		feature, day, usd)
	return err
}
