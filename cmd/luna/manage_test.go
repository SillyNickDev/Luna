package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigSet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "luna.toml")
	os.WriteFile(path, []byte("# my settings\n[chat]\nrate_limit = 20 # mod limit is higher\nblocklist = [\"spam\"]\n"), 0o600)
	if err := configSet(path, "chat.rate_limit", "18"); err != nil {
		t.Fatal(err)
	}
	if err := configSet(path, "plugins.hrc.threshold", "150"); err != nil {
		t.Fatal(err)
	}
	if err := configSet(path, "plugins.hrc.message", "hi there"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	got := string(b)
	for _, want := range []string{"# my settings", "rate_limit = 18", `blocklist = ["spam"]`, "[plugins.hrc]\nthreshold = 150\nmessage = \"hi there\""} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if err := configSet(path, "chat.rate_limit", "9000"); err == nil {
		t.Error("invalid value saved")
	}
	if b2, _ := os.ReadFile(path); string(b2) != got {
		t.Error("a rejected set changed the file")
	}
}
