package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
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

func TestWatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "luna.toml")
	os.WriteFile(path, []byte("[chat]\nrate_limit = 18\n"), 0o600)
	h, err := NewHolder(path)
	if err != nil {
		t.Fatal(err)
	}
	reloaded := make(chan error, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := h.Watch(ctx, func(err error) { reloaded <- err }); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(path, []byte("[chat]\nrate_limit = 5\n"), 0o600)
	select {
	case err := <-reloaded:
		if err != nil || h.Get().Chat.RateLimit != 5 {
			t.Fatalf("reload: err=%v rate_limit=%d", err, h.Get().Chat.RateLimit)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no reload after the file changed")
	}
}
