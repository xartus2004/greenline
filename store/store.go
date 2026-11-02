// Package store is the durable write-ahead log for the queue engine, backed
// by SQLite (modernc.org/sqlite, pure Go). Every engine state transition is
// appended and fsynced before it takes effect, so a crash at any point
// recovers by replaying the log (see queue.New).
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/xartus2004/greenline/queue"
	_ "modernc.org/sqlite"
)

// SQLite implements queue.Store.
type SQLite struct {
	mu sync.Mutex
	db *sql.DB
}

// Open opens (creating if needed) the database at path.
func Open(path string) (*SQLite, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=busy_timeout(5000)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS events (
		seq  INTEGER PRIMARY KEY AUTOINCREMENT,
		type TEXT NOT NULL,
		at   INTEGER NOT NULL,
		pr   INTEGER,
		data BLOB NOT NULL
	)`); err != nil {
		db.Close()
		return nil, err
	}
	return &SQLite{db: db}, nil
}

// Append implements queue.Store. It returns only after the transaction is
// committed with synchronous=FULL.
func (s *SQLite) Append(ev queue.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO events(type, at, pr, data) VALUES (?, ?, ?, ?)`, ev.Type, ev.At.UnixNano(), int64(ev.PR), b)
	return err
}

// Compact implements queue.Compacter: the whole log is replaced by snap in
// one transaction, so a crash leaves either the old log or the new one.
func (s *SQLite) Compact(snap []queue.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM events`); err != nil {
		return err
	}
	for _, ev := range snap {
		b, err := json.Marshal(ev)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO events(type, at, pr, data) VALUES (?, ?, ?, ?)`, ev.Type, ev.At.UnixNano(), int64(ev.PR), b); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Load implements queue.Store.
func (s *SQLite) Load() ([]queue.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT seq, data FROM events ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []queue.Event
	for rows.Next() {
		var seq int64
		var b []byte
		if err := rows.Scan(&seq, &b); err != nil {
			return nil, err
		}
		var ev queue.Event
		if err := json.Unmarshal(b, &ev); err != nil {
			return nil, fmt.Errorf("store: event %d: %w", seq, err)
		}
		ev.Seq = seq
		out = append(out, ev)
	}
	return out, rows.Err()
}

// Count returns the number of events.
func (s *SQLite) Count() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&n)
	return n, err
}

// Close closes the database.
func (s *SQLite) Close() error { return s.db.Close() }

var _ queue.Compacter = (*SQLite)(nil)
