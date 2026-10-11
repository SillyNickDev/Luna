package plugin

import (
	"testing"

	"luna/internal/osc"
	"luna/internal/plugin/pb"
)

// OSC values come from plugins: converting must refuse, never panic, and produce only the
// types the OSC encoder can send.
func FuzzOSCValue(f *testing.F) {
	for _, v := range []string{"true", "3", "0.5", `"hi"`, "1e400", "{}", "[1]", "null"} {
		f.Add("/avatar/parameters/Speed", v, "")
	}
	av := osc.Avatar{Params: []osc.Param{{Name: "Speed", Type: "Float", Writable: true}}}
	f.Fuzz(func(t *testing.T, addr, val, typ string) {
		v, err := oscValue(&pb.OscRequest{Address: addr, ValueJson: []byte(val), Type: typ}, av)
		if err != nil {
			return
		}
		if _, err := (osc.Message{Address: "/x", Args: []any{v}}).Encode(); err != nil {
			t.Fatalf("%s %q became %v (%T), which can't be sent: %v", val, typ, v, v, err)
		}
	})
}
