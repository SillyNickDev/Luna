// Package spotify is the daemon's Spotify integration for song requests. The token lives
// here, in the core, like Twitch's: plugins only get the calls they're permitted.
package spotify

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"luna/internal/secrets"
)

// Endpoints; tests point them at a fake.
var (
	AuthURL  = "https://accounts.spotify.com/authorize"
	TokenURL = "https://accounts.spotify.com/api/token"
	APIURL   = "https://api.spotify.com/v1"
)

// RedirectURI must be added to the Spotify app's settings exactly like this.
const RedirectURI = "http://127.0.0.1:43219/callback"

var Scopes = []string{"user-read-currently-playing", "user-read-playback-state", "user-modify-playback-state", "playlist-read-private"}

var ErrNotLoggedIn = errors.New("spotify isn't logged in; run: luna auth login spotify")

type Token struct {
	Access  string    `json:"access_token"`
	Refresh string    `json:"refresh_token"`
	Expiry  time.Time `json:"expiry"`
	Scopes  []string  `json:"scopes"`
}

var hc = &http.Client{Timeout: 15 * time.Second}

type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("spotify: %d %s", e.Status, e.Message) }

func do(req *http.Request, out any) error {
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
			Desc string `json:"error_description"`
		}
		json.Unmarshal(b, &e)
		msg := e.Error.Message + e.Desc
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		return &APIError{resp.StatusCode, msg}
	}
	if out == nil || len(b) == 0 {
		return nil
	}
	return json.Unmarshal(b, out)
}

func tokenRequest(ctx context.Context, form url.Values) (*Token, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var tr struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
		Expires int    `json:"expires_in"`
		Scope   string `json:"scope"`
	}
	if err := do(req, &tr); err != nil {
		return nil, err
	}
	return &Token{Access: tr.Access, Refresh: tr.Refresh, Expiry: time.Now().Add(time.Duration(tr.Expires) * time.Second),
		Scopes: strings.Fields(tr.Scope)}, nil
}

// Login runs Spotify's authorization-code flow with PKCE. Spotify has no device codes, so
// Luna listens on 127.0.0.1:43219 for the single browser redirect, then stops.
func Login(ctx context.Context, clientID string, open func(url string)) (*Token, error) {
	if clientID == "" {
		return nil, errors.New("set [spotify] client_id in luna.toml first: create an app at developer.spotify.com with redirect URI " + RedirectURI)
	}
	buf := make([]byte, 64)
	rand.Read(buf)
	verifier := base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(verifier))
	state := base64.RawURLEncoding.EncodeToString(buf[:16])
	u, _ := url.Parse(RedirectURI)
	ln, err := net.Listen("tcp", u.Host)
	if err != nil {
		return nil, fmt.Errorf("can't listen for Spotify's redirect on %s: %w", u.Host, err)
	}
	defer ln.Close()
	codes := make(chan string, 1)
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != u.Path || q.Get("state") != state {
			http.Error(w, "not this login", http.StatusBadRequest)
			return
		}
		if e := q.Get("error"); e != "" {
			fmt.Fprintf(w, "Spotify said: %s. You can close this tab.", e)
			select {
			case codes <- "":
			default:
			}
			return
		}
		fmt.Fprint(w, "Luna is logged in to Spotify. You can close this tab.")
		select {
		case codes <- q.Get("code"):
		default:
		}
	})}
	go srv.Serve(ln)
	defer srv.Close()
	open(AuthURL + "?" + url.Values{"client_id": {clientID}, "response_type": {"code"}, "redirect_uri": {RedirectURI},
		"code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])},
		"scope": {strings.Join(Scopes, " ")}, "state": {state}}.Encode())
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	var code string
	select {
	case code = <-codes:
	case <-ctx.Done():
		return nil, errors.New("no answer from the browser within 5 minutes")
	}
	if code == "" {
		return nil, errors.New("the login was declined")
	}
	return tokenRequest(ctx, url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {RedirectURI},
		"client_id": {clientID}, "code_verifier": {verifier}})
}

func LoadToken(s *secrets.Store) (*Token, error) {
	b, err := s.Get("spotify")
	if err != nil {
		return nil, ErrNotLoggedIn
	}
	var t Token
	return &t, json.Unmarshal(b, &t)
}

func SaveToken(s *secrets.Store, t *Token) error {
	b, _ := json.Marshal(t)
	return s.Set("spotify", b)
}

// Client calls the Web API as the logged-in Spotify account, refreshing its token as needed.
type Client struct {
	clientID string
	store    *secrets.Store
	mu       sync.Mutex
	tok      *Token
}

func NewClient(clientID string, store *secrets.Store) (*Client, error) {
	t, err := LoadToken(store)
	if err != nil {
		return nil, err
	}
	return &Client{clientID: clientID, store: store, tok: t}, nil
}

func (c *Client) token(ctx context.Context, force bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !force && time.Until(c.tok.Expiry) > time.Minute {
		return c.tok.Access, nil
	}
	t, err := tokenRequest(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {c.tok.Refresh}, "client_id": {c.clientID}})
	if err != nil {
		return "", fmt.Errorf("refreshing the Spotify login failed (run luna auth login spotify): %w", err)
	}
	if t.Refresh == "" {
		t.Refresh = c.tok.Refresh // Spotify may keep the old refresh token
	}
	c.tok = t
	if err := SaveToken(c.store, t); err != nil {
		return "", err
	}
	return t.Access, nil
}

