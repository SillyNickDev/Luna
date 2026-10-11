// Package osc talks to VRChat over OSC: UDP on one reused socket, outgoing parameters
// coalesced per address per frame, incoming ones kept in a cache and published as events.
package osc

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// Message is one OSC message. Args are int32, float32, string or bool.
type Message struct {
	Address string
	Args    []any
}

func pad(b *bytes.Buffer) {
	for b.Len()%4 != 0 {
		b.WriteByte(0)
	}
}

func writeString(b *bytes.Buffer, s string) {
	b.WriteString(s)
	b.WriteByte(0)
	pad(b)
}

// Encode turns m into a packet. Go ints and float64s are narrowed to OSC's 32-bit types.
func (m Message) Encode() ([]byte, error) {
	var b, args bytes.Buffer
	tags := ","
	for _, a := range m.Args {
		switch v := a.(type) {
		case bool:
			tags += map[bool]string{true: "T", false: "F"}[v]
		case int:
			if v < math.MinInt32 || v > math.MaxInt32 {
				return nil, fmt.Errorf("osc: %d doesn't fit an int32", v)
			}
			tags += "i"
			binary.Write(&args, binary.BigEndian, int32(v))
		case int32:
			tags += "i"
			binary.Write(&args, binary.BigEndian, v)
		case float32:
			tags += "f"
			binary.Write(&args, binary.BigEndian, v)
		case float64:
			tags += "f"
			binary.Write(&args, binary.BigEndian, float32(v))
		case string:
			tags += "s"
			writeString(&args, v)
		default:
			return nil, fmt.Errorf("osc: can't send a %T", a)
		}
	}
	writeString(&b, m.Address)
	writeString(&b, tags)
	b.Write(args.Bytes())
	return b.Bytes(), nil
}

var errShort = errors.New("osc: truncated packet")

func readString(p []byte) (string, []byte, error) {
	i := bytes.IndexByte(p, 0)
	if i < 0 {
		return "", nil, errShort
	}
	n := (i + 4) &^ 3
	if n > len(p) {
		return "", nil, errShort
	}
	return string(p[:i]), p[n:], nil
}

// Decode parses a packet into messages, unpacking bundles (up to 4 deep).
func Decode(p []byte) ([]Message, error) { return decode(p, 0) }

func decode(p []byte, depth int) ([]Message, error) {
	if bytes.HasPrefix(p, []byte("#bundle\x00")) {
		if depth >= 4 {
			return nil, errors.New("osc: bundles nested too deep")
		}
		if len(p) < 16 {
			return nil, errShort
		}
		var out []Message
		for p = p[16:]; len(p) > 0; {
			if len(p) < 4 {
				return nil, errShort
			}
			n := int(binary.BigEndian.Uint32(p))
			if n < 0 || n > len(p)-4 || n%4 != 0 {
				return nil, errShort
			}
			ms, err := decode(p[4:4+n], depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, ms...)
			p = p[4+n:]
		}
		return out, nil
	}
	addr, p, err := readString(p)
	if err != nil {
		return nil, err
	}
	if len(addr) == 0 || addr[0] != '/' {
		return nil, fmt.Errorf("osc: bad address %q", addr)
	}
	m := Message{Address: addr}
	if len(p) == 0 { // some senders omit the type tags for no arguments
		return []Message{m}, nil
	}
	tags, p, err := readString(p)
	if err != nil {
		return nil, err
	}
	if len(tags) == 0 || tags[0] != ',' {
		return nil, errors.New("osc: missing type tags")
	}
	for _, t := range tags[1:] {
		switch t {
		case 'i', 'f':
			if len(p) < 4 {
				return nil, errShort
			}
			u := binary.BigEndian.Uint32(p)
			p = p[4:]
			if t == 'i' {
				m.Args = append(m.Args, int32(u))
			} else {
				m.Args = append(m.Args, math.Float32frombits(u))
			}
		case 'h', 'd':
			if len(p) < 8 {
				return nil, errShort
			}
			u := binary.BigEndian.Uint64(p)
			p = p[8:]
			if t == 'h' {
				m.Args = append(m.Args, int32(int64(u))) // VRChat's ints are 32-bit
			} else {
				m.Args = append(m.Args, float32(math.Float64frombits(u)))
			}
		case 's':
			var s string
			if s, p, err = readString(p); err != nil {
				return nil, err
			}
			m.Args = append(m.Args, s)
		case 'T':
			m.Args = append(m.Args, true)
		case 'F':
			m.Args = append(m.Args, false)
		case 'N', 'I':
			m.Args = append(m.Args, nil)
		default:
			return nil, fmt.Errorf("osc: unsupported type tag %q", t)
		}
	}
	return []Message{m}, nil
}
