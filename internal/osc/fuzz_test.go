package osc

import "testing"

// OSC packets arrive over UDP: decoding must refuse junk, never panic.
func FuzzDecode(f *testing.F) {
	b, _ := Message{"/avatar/parameters/Ears", []any{true, 3, float32(0.5), "hi"}}.Encode()
	f.Add(b)
	f.Add(append([]byte("#bundle\x00"), make([]byte, 12)...))
	f.Fuzz(func(t *testing.T, p []byte) {
		ms, err := Decode(p)
		if err != nil {
			return
		}
		for _, m := range ms {
			if len(m.Address) == 0 || m.Address[0] != '/' {
				t.Fatalf("decoded a bad address %q", m.Address)
			}
		}
	})
}
