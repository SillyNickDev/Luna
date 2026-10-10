// Package store is Luna's SQLite database. The event log is what replay reads from.
package store

import (
	"database/sql"
	"log/slog"
	"time"

	_ "modernc.org/sqlite"

	"luna/internal/event"
)

const schema = `
CREATE TABLE IF NOT EXISTS events (
	id        TEXT PRIMARY KEY,
	type      TEXT NOT NULL,
	source    TEXT NOT NULL,
	ts        INTEGER NOT NULL,
	simulated INTEGER NOT NULL,
	data      BLOB NOT NULL
);
CREATE INDEX IF NOT EXISTS events_ts ON events(ts);`

type Store struct {
	db     *sql.DB
	insert *sql.Stmt
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // one writer
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	insert, err := db.Prepare(`INSERT OR IGNORE INTO events (id, type, source, ts, simulated, data) VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, insert: insert}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Log appends events from c in batches (250 ms or 500 events, whichever first)
// and flushes what's left when c closes.
func (s *Store) Log(c <-chan event.Event) {
	batch := make([]event.Event, 0, 500)
	var due <-chan time.Time // nil while idle, so no wake-ups without events
	flush := func() {
		if len(batch) > 0 {
			if err := s.write(batch); err != nil {
				slog.Error("event log write failed", "events", len(batch), "err", err)
			}
		}
		batch, due = batch[:0], nil
	}
	for {
		select {
		case ev, ok := <-c:
			if !ok {
				flush()
				return
			}
			batch = append(batch, ev)
			if len(batch) == 1 {
				due = time.After(250 * time.Millisecond)
			}
			if len(batch) == cap(batch) {
				flush()
			}
		case <-due:
			flush()
		}
	}
}

func (s *Store) write(batch []event.Event) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	st := tx.Stmt(s.insert)
	for _, ev := range batch {
		if _, err := st.Exec(ev.ID, ev.Type, ev.Source, ev.TS.UnixMilli(), ev.Simulated, []byte(ev.Data)); err != nil {
			return err
		}
	}
	return tx.Commit()
}
