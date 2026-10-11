// Package store is Luna's SQLite database. The event log is what replay reads from.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
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
);
CREATE TABLE IF NOT EXISTS plugin_store (
	plugin  TEXT NOT NULL,
	key     TEXT NOT NULL,
	value   BLOB NOT NULL,
	expires INTEGER NOT NULL, -- unix ms, 0 = never
	PRIMARY KEY (plugin, key)
);
CREATE TABLE IF NOT EXISTS viewers (
	user_id     TEXT PRIMARY KEY,
	login       TEXT NOT NULL,
	first_seen  INTEGER NOT NULL, -- unix ms
	last_seen   INTEGER NOT NULL,
	messages    INTEGER NOT NULL,
	watch_ms    INTEGER NOT NULL, -- time chatting while live
	streams     INTEGER NOT NULL, -- streams they chatted in
	streak      INTEGER NOT NULL, -- consecutive streams
	last_stream INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS viewers_login ON viewers(login);`

type Store struct {
	db     *sql.DB
	insert *sql.Stmt
	Now    func() time.Time // plugin store TTLs; a test harness swaps in its fake clock
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
	return &Store{db: db, insert: insert, Now: time.Now}, nil
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

// Plugin store limits, so one plugin can't fill the disk.
const (
	MaxKey       = 256
	MaxValue     = 64 << 10
	MaxKeys      = 10000
	errKeyPrefix = "store: "
)

var ErrStoreFull = errors.New(errKeyPrefix + "this plugin already has the maximum of 10000 keys")

func live(now time.Time) int64 { return now.UnixMilli() }

func (s *Store) live() int64 { return s.Now().UnixMilli() }

// PluginGet returns a plugin's JSON value, or nil if missing or expired.
func (s *Store) PluginGet(plugin, key string) ([]byte, error) {
	var v []byte
	err := s.db.QueryRow(`SELECT value FROM plugin_store WHERE plugin = ? AND key = ? AND (expires = 0 OR expires > ?)`,
		plugin, key, s.live()).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return v, err
}

// PluginSet stores a JSON value; ttl 0 keeps it forever.
func (s *Store) PluginSet(plugin, key string, value []byte, ttl time.Duration) error {
	if err := checkKV(key, value); err != nil {
		return err
	}
	return s.PluginBatch(plugin, []KV{{key, value, ttl}}, nil)
}

type KV struct {
	Key   string
	Value []byte
	TTL   time.Duration
}

// PluginBatch applies sets then deletes in one transaction: all of them or none.
func (s *Store) PluginBatch(plugin string, sets []KV, deletes []string) error {
	for _, kv := range sets {
		if err := checkKV(kv.Key, kv.Value); err != nil {
			return err
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, kv := range sets {
		var exp int64
		if kv.TTL > 0 {
			exp = s.Now().Add(kv.TTL).UnixMilli()
		}
		if err := s.roomFor(tx, plugin, kv.Key); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO plugin_store (plugin, key, value, expires) VALUES (?, ?, ?, ?)
			ON CONFLICT(plugin, key) DO UPDATE SET value = excluded.value, expires = excluded.expires`, plugin, kv.Key, kv.Value, exp); err != nil {
			return err
		}
	}
	for _, k := range deletes {
		if _, err := tx.Exec(`DELETE FROM plugin_store WHERE plugin = ? AND key = ?`, plugin, k); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PluginIncr adds by to an integer value (missing or expired counts as 0), atomically.
func (s *Store) PluginIncr(plugin, key string, by int64) (int64, error) {
	if err := checkKV(key, nil); err != nil {
		return 0, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var cur []byte
	var exp int64
	err = tx.QueryRow(`SELECT value, expires FROM plugin_store WHERE plugin = ? AND key = ?`, plugin, key).Scan(&cur, &exp)
	var n int64
	switch {
	case errors.Is(err, sql.ErrNoRows) || exp != 0 && exp <= s.live():
		if err := s.roomFor(tx, plugin, key); err != nil {
			return 0, err
		}
		exp = 0
	case err != nil:
		return 0, err
	default:
		if json.Unmarshal(cur, &n) != nil {
			return 0, fmt.Errorf(errKeyPrefix+"%q doesn't hold an integer", key)
		}
	}
	n += by
	if _, err := tx.Exec(`INSERT INTO plugin_store (plugin, key, value, expires) VALUES (?, ?, ?, ?)
		ON CONFLICT(plugin, key) DO UPDATE SET value = excluded.value`, plugin, key, strconv.AppendInt(nil, n, 10), exp); err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

func (s *Store) PluginDelete(plugin, key string) error {
	_, err := s.db.Exec(`DELETE FROM plugin_store WHERE plugin = ? AND key = ?`, plugin, key)
	return err
}

// PluginKeys lists live keys starting with prefix, sorted.
func (s *Store) PluginKeys(plugin, prefix string) ([]string, error) {
	rows, err := s.db.Query(`SELECT key FROM plugin_store WHERE plugin = ? AND substr(key, 1, ?) = ? AND (expires = 0 OR expires > ?) ORDER BY key`,
		plugin, len(prefix), prefix, s.live())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// PluginForget deletes everything a plugin stored (uninstall without --keep-data).
func (s *Store) PluginForget(plugin string) error {
	_, err := s.db.Exec(`DELETE FROM plugin_store WHERE plugin = ?`, plugin)
	return err
}

func checkKV(key string, value []byte) error {
	switch {
	case key == "" || len(key) > MaxKey:
		return fmt.Errorf(errKeyPrefix+"keys must be 1-%d bytes", MaxKey)
	case len(value) > MaxValue:
		return fmt.Errorf(errKeyPrefix+"values must be at most %d KiB", MaxValue>>10)
	}
	return nil
}

// roomFor refuses a new key once the plugin is at MaxKeys, dropping expired rows first.
func (s *Store) roomFor(tx *sql.Tx, plugin, key string) error {
	var exists bool
	tx.QueryRow(`SELECT 1 FROM plugin_store WHERE plugin = ? AND key = ?`, plugin, key).Scan(&exists)
	if exists {
		return nil
	}
	if _, err := tx.Exec(`DELETE FROM plugin_store WHERE plugin = ? AND expires != 0 AND expires <= ?`, plugin, s.live()); err != nil {
		return err
	}
	var n int
	if err := tx.QueryRow(`SELECT count(*) FROM plugin_store WHERE plugin = ?`, plugin).Scan(&n); err != nil {
		return err
	}
	if n >= MaxKeys {
		return ErrStoreFull
	}
	return nil
}

// Viewer is Luna's shared profile of one chatter.
type Viewer struct {
	ID        string    `json:"id"`
	Login     string    `json:"login"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	Messages  int64     `json:"messages"`
	WatchS    int64     `json:"watch_s"`
	Streams   int64     `json:"streams"`
	Streak    int64     `json:"streak"`
}

const viewerCols = `user_id, login, first_seen, last_seen, messages, watch_ms, streams, streak, last_stream`

func scanViewer(row interface{ Scan(...any) error }) (*Viewer, int64, error) {
	var v Viewer
	var first, last, watch, lastStream int64
	err := row.Scan(&v.ID, &v.Login, &first, &last, &v.Messages, &watch, &v.Streams, &v.Streak, &lastStream)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, nil
	}
	v.FirstSeen, v.LastSeen, v.WatchS = time.UnixMilli(first).UTC(), time.UnixMilli(last).UTC(), watch/1000
	return &v, lastStream, err
}

// activeGap is how long after a message a chatter still counts as watching.
const activeGap = 10 * time.Minute

// SeeChatter records a chat message from id and returns their profile from before it
// (nil the first time Luna sees them). stream numbers the current live stream, 0 offline:
// watch time and streaks only count while live.
func (s *Store) SeeChatter(id, login string, now time.Time, stream int64) (*Viewer, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	prev, lastStream, err := scanViewer(tx.QueryRow(`SELECT `+viewerCols+` FROM viewers WHERE user_id = ?`, id))
	if err != nil {
		return nil, err
	}
	v := Viewer{ID: id, Login: login, FirstSeen: now, LastSeen: now, Messages: 1}
	var watch int64
	if prev != nil {
		v, watch = *prev, prev.WatchS*1000
		v.Login, v.LastSeen = login, now
		v.Messages++
		if gap := now.Sub(prev.LastSeen); stream > 0 && lastStream == stream && gap < activeGap {
			watch += gap.Milliseconds()
		}
	}
	if stream > 0 && lastStream != stream {
		v.Streams++
		if prev != nil && lastStream == stream-1 {
			v.Streak++
		} else {
			v.Streak = 1
		}
		lastStream = stream
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO viewers (`+viewerCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		v.ID, v.Login, v.FirstSeen.UnixMilli(), v.LastSeen.UnixMilli(), v.Messages, watch, v.Streams, v.Streak, lastStream); err != nil {
		return nil, err
	}
	return prev, tx.Commit()
}

// GetViewer finds a profile by Twitch user id or login; nil if never seen.
func (s *Store) GetViewer(user string) (*Viewer, error) {
	v, _, err := scanViewer(s.db.QueryRow(`SELECT `+viewerCols+` FROM viewers WHERE user_id = ? OR login = ? ORDER BY last_seen DESC LIMIT 1`, user, strings.ToLower(user)))
	return v, err
}
