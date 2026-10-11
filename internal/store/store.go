// Package store is Luna's SQLite database. The event log is what replay reads from.
package store

import (
	"database/sql"
	"log/slog"
	"slices"
	"strings"
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
CREATE INDEX IF NOT EXISTS events_ts ON events(ts);
CREATE TABLE IF NOT EXISTS ranks (
	user_id TEXT PRIMARY KEY,
	login   TEXT NOT NULL,
	rank    TEXT NOT NULL,
	set_by  TEXT NOT NULL,
	updated INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS capsules (
	id       TEXT PRIMARY KEY,
	version  TEXT NOT NULL,
	hash     TEXT NOT NULL,
	enabled  INTEGER NOT NULL,
	uses     TEXT NOT NULL,
	approved TEXT NOT NULL,
	updated  INTEGER NOT NULL
);`

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

// RankRow is a rank Luna keeps itself (head mod, Luna mod); the rest come from Twitch.
type RankRow struct {
	UserID, Login, Rank, SetBy string
	Updated                    time.Time
}

func (s *Store) Ranks() ([]RankRow, error) {
	rows, err := s.db.Query(`SELECT user_id, login, rank, set_by, updated FROM ranks ORDER BY rank DESC, login`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RankRow
	for rows.Next() {
		var r RankRow
		var ms int64
		if err := rows.Scan(&r.UserID, &r.Login, &r.Rank, &r.SetBy, &ms); err != nil {
			return nil, err
		}
		r.Updated = time.UnixMilli(ms)
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetRank stores r, or deletes the row when r.Rank is empty.
func (s *Store) SetRank(r RankRow) error {
	if r.Rank == "" {
		_, err := s.db.Exec(`DELETE FROM ranks WHERE user_id = ?`, r.UserID)
		return err
	}
	_, err := s.db.Exec(`INSERT INTO ranks (user_id, login, rank, set_by, updated) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(user_id) DO UPDATE SET login = excluded.login, rank = excluded.rank, set_by = excluded.set_by, updated = excluded.updated`,
		r.UserID, r.Login, r.Rank, r.SetBy, time.Now().UnixMilli())
	return err
}

// Session is a recorded stream: real events with no gap longer than the split gap.
// ponytail: gap-based; switch to stream.online/offline events once those are subscribed.
type Session struct {
	Start, End time.Time
	Events     int
}

// Sessions lists recorded sessions, newest first.
func (s *Store) Sessions(gap time.Duration) ([]Session, error) {
	rows, err := s.db.Query(`SELECT ts FROM events WHERE simulated = 0 ORDER BY ts`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var ms int64
		if err := rows.Scan(&ms); err != nil {
			return nil, err
		}
		ts := time.UnixMilli(ms)
		if n := len(out); n > 0 && ts.Sub(out[n-1].End) <= gap {
			out[n-1].End = ts
			out[n-1].Events++
		} else {
			out = append(out, Session{Start: ts, End: ts, Events: 1})
		}
	}
	slices.Reverse(out)
	return out, rows.Err()
}

// RealEvents returns the non-simulated events from start to end inclusive, oldest first.
func (s *Store) RealEvents(start, end time.Time) ([]event.Event, error) {
	rows, err := s.db.Query(`SELECT id, type, source, ts, data FROM events
		WHERE simulated = 0 AND ts BETWEEN ? AND ? ORDER BY ts`, start.UnixMilli(), end.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []event.Event
	for rows.Next() {
		var ev event.Event
		var ms int64
		if err := rows.Scan(&ev.ID, &ev.Type, &ev.Source, &ms, &ev.Data); err != nil {
			return nil, err
		}
		ev.TS = time.UnixMilli(ms).UTC()
		out = append(out, ev)
	}
	return out, rows.Err()
}

// CapsuleRow is an installed capsule. Uses are the links the user approved;
// Approved fingerprints the permission set they said yes to.
type CapsuleRow struct {
	ID, Version, Hash, Approved string
	Enabled                     bool
	Uses                        []string
}

func (s *Store) Capsules() ([]CapsuleRow, error) {
	rows, err := s.db.Query(`SELECT id, version, hash, enabled, uses, approved FROM capsules ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CapsuleRow
	for rows.Next() {
		var r CapsuleRow
		var uses string
		if err := rows.Scan(&r.ID, &r.Version, &r.Hash, &r.Enabled, &uses, &r.Approved); err != nil {
			return nil, err
		}
		if uses != "" {
			r.Uses = strings.Split(uses, "\n")
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) SaveCapsule(r CapsuleRow) error {
	_, err := s.db.Exec(`INSERT INTO capsules (id, version, hash, enabled, uses, approved, updated) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET version = excluded.version, hash = excluded.hash, enabled = excluded.enabled,
		uses = excluded.uses, approved = excluded.approved, updated = excluded.updated`,
		r.ID, r.Version, r.Hash, r.Enabled, strings.Join(r.Uses, "\n"), r.Approved, time.Now().UnixMilli())
	return err
}

func (s *Store) DeleteCapsule(id string) error {
	_, err := s.db.Exec(`DELETE FROM capsules WHERE id = ?`, id)
	return err
}
