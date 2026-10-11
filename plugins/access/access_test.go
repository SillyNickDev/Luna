package access

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	luna "luna/sdk/go"
	"luna/sdk/go/lunatest"
)

func said(t *testing.T, got []string, want ...string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("said %q, want %q", got, want)
	}
}

func TestManifestMatchesCode(t *testing.T) { lunatest.CheckManifest(t, New(), ".") }

func TestPronouns(t *testing.T) {
	var lookups atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/pronouns":
			json.NewEncoder(w).Encode(map[string]any{
				"hehim": map[string]any{"subject": "He", "object": "Him"}, "sheher": map[string]any{"subject": "She", "object": "Her"},
				"theythem": map[string]any{"subject": "They", "object": "Them"}, "any": map[string]any{"subject": "Any", "object": "Any", "singular": true}})
		case "/users/fox":
			lookups.Add(1)
			json.NewEncoder(w).Encode(map[string]any{"pronoun_id": "sheher", "alt_pronoun_id": nil})
		case "/users/owl":
			json.NewEncoder(w).Encode(map[string]any{"pronoun_id": "hehim", "alt_pronoun_id": "theythem"})
		case "/users/elk":
			json.NewEncoder(w).Encode(map[string]any{"pronoun_id": "any"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	pronounsAPI = srv.URL
	b := New()
	b.HTTP = srv.Client()
	h := lunatest.New(t, b, ".", nil)
	said(t, h.Chat("fox", "!pronouns"), "fox: She/Her")
	said(t, h.Chat("mink", "!pronouns @owl"), "owl: He/They")
	said(t, h.Chat("elk", "!pronouns"), "elk: Any")
	said(t, h.Chat("bat", "!pronouns"), "bat hasn't set pronouns; anyone can at pr.alejo.io")
	h.Clock.Advance(10 * time.Second)
	said(t, h.Chat("fox", "!pronouns"), "fox: She/Her")
	if lookups.Load() != 1 {
		t.Fatalf("looked fox up %d times; the answer is cached", lookups.Load())
	}
}

func TestTranslate(t *testing.T) {
	h := lunatest.New(t, New(), ".", map[string]any{"auto_translate": true})
	h.Translations["buena suerte"] = [2]string{"good luck", "ES"}
	h.Translations["good luck everyone tonight"] = [2]string{"buena suerte a todos esta noche", "EN"}
	said(t, h.Chat("fox", "!translate buena suerte"), "🌐 (ES→EN) good luck")
	h.Clock.Advance(10 * time.Second)
	said(t, h.Chat("fox", "!tr es good luck everyone tonight"), "🌐 (EN→ES) buena suerte a todos esta noche")
	h.Clock.Advance(10 * time.Second)
	said(t, h.Chat("fox", "!translate hi there"), "🌐 (EN→EN) hi there") // "hi" isn't a language code DeepL knows

	h.Translations["hola a todos, ¿cómo están?"] = [2]string{"hi everyone, how are you?", "ES"}
	said(t, h.Chat("owl", "hola a todos, ¿cómo están?"), "🌐 owl (ES): hi everyone, how are you?")
	said(t, h.Chat("owl", "hello everyone, how are you?")) // already English
	said(t, h.Chat("owl", "hola"))                         // too short
	said(t, h.Chat("luna", "🌐 owl (ES): hi everyone"))     // Luna's own line
}

func TestNoNetworkWithoutListing(t *testing.T) {
	b := luna.New() // outside a capsule (or with no [capsule].net) there's no egress socket
	if _, err := b.HTTP.Get("https://api.pronouns.alejo.io/v1/pronouns"); err == nil {
		t.Fatal("reached the network outside a capsule")
	}
}
