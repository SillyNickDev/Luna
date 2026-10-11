package bundle

import "testing"

// Moderation folds every chat message before matching: it must never panic or blow up.
func FuzzFold(f *testing.F) {
	for _, s := range []string{"Fr33 V-bucks @ evil.com", "ｆｒｅｅ", "​hi‮", "ﬁ"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if out := fold(s); len(out) > 4*len(s)+16 {
			t.Fatalf("fold grew %d bytes into %d", len(s), len(out))
		}
	})
}
