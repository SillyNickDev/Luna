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

func TestRingSince(t *testing.T) {
	r := NewRing(20)
	r.Write([]byte("one\ntwo\nthree\n"))
	if got, n := r.Since(1); len(got) != 2 || got[0] != "two" || n != 3 {
		t.Fatalf("since 1: %v %d", got, n)
	}
	r.Write([]byte("a-much-longer-line\n")) // pushes old lines out of the 20-byte ring
	if got, _ := r.Since(0); got[0] == "one" {
		t.Fatalf("ring kept more than its cap: %v", got)
	}
	if got, _ := r.Since(99); len(got) == 0 {
		t.Fatal("a follower ahead of a new ring should get everything")
	}
}
