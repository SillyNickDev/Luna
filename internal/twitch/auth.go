// Package twitch is the Twitch adapter: device-code login, Helix, and EventSub over WebSocket.
// Tokens live only here, in the core; nothing hands them to plugins.
package twitch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"luna/internal/secrets"
)

const (
	idURL    = "https://id.twitch.tv/oauth2"
	helixURL = "https://api.twitch.tv/helix"
)

// Scopes per account. The broadcaster's cover each subscribed event, chat when there's
// no bot account, and channel:bot so a bot account may chat without being a mod.
var Scopes = map[string][]string{
	"broadcaster": {
		"user:read:chat", "user:write:chat", "moderator:read:followers",
		"channel:read:subscriptions", "bits:read", "channel:read:redemptions", "channel:bot",
		"moderation:read",
		// the core bundle's moderation, shoutouts, announcements and whispers
		"moderator:manage:banned_users", "moderator:manage:chat_messages", "moderator:manage:warnings",
		"moderator:manage:shield_mode", "moderator:manage:shoutouts", "moderator:manage:announcements",
		"user:manage:whispers",
		// Stream tools: title and category, markers, clips, ads, raids
		"channel:manage:broadcast", "clips:edit", "channel:read:ads", "channel:manage:ads", "channel:manage:raids",
		// Polls: native polls and predictions
		"channel:manage:polls", "channel:manage:predictions",
	},
	"bot": {"user:read:chat", "user:write:chat", "user:bot"},
}

// ErrAuth means the token was rejected and can't be refreshed: log in again.
var ErrAuth = errors.New("twitch rejected the login; run: luna auth login [broadcaster|bot]")

type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("twitch: %d %s", e.Status, e.Message) }

type Token struct {
	Access  string    `json:"access_token"`
	Refresh string    `json:"refresh_token"`
	Expiry  time.Time `json:"expiry"`
	Scopes  []string  `json:"scopes"`
	UserID  string    `json:"user_id"`
	Login   string    `json:"login"`
}

var hc = &http.Client{Timeout: 15 * time.Second}

func doJSON(req *http.Request, out any) error {
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body := io.LimitReader(resp.Body, 1<<20)
	if resp.StatusCode >= 300 {
		var e struct {
			Message string `json:"message"`
		}
		json.NewDecoder(body).Decode(&e)
		return &APIError{resp.StatusCode, e.Message}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(body).Decode(out)
}

func postForm(ctx context.Context, u string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, "POST", u, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return doJSON(req, out)
}

type tokenResp struct {
	Access  string `json:"access_token"`
	Refresh string `json:"refresh_token"`
}

// newToken validates a fresh access token and records whose it is.
func newToken(ctx context.Context, clientID string, tr tokenResp) (*Token, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", idURL+"/validate", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "OAuth "+tr.Access)
	var v struct {
		ClientID  string   `json:"client_id"`
		Login     string   `json:"login"`
		Scopes    []string `json:"scopes"`
		UserID    string   `json:"user_id"`
		ExpiresIn int      `json:"expires_in"`
	}
	if err := doJSON(req, &v); err != nil {
		return nil, err
	}
	if v.ClientID != clientID || v.UserID == "" {
		return nil, errors.New("twitch returned a token for a different app")
	}
	return &Token{
		Access: tr.Access, Refresh: tr.Refresh,
		Expiry: time.Now().Add(time.Duration(v.ExpiresIn) * time.Second),
		Scopes: v.Scopes, UserID: v.UserID, Login: v.Login,
	}, nil
}

// DeviceLogin runs Twitch's device code flow; show tells the user where to approve.
func DeviceLogin(ctx context.Context, clientID string, scopes []string, show func(uri, code string)) (*Token, error) {
	scope := strings.Join(scopes, " ")
	var dc struct {
		DeviceCode string `json:"device_code"`
		ExpiresIn  int    `json:"expires_in"`
		Interval   int    `json:"interval"`
		UserCode   string `json:"user_code"`
		URI        string `json:"verification_uri"`
	}
	if err := postForm(ctx, idURL+"/device", url.Values{"client_id": {clientID}, "scopes": {scope}}, &dc); err != nil {
		return nil, err
	}
	show(dc.URI, dc.UserCode)
	ctx, cancel := context.WithTimeout(ctx, time.Duration(dc.ExpiresIn)*time.Second)
	defer cancel()
	interval := time.Duration(max(dc.Interval, 1)) * time.Second
	for {
		select {
		case <-time.After(interval):
		case <-ctx.Done():
			return nil, errors.New("login code expired before it was approved")
		}
		var tr tokenResp
		err := postForm(ctx, idURL+"/token", url.Values{
			"client_id": {clientID}, "scopes": {scope}, "device_code": {dc.DeviceCode},
			"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"},
		}, &tr)
		var ae *APIError
		switch {
		case errors.As(err, &ae) && ae.Message == "authorization_pending":
			continue
		case errors.As(err, &ae) && ae.Message == "slow_down":
			interval += 5 * time.Second
			continue
		case err != nil:
			return nil, err
		}
		return newToken(ctx, clientID, tr)
	}
}

// refresh swaps a refresh token for a new pair. Public clients get single-use refresh tokens.
func refresh(ctx context.Context, clientID string, t *Token) (*Token, error) {
	var tr tokenResp
	err := postForm(ctx, idURL+"/token", url.Values{
		"client_id": {clientID}, "grant_type": {"refresh_token"}, "refresh_token": {t.Refresh},
	}, &tr)
	var ae *APIError
	if errors.As(err, &ae) && (ae.Status == 400 || ae.Status == 401) {
		return nil, ErrAuth
	}
	if err != nil {
		return nil, err
	}
	return newToken(ctx, clientID, tr)
}

// Revoke invalidates the token with Twitch, not just locally.
func Revoke(ctx context.Context, clientID string, t *Token) error {
	return postForm(ctx, idURL+"/revoke", url.Values{"client_id": {clientID}, "token": {t.Access}}, nil)
}

func LoadToken(s *secrets.Store, account string) (*Token, error) {
	b, err := s.Get(account)
	if errors.Is(err, secrets.ErrNotFound) {
		return nil, fmt.Errorf("%s account not logged in to Twitch; run: luna auth login %s", account, account)
	}
	if err != nil {
		return nil, err
	}
	var t Token
	return &t, json.Unmarshal(b, &t)
}

func SaveToken(s *secrets.Store, account string, t *Token) error {
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return s.Set(account, b)
}
