package guard

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// Whatever a plugin or a relayed viewer message contains, what Luna sends to chat can't be
// a slash command, has no control characters, and fits Twitch's limit.
func FuzzClean(f *testing.F) {
	for _, s := range []string{"/ban fox", " .timeout x", "\x00/me", "hello\nworld", strings.Repeat("é", 600)} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		out := Clean(s)
		if strings.HasPrefix(out, "/") || strings.HasPrefix(out, ".") {
			t.Fatalf("%q became the command %q", s, out)
		}
		if utf8.RuneCountInString(out) > maxLen+1 { // +1: the zero-width space in front of an escaped command
			t.Fatalf("%d runes", utf8.RuneCountInString(out))
		}
		for _, r := range out {
			if unicode.IsControl(r) {
				t.Fatalf("control character %U in %q", r, out)
			}
		}
	})
}
