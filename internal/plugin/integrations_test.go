package plugin

import (
	"testing"
	"time"

	"luna/internal/osc"
	"luna/internal/plugin/pb"
)

func TestOSCPermissions(t *testing.T) {
	m := &Manifest{}
	m.Permissions.OSC = []string{"send:/avatar/parameters/Ears", "send:/avatar/parameters/Fx*"}
	for addr, ok := range map[string]bool{"/avatar/parameters/Ears": true, "/avatar/parameters/Ears2": false,
		"/avatar/parameters/FxSparkle": true, "/avatar/parameters/Fx/../Ears": false, "/chatbox/input": false,
		"/avatar/parameters/Fx*": false, "/avatar/parameters/Fx Spark": false} {
		if m.AllowsOSCSend(addr) != ok {
			t.Errorf("%s: want %v", addr, ok)
		}
	}
}

func TestOSCValue(t *testing.T) {
	av := osc.Avatar{Params: []osc.Param{{Name: "Speed", Type: "Float", Writable: true}, {Name: "Outfit", Type: "Int", Writable: true}}}
	for _, c := range []struct {
		addr, val, typ string
		want           any
	}{
		{"/avatar/parameters/Ears", "true", "", true},
		{"/avatar/parameters/Speed", "1", "", float32(1)}, // the avatar says Float
		{"/avatar/parameters/Outfit", "3", "", 3},
		{"/avatar/parameters/Other", "0.5", "", float32(0.5)},
		{"/avatar/parameters/Other", "2", "float", float32(2)},
		{"/avatar/parameters/Other", "1", "bool", true},
		{"/avatar/parameters/Other", `"hi"`, "", "hi"},
	} {
		got, err := oscValue(&pb.OscRequest{Address: c.addr, ValueJson: []byte(c.val), Type: c.typ}, av)
		if err != nil || got != c.want {
			t.Errorf("%s=%s (%s): %v %T %v", c.addr, c.val, c.typ, got, got, err)
		}
	}
	for _, bad := range []string{"1.5", "1e12", "{}", "[1]"} {
		if v, err := oscValue(&pb.OscRequest{Address: "/avatar/parameters/Outfit", ValueJson: []byte(bad)}, av); err == nil {
			t.Errorf("%s became %v", bad, v)
		}
	}
}

func TestHRHistory(t *testing.T) {
	h := &HR{}
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	for i := range 90 {
		h.Add(t0.Add(time.Duration(i)*time.Minute), 60+i)
	}
	s := &server{h: &Host{HR: h}, p: Plugin{Manifest: &Manifest{}}}
	s.p.Manifest.Permissions.Events = []string{"hr.*"}
	r, err := s.Hr(t.Context(), &pb.HrRequest{Minutes: 5})
	if err != nil || r.Bpm != 149 || len(r.History) != 6 || r.History[0].Bpm != 144 {
		t.Fatalf("%v %v", r, err)
	}
	if len(h.samples) != 61 {
		t.Fatalf("kept %d samples, want the last hour", len(h.samples))
	}
	s.p.Manifest.Permissions.Events = nil
	if _, err := s.Hr(t.Context(), &pb.HrRequest{}); err == nil {
		t.Fatal("hr without hr.update in events")
	}
}
