package capsule

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	c := NewConn(&buf)
	if err := c.Write("hello", 0, Hello{Spec: "0.1", PID: 7}); err != nil {
		t.Fatal(err)
	}
	if err := c.Write("ping", 3, nil); err != nil {
		t.Fatal(err)
	}
	f, err := c.Read()
	var h Hello
	if err != nil || f.T != "hello" || Decode(f.B, &h) != nil || h.PID != 7 {
		t.Fatalf("got %+v %+v %v", f, h, err)
	}
	if f, err = c.Read(); err != nil || f.T != "ping" || f.ID != 3 {
		t.Fatalf("got %+v %v", f, err)
	}
}

func TestFrameTooBig(t *testing.T) {
	var buf bytes.Buffer
	binary.Write(&buf, binary.BigEndian, uint32(2<<20))
	buf.Write(make([]byte, 64))
	if _, err := NewConn(&buf).Read(); !errors.Is(err, ErrFrameTooBig) {
		t.Fatalf("2 MiB frame not rejected: %v", err)
	}
}

func FuzzFrame(f *testing.F) {
	var buf bytes.Buffer
	NewConn(&buf).Write("call", 1, Call{Method: "show", Caller: "x"})
	f.Add(buf.Bytes())
	f.Fuzz(func(t *testing.T, b []byte) {
		fr, err := NewConn(bytes.NewBuffer(b)).Read()
		if err == nil {
			var c Call
			Decode(fr.B, &c)
		}
	})
}
