package main

import (
	"strings"
	"testing"

	"luna/internal/plugin"
)

func TestParseCLIArgs(t *testing.T) {
	cmd := &plugin.CLICommand{Name: "threshold",
		Args:  []plugin.Arg{{Name: "bpm", Type: "int", Optional: true}},
		Flags: []plugin.Arg{{Name: "reset", Type: "bool"}, {Name: "note", Type: "str"}}}
	v, err := parseCLIArgs(cmd, []string{"160", "--reset", "--note", "calm down"})
	if err != nil || v["bpm"] != int64(160) || v["reset"] != true || v["note"] != "calm down" {
		t.Fatalf("got %v %v", v, err)
	}
	for _, bad := range [][]string{{"fast"}, {"1", "2"}, {"--nope"}, {"--note"}} {
		if _, err := parseCLIArgs(cmd, bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestRender(t *testing.T) {
	if got := render([]byte(`{"threshold":160,"name":"hr"}`)); got != "name       hr\nthreshold  160" {
		t.Errorf("object: %q", got)
	}
	got := render([]byte(`[{"user":"fox","hugs":3},{"user":"wolf","hugs":12}]`))
	if lines := strings.Split(got, "\n"); len(lines) != 3 || !strings.HasPrefix(lines[0], "hugs") || !strings.Contains(lines[2], "wolf") {
		t.Errorf("table: %q", got)
	}
}

func TestInvisibleStripped(t *testing.T) {
	if got := strings.Map(invisible, "!h​u‮g"); got != "!hug" {
		t.Errorf("got %q", got)
	}
}