func (c *Client) api(ctx context.Context, method, path string, q url.Values, out any) error {
	for attempt := 0; ; attempt++ {
		tok, err := c.token(ctx, attempt > 0)
		if err != nil {
			return err
		}
		u := APIURL + path
		if len(q) > 0 {
			u += "?" + q.Encode()
		}
		req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(nil))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		err = do(req, out)
		var ae *APIError
		if errors.As(err, &ae) && ae.Status == 401 && attempt == 0 {
			continue
		}
		return err
	}
}

type Track struct {
	ID         string `json:"id"`
	URI        string `json:"uri"`
	Name       string `json:"name"`
	Artists    string `json:"artists"` // comma-separated
	DurationMS int    `json:"duration_ms"`
	Explicit   bool   `json:"explicit"`
}

type apiTrack struct {
	ID         string `json:"id"`
	URI        string `json:"uri"`
	Name       string `json:"name"`
	DurationMS int    `json:"duration_ms"`
	Explicit   bool   `json:"explicit"`
	Artists    []struct {
		Name string `json:"name"`
	} `json:"artists"`
}

func (t apiTrack) track() *Track {
	var names []string
	for _, a := range t.Artists {
		names = append(names, a.Name)
	}
	return &Track{ID: t.ID, URI: t.URI, Name: t.Name, Artists: strings.Join(names, ", "), DurationMS: t.DurationMS, Explicit: t.Explicit}
}

// TrackID reads a track ID from a Spotify link or URI ("" if s isn't one).
func TrackID(s string) string {
	s = strings.TrimSpace(s)
	if id, ok := strings.CutPrefix(s, "spotify:track:"); ok {
		return id
	}
	u, err := url.Parse(s)
	if err != nil || u.Host != "open.spotify.com" {
		return ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i := 0; i+1 < len(parts); i++ {
		if parts[i] == "track" {
			return parts[i+1]
		}
	}
	return ""
}

// PlaylistID reads a playlist ID from a link, URI or the bare ID.
func PlaylistID(s string) string {
	s = strings.TrimSpace(s)
	if id, ok := strings.CutPrefix(s, "spotify:playlist:"); ok {
		return id
	}
	if u, err := url.Parse(s); err == nil && u.Host == "open.spotify.com" {
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		for i := 0; i+1 < len(parts); i++ {
			if parts[i] == "playlist" {
				return parts[i+1]
			}
		}
	}
	return s
}

// Find is the track a request names: a Spotify link, or the top search result.
func (c *Client) Find(ctx context.Context, query string) (*Track, error) {
	if id := TrackID(query); id != "" {
		var t apiTrack
		if err := c.api(ctx, "GET", "/tracks/"+url.PathEscape(id), nil, &t); err != nil {
			return nil, err
		}
		return t.track(), nil
	}
	var out struct {
		Tracks struct {
			Items []apiTrack `json:"items"`
		} `json:"tracks"`
	}
	if err := c.api(ctx, "GET", "/search", url.Values{"q": {query}, "type": {"track"}, "limit": {"1"}}, &out); err != nil {
		return nil, err
	}
	if len(out.Tracks.Items) == 0 {
		return nil, nil
	}
	return out.Tracks.Items[0].track(), nil
}

// Queue adds a track after the current one. Spotify needs Premium and an active player.
func (c *Client) Queue(ctx context.Context, uri string) error {
	return c.api(ctx, "POST", "/me/player/queue", url.Values{"uri": {uri}}, nil)
}

func (c *Client) Skip(ctx context.Context) error {
	return c.api(ctx, "POST", "/me/player/next", nil, nil)
}

type Playing struct {
	Track     *Track `json:"track"` // nil when nothing is playing
	ProgressM int    `json:"progress_ms"`
	IsPlaying bool   `json:"is_playing"`
}

func (c *Client) Playing(ctx context.Context) (*Playing, error) {
	var out struct {
		Item      *apiTrack `json:"item"`
		Progress  int       `json:"progress_ms"`
		IsPlaying bool      `json:"is_playing"`
	}
	if err := c.api(ctx, "GET", "/me/player/currently-playing", nil, &out); err != nil {
		return nil, err
	}
	p := &Playing{ProgressM: out.Progress, IsPlaying: out.IsPlaying}
	if out.Item != nil {
		p.Track = out.Item.track()
	}
	return p, nil
}

// PlaylistTracks is the set of track IDs in a playlist, for the allowlist rule.
func (c *Client) PlaylistTracks(ctx context.Context, id string) (map[string]bool, error) {
	ids := map[string]bool{}
	q := url.Values{"fields": {"items(track(id)),next"}, "limit": {"100"}}
	for offset := 0; offset < 10000; offset += 100 {
		q.Set("offset", fmt.Sprint(offset))
		var page struct {
			Items []struct {
				Track *struct {
					ID string `json:"id"`
				} `json:"track"`
			} `json:"items"`
			Next string `json:"next"`
		}
		if err := c.api(ctx, "GET", "/playlists/"+url.PathEscape(id)+"/tracks", q, &page); err != nil {
			return nil, err
		}
		for _, it := range page.Items {
			if it.Track != nil {
				ids[it.Track.ID] = true
			}
		}
		if page.Next == "" {
			break
		}
	}
	return ids, nil
}
