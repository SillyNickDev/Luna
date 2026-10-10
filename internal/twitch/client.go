package twitch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// Client acts as the broadcaster account.
// ponytail: one account; the separate bot account (Security tab: split accounts) comes with `luna auth login bot`.
type Client struct {
	clientID, path, userID string

	mu  sync.Mutex
	tok *Token
}

func NewClient(clientID, tokenPath string) (*Client, error) {
	tok, err := LoadToken(tokenPath)
	if err != nil {
		return nil, err
	}
	return &Client{clientID: clientID, path: tokenPath, userID: tok.UserID, tok: tok}, nil
}

// token returns an access token, refreshing it when close to expiry or when forced after a 401.
func (c *Client) token(ctx context.Context, force bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if force || time.Until(c.tok.Expiry) < 5*time.Minute {
		t, err := refresh(ctx, c.clientID, c.tok)
		if err != nil {
			return "", err
		}
		if err := SaveToken(c.path, t); err != nil {
			slog.Error("saving refreshed twitch token failed", "err", err)
		}
		c.tok = t
	}
	return c.tok.Access, nil
}

func (c *Client) helix(ctx context.Context, method, path string, body, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
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

// SendChat posts a message to the broadcaster's chat. Callers go through the output guard.
func (c *Client) SendChat(ctx context.Context, text string) error {
	var out struct {
		Data []struct {
			IsSent     bool `json:"is_sent"`
			DropReason *struct {
				Message string `json:"message"`
			} `json:"drop_reason"`
		} `json:"data"`
	}
	err := c.helix(ctx, "POST", "/chat/messages", map[string]string{
		"broadcaster_id": c.userID, "sender_id": c.userID, "message": text,
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
				slog.Error("twitch token invalid", "err", err)
			}
		}
	}
}
