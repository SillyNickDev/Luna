package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"luna/internal/config"
	"luna/internal/twitch"
)

func auth(p paths, sub string) error {
	cfg, err := config.Load(p.config)
	if err != nil {
		return err
	}
	ctx := context.Background()
	switch sub {
	case "login":
		t, err := twitch.DeviceLogin(ctx, cfg.Twitch.ClientID, func(uri, code string) {
			fmt.Printf("Open %s and enter code %s\nWaiting for approval…\n", uri, code)
		})
		if err != nil {
			return err
		}
		if err := twitch.SaveToken(p.tokens, t); err != nil {
			return err
		}
		fmt.Printf("Logged in as %s. Restart `luna run` to connect.\n", t.Login)
	case "status":
		t, err := twitch.LoadToken(p.tokens)
		if err != nil {
			return err
		}
		fmt.Printf("broadcaster: %s (id %s)\nscopes:      %s\nexpires:     %s\n",
			t.Login, t.UserID, strings.Join(t.Scopes, " "), t.Expiry.Local().Format(time.RFC1123))
	case "logout":
		t, err := twitch.LoadToken(p.tokens)
		if err != nil {
			return err
		}
		if err := twitch.Revoke(ctx, cfg.Twitch.ClientID, t); err != nil {
			fmt.Fprintln(os.Stderr, "warning: revoking with twitch failed:", err)
		}
		if err := os.Remove(p.tokens); err != nil {
			return err
		}
		fmt.Println("Logged out.")
	default:
		return fmt.Errorf("unknown auth command %q (login, status, logout)", sub)
	}
	return nil
}
