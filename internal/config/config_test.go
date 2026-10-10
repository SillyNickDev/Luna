package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBadReloadKeepsLastGood(t *testing.T) {
	path := filepath.Join(t.TempDir(), "luna.toml")
	write := func(s string) {
		if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("[chat]\nrate_limit = 18\n")
	h, err := NewHolder(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		"[chat]\nrate_limit = 0\n",
		"[chat]\nblocklist = [\"(\"]\n",
		"[chat]\nrate_limt = 5\n", // typo
		"[perms.same_rank_override]\nmods = true\n",
	} {
		write(bad)
		if h.Reload() == nil {
			t.Errorf("accepted bad config %q", bad)
		}
	}
	if got := h.Get().Chat.RateLimit; got != 18 {
		t.Fatalf("rate_limit = %d after bad reloads, want 18", got)
	}
}
