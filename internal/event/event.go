// Package event defines the envelope every adapter puts on the bus.
package event

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"time"
)

type Event struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	Source    string          `json:"source"`
	TS        time.Time       `json:"ts"`
	Simulated bool            `json:"simulated"`
	Data      json.RawMessage `json:"data"`
}

func New(typ, source string, simulated bool, data any) (Event, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return Event{}, err
	}
	now := time.Now().UTC()
	return Event{ID: newID(now), Type: typ, Source: source, TS: now, Simulated: simulated, Data: raw}, nil
}

// newID is a 48-bit millisecond timestamp plus 80 random bits, so IDs sort by time like a ULID.
func newID(t time.Time) string {
	var b [16]byte
	binary.BigEndian.PutUint64(b[:8], uint64(t.UnixMilli())<<16)
	rand.Read(b[6:])
	return hex.EncodeToString(b[:])
}
