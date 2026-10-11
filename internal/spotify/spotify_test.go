package spotify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	"luna/internal/secrets"
)

func TestTrackID(t *testing.T) {
	for in, want := range map[string]string{
		"spotify:track:4uLU6hMCjMI75M1A2tKUQC":                          "4uLU6hMCjMI75M1A2tKUQC",
		"https://open.spotify.com/track/4uLU6hMCjMI75M1A2tKUQC?si=abc":  "4uLU6hMCjMI75M1A2tKUQC",
		"https://open.spotify.com/intl-de/track/4uLU6hMCjMI75M1A2tKUQC": "4uLU6hMCjMI75M1A2tKUQC",
		"https://open.spotify.com/album/4uLU6hMCjMI75M1A2tKUQC":         "",
		"https://evil.example/track/4uLU6hMCjMI75M1A2tKUQC":             "",
		"never gonna give you up":                                       "",
	} {
		if got := TrackID(in); got != want {
			t.Errorf("%s: %q, want %q", in, got, want)
		}
	}
}

// fake is the Spotify accounts and Web API, enough for the client.
func fake(t *testing.T) (*httptest.Server, *atomic.Int32) {
	var refreshes atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		switch r.Form.Get("grant_type") {
		case "refresh_token":
			refreshes.Add(1)
			json.NewEncoder(w).Encode(map[string]any{"access_token": "fresh", "expires_in": 3600})
		case "authorization_code":
			if r.Form.Get("code") != "the-code" || r.Form.Get("code_verifier") == "" {
				http.Error(w, `{"error_description":"bad code"}`, 400)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"access_token": "a1", "refresh_token": "r1", "expires_in": 3600,
				"scope": strings.Join(Scopes, " ")})
		}
	})
	track := map[string]any{"id": "t1", "uri": "spotify:track:t1", "name": "Song", "duration_ms": 200000,
		"artists": []map[string]any{{"name": "A"}, {"name": "B"}}}
	mux.HandleFunc("/v1/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer stale" {
			http.Error(w, `{"error":{"status":401,"message":"The access token expired"}}`, 401)
			return
		}
		switch {
		case r.URL.Path == "/v1/search":
			json.NewEncoder(w).Encode(map[string]any{"tracks": map[string]any{"items": []any{track}}})
		case r.URL.Path == "/v1/tracks/t1":
			json.NewEncoder(w).Encode(track)
		case r.URL.Path == "/v1/me/player/queue" && r.Method == "POST":
			if r.URL.Query().Get("uri") != "spotify:track:t1" {
				http.Error(w, `{"error":{"message":"bad uri"}}`, 400)
				return
			}
			w.WriteHeader(204)
		case r.URL.Path == "/v1/playlists/p1/tracks":
			off := r.URL.Query().Get("offset")
			next := ""
			if off == "0" {
				next = "more"
			}
			json.NewEncoder(w).Encode(map[string]any{"items": []any{map[string]any{"track": map[string]any{"id": "x" + off}},
				map[string]any{"track": nil}}, "next": next})
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	TokenURL, APIURL = srv.URL+"/api/token", srv.URL+"/v1"
	return srv, &refreshes
}

func TestClient(t *testing.T) {
	keyring.MockInit()
	_, refreshes := fake(t)
	store := secrets.Open(t.TempDir())
	SaveToken(store, &Token{Access: "stale", Refresh: "r0", Expiry: time.Now().Add(time.Hour)})
	c, err := NewClient("cid", store)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tr, err := c.Find(ctx, "never gonna") // the stale token gets a 401, then a refresh
	if err != nil || tr.Name != "Song" || tr.Artists != "A, B" || refreshes.Load() != 1 {
		t.Fatalf("search: %+v %v (refreshes %d)", tr, err, refreshes.Load())
	}
	if saved, _ := LoadToken(store); saved.Access != "fresh" || saved.Refresh != "r0" {
		t.Fatalf("refreshed token not saved, or refresh token lost: %+v", saved)
	}
	if tr, err := c.Find(ctx, "https://open.spotify.com/track/t1"); err != nil || tr.ID != "t1" {
		t.Fatalf("link: %+v %v", tr, err)
	}
	if err := c.Queue(ctx, "spotify:track:t1"); err != nil {
		t.Fatal(err)
	}
	if ids, err := c.PlaylistTracks(ctx, "p1"); err != nil || len(ids) != 2 || !ids["x0"] || !ids["x100"] {
		t.Fatalf("playlist: %v %v", ids, err)
	}
}

func TestLogin(t *testing.T) {
	srv, _ := fake(t)
	AuthURL = srv.URL + "/authorize"
	tok, err := Login(context.Background(), "cid", func(authURL string) {
		u, _ := url.Parse(authURL)
		q := u.Query()
		if q.Get("code_challenge_method") != "S256" || q.Get("redirect_uri") != RedirectURI || q.Get("client_id") != "cid" {
			t.Errorf("auth url %s", authURL)
		}
		go func() {
			// A wrong state is refused; the right one finishes the login.
			http.Get(RedirectURI + "?code=evil&state=nope")
			http.Get(fmt.Sprintf("%s?code=the-code&state=%s", RedirectURI, url.QueryEscape(q.Get("state"))))
		}()
	})
	if err != nil || tok.Access != "a1" || tok.Refresh != "r1" {
		t.Fatalf("%+v %v", tok, err)
	}
	if _, err := Login(context.Background(), "", func(string) {}); err == nil || !strings.Contains(err.Error(), "client_id") {
		t.Fatalf("no client id: %v", err)
	}
}
