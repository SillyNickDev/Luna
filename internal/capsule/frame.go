package capsule

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/fxamacker/cbor/v2"
)

// MaxFrame is the spec's hard cap; a bigger frame closes the connection.
const MaxFrame = 1 << 20

// Frame is one control or link message: 4-byte big-endian length, then a CBOR map.
type Frame struct {
	T  string          `cbor:"t"`
	ID uint64          `cbor:"id,omitempty"`
	B  cbor.RawMessage `cbor:"b,omitempty"`
}

// Bodies of the control and link messages (Capsules Spec, Control channel and links).
type (
	Hello struct {
		Spec string `cbor:"spec"`
		PID  int    `cbor:"pid"`
	}
	Welcome struct {
		ID     string         `cbor:"id"`
		Limits map[string]any `cbor:"limits"`
		Links  []string       `cbor:"links"`
	}
	LinkName struct {
		Name string `cbor:"name"`
	}
	Shutdown struct {
		DeadlineMS int64 `cbor:"deadline_ms"`
	}
	Log struct {
		Level string `cbor:"level"`
		Msg   string `cbor:"msg"`
	}
	Call struct {
		Method string          `cbor:"method"`
		Body   cbor.RawMessage `cbor:"body,omitempty"`
		Caller string          `cbor:"caller,omitempty"` // stamped by the broker; ignored from consumers
	}
	Result struct {
		OK    bool            `cbor:"ok"`
		Body  cbor.RawMessage `cbor:"body,omitempty"`
		Error string          `cbor:"error,omitempty"`
	}
)

var decMode, _ = cbor.DecOptions{
	MaxNestedLevels:  16,
	MaxArrayElements: 4096,
	MaxMapPairs:      4096,
	DupMapKey:        cbor.DupMapKeyEnforcedAPF,
	IndefLength:      cbor.IndefLengthForbidden,
	TagsMd:           cbor.TagsForbidden,
}.DecMode()

var encMode, _ = cbor.CoreDetEncOptions().EncMode()

// Decode unmarshals a frame body into a fixed type.
func Decode(raw cbor.RawMessage, v any) error { return decMode.Unmarshal(raw, v) }

// Conn reads and writes frames; writes are safe from several goroutines.
type Conn struct {
	rw  io.ReadWriter
	wmu sync.Mutex
	buf [4]byte
}

func NewConn(rw io.ReadWriter) *Conn { return &Conn{rw: rw} }

var ErrFrameTooBig = errors.New("frame over 1 MiB")

func (c *Conn) Read() (Frame, error) {
	if _, err := io.ReadFull(c.rw, c.buf[:]); err != nil {
		return Frame{}, err
	}
	n := binary.BigEndian.Uint32(c.buf[:])
	if n > MaxFrame {
		return Frame{}, ErrFrameTooBig
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(c.rw, b); err != nil {
		return Frame{}, err
	}
	var f Frame
	if err := decMode.Unmarshal(b, &f); err != nil {
		return Frame{}, fmt.Errorf("bad frame: %w", err)
	}
	return f, nil
}

// Write sends a frame whose body is v (nil for none).
func (c *Conn) Write(t string, id uint64, v any) error {
	f := Frame{T: t, ID: id}
	if v != nil {
		var err error
		if f.B, err = encMode.Marshal(v); err != nil {
			return err
		}
	}
	b, err := encMode.Marshal(f)
	if err != nil {
		return err
	}
	if len(b) > MaxFrame {
		return ErrFrameTooBig
	}
	out := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(out, uint32(len(b)))
	copy(out[4:], b)
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.rw.Write(out)
	return err
}
