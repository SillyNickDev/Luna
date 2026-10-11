package spotify

import (
	"strings"
	"testing"
)

// Track and playlist links come from chat: parsing never panics, and a track ID never
// carries path or query characters into the API URL.
func FuzzIDs(f *testing.F) {
	for _, s := range []string{"https://open.spotify.com/track/abc?si=1", "spotify:track:x", "open.spotify.com/track/../x", "%"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if id := TrackID(s); strings.ContainsAny(id, "/?#") {
			t.Fatalf("track id %q from %q", id, s)
		}
		if id := PlaylistID(s); strings.ContainsAny(id, "/?#%") {
			t.Fatalf("playlist id %q from %q", id, s)
		}
	})
}
