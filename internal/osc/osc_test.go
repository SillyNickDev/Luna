package osc

import (
	"context"
	"encoding/binary"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"luna/internal/event"
)

func TestRoundTrip(t *testing.T) {
	m := Message{"/avatar/parameters/Ears", []any{true, 3, float32(0.5), "hi", false}}
	b, err := m.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if len(b)%4 != 0 {
		t.Fatalf("packet not 4-byte aligned: %d", len(b))
	}
	got, err := Decode(b)
	want := Message{"/avatar/parameters/Ears", []any{true, int32(3), float32(0.5), "hi", false}}
	if err != nil || len(got) != 1 || !reflect.DeepEqual(got[0], want) {
		t.Fatalf("got %#v %v", got, err)
	}
	// A bundle of two messages.
	b2, _ := Message{"/avatar/change", []any{"avtr_x"}}.Encode()
	bundle := append([]byte("#bundle\x00"), make([]byte, 8)...)
	for _, p := range [][]byte{b, b2} {
		bundle = binary.BigEndian.AppendUint32(bundle, uint32(len(p)))
		bundle = append(bundle, p...)
	}
	if got, err := Decode(bundle); err != nil || len(got) != 2 || got[1].Address != "/avatar/change" {
		t.Fatalf("bundle: %v %v", got, err)
	}
	for _, bad := range [][]byte{b[:len(b)-2], []byte("nope\x00\x00\x00\x00"), bundle[:20], append(bundle[:16:16], 0xff, 0xff, 0xff, 0xf0)} {
		if _, err := Decode(bad); err == nil {
			t.Errorf("decoded junk %q", bad)
		}
	}
	if _, err := (Message{"/x", []any{1 << 40}}).Encode(); err == nil {
		t.Error("encoded an int that doesn't fit")
	}
}

func TestClient(t *testing.T) {
	vrc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer vrc.Close()
	dir := t.TempDir()
	id := "avtr_01234567-89ab-cdef-0123-456789abcdef"
	os.MkdirAll(filepath.Join(dir, "usr_me", "Avatars"), 0o755)
	os.WriteFile(filepath.Join(dir, "usr_me", "Avatars", id+".json"), []byte("\xef\xbb\xbf"+`{"id":"`+id+`","name":"Winterpaw",
		"parameters":[{"name":"Ears","input":{"address":"/avatar/parameters/Ears","type":"Bool"},"output":{"type":"Bool"}},
		{"name":"VelocityX","output":{"type":"Float"}}]}`), 0o644)

	var mu sync.Mutex
	var evs []event.Event
	c, err := Dial(vrc.LocalAddr().String(), "127.0.0.1:0", dir, func(e event.Event) { mu.Lock(); evs = append(evs, e); mu.Unlock() })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	// Two values for one address inside a frame: only the latest goes out.
	c.Send("/avatar/parameters/Ears", true)
	c.Send("/avatar/parameters/Ears", false)
	c.Chatbox("hello")
	vrc.SetReadDeadline(time.Now().Add(2 * time.Second))
	var got []Message
	buf := make([]byte, 1500)
	for len(got) < 2 {
		n, err := vrc.Read(buf)
		if err != nil {
			t.Fatalf("after %v: %v", got, err)
		}
		ms, _ := Decode(buf[:n])
		got = append(got, ms...)
	}
	if got[0].Address != "/avatar/parameters/Ears" || got[0].Args[0] != false || got[1].Address != "/chatbox/input" || got[1].Args[0] != "hello" {
		t.Fatalf("sent %v", got)
	}

	// VRChat switches avatar and reports parameters.
	to, _ := net.DialUDP("udp", nil, c.recv.LocalAddr().(*net.UDPAddr))
	for _, m := range []Message{{"/avatar/change", []any{id}}, {ParamPrefix + "Ears", []any{true}}, {ParamPrefix + "VelocityX", []any{float32(1)}}} {
		b, _ := m.Encode()
		to.Write(b)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(evs)
		mu.Unlock()
		if v, ok := c.Param("VelocityX"); n >= 2 && ok && v == float32(1) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("events %v", evs)
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(3 * frame)
	mu.Lock()
	defer mu.Unlock()
	if len(evs) != 2 || evs[0].Type != "osc.avatar" || evs[1].Type != "osc.param" {
		t.Fatalf("events %v (VelocityX is noisy and shouldn't be published)", evs)
	}
	a := c.Avatar()
	if a.Name != "Winterpaw" || len(a.Params) != 2 || !a.Params[0].Writable || a.Params[1].Writable {
		t.Fatalf("avatar %+v", a)
	}
	if c.loadAvatar("../../etc/passwd").Name != "" {
		t.Fatal("loaded an avatar from a path")
	}
}
