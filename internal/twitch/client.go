package twitch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"luna/internal/secrets"
)

// Client acts as one Twitch account (broadcaster or bot).
type Client struct {
	clientID, account, userID string
	store                     *secrets.Store

	mu  sync.Mutex
	tok *Token
}

func NewClient(clientID string, store *secrets.Store, account string) (*Client, error) {
	tok, err := LoadToken(store, account)
	if err != nil {
		return nil, err
	}
	return &Client{clientID: clientID, account: account, userID: tok.UserID, store: store, tok: tok}, nil
}

func (c *Client) UserID() string { return c.userID }

// token returns an access token, refreshing it when close to expiry or when forced after a 401.
func (c *Client) token(ctx context.Context, force bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if force || time.Until(c.tok.Expiry) < 5*time.Minute {
		t, err := refresh(ctx, c.clientID, c.tok)
		if err != nil {
			return "", err
		}
		if err := SaveToken(c.store, c.account, t); err != nil {
			slog.Error("saving refreshed twitch token failed", "account", c.account, "err", err)
		}
		c.tok = t
	}
	return c.tok.Access, nil
}

func (c *Client) helix(ctx context.Context, method, path string, body, out any) error {
	var b []byte
	if body != nil {
		var err error
		if b, err = json.Marshal(body); err != nil {
			return err
		}
	}
	for attempt := 0; ; attempt++ {
		tok, err := c.token(ctx, attempt > 0)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, method, helixURL+path, bytes.NewReader(b))
		if err != nil {
			return err
		}
		req.Header.Set("Client-Id", c.clientID)
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		err = doJSON(req, out)
		var ae *APIError
		if errors.As(err, &ae) && ae.Status == 401 {
			if attempt == 0 {
				continue
			}
			return ErrAuth // a fresh token was rejected too: stop, don't loop
		}
		return err
	}
}

// SendChat posts text as this account in broadcasterID's chat. Callers go through the output guard.
func (c *Client) SendChat(ctx context.Context, broadcasterID, text string) error {
	var out struct {
		Data []struct {
			IsSent     bool `json:"is_sent"`
			DropReason *struct {
				Message string `json:"message"`
			} `json:"drop_reason"`
		} `json:"data"`
	}
	err := c.helix(ctx, "POST", "/chat/messages", map[string]string{
		"broadcaster_id": broadcasterID, "sender_id": c.userID, "message": text,
	}, &out)
	if err != nil {
		return err
	}
	if len(out.Data) == 0 || !out.Data[0].IsSent {
		reason := "unknown reason"
		if len(out.Data) > 0 && out.Data[0].DropReason != nil {
			reason = out.Data[0].DropReason.Message
		}
		return fmt.Errorf("twitch dropped the message: %s", reason)
	}
	return nil
}

// validateHourly re-checks the token every hour, as Twitch requires of apps.
func (c *Client) validateHourly(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		c.mu.Lock()
		tr := tokenResp{Access: c.tok.Access, Refresh: c.tok.Refresh}
		c.mu.Unlock()
		var ae *APIError
		if _, err := newToken(ctx, c.clientID, tr); errors.As(err, &ae) && ae.Status == 401 {
			if _, err := c.token(ctx, true); err != nil {
				slog.Error("twitch token invalid", "account", c.account, "err", err)
			}
		}
	}
}

// UserByLogin looks up a Twitch user's ID.
func (c *Client) UserByLogin(ctx context.Context, login string) (id string, err error) {
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := c.helix(ctx, "GET", "/users?login="+url.QueryEscape(login), nil, &out); err != nil {
		return "", err
	}
	if len(out.Data) == 0 {
		return "", fmt.Errorf("no twitch user %q", login)
	}
	return out.Data[0].ID, nil
}

// IsModerator reports whether userID is a moderator in this (broadcaster) account's channel.
func (c *Client) IsModerator(ctx context.Context, userID string) (bool, error) {
	var out struct {
		Data []struct{} `json:"data"`
	}
	q := "/moderation/moderators?broadcaster_id=" + url.QueryEscape(c.userID) + "&user_id=" + url.QueryEscape(userID)
	if err := c.helix(ctx, "GET", q, nil, &out); err != nil {
		return false, err
	}
	return len(out.Data) > 0, nil
}
