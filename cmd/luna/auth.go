package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"luna/internal/config"
	"luna/internal/secrets"
	"luna/internal/twitch"
)

var accounts = []string{"broadcaster", "bot"}

func auth(p paths, sub string, rest []string) error {
	cfg, err := config.Load(p.config)
	if err != nil {
		return err
	}
	which := accounts
	if len(rest) == 1 && twitch.Scopes[rest[0]] != nil {
		which = rest[:1]
	} else if len(rest) > 0 {
		return fmt.Errorf("unknown account %q (broadcaster or bot)", strings.Join(rest, " "))
	}
	store := secrets.Open(p.data)
	ctx := context.Background()
	switch sub {
	case "login":
		account := "broadcaster"
		if len(rest) == 1 {
			account = rest[0]
		}
		if account == "bot" {
			fmt.Println("Log in with the BOT account in your browser (a private window helps).")
		}
		t, err := twitch.DeviceLogin(ctx, cfg.Twitch.ClientID, twitch.Scopes[account], func(uri, code string) {
			fmt.Printf("Open %s and enter code %s\nWaiting for approval…\n", uri, code)
		})
		if err != nil {
			return err
		}
		if account == "bot" {
			if bc, err := twitch.LoadToken(store, "broadcaster"); err == nil && bc.UserID == t.UserID {
				return errors.New("that's the broadcaster account; log in to the bot account instead")
			}
		}
		if err := twitch.SaveToken(store, account, t); err != nil {
			return err
		}
		where := "the OS keyring"
		if !store.UsesKeyring() {
			where = "an encrypted file (no OS keyring found)"
		}
		fmt.Printf("Logged in %s as %s; token saved in %s. Restart `luna run` to connect.\n", account, t.Login, where)
	case "status":
		for _, a := range which {
			t, err := twitch.LoadToken(store, a)
			if err != nil {
				fmt.Printf("%-12s not logged in\n", a+":")
				continue
			}
			fmt.Printf("%-12s %s (id %s), expires %s\n%-12s %s\n", a+":", t.Login, t.UserID,
				t.Expiry.Local().Format(time.RFC1123), "", strings.Join(t.Scopes, " "))
		}
	case "logout":
		for _, a := range which {
			t, err := twitch.LoadToken(store, a)
			if err != nil {
				continue
			}
			if err := twitch.Revoke(ctx, cfg.Twitch.ClientID, t); err != nil {
				fmt.Fprintln(os.Stderr, "warning: revoking with twitch failed:", err)
			}
			if err := store.Delete(a); err != nil {
				return err
			}
			fmt.Println("Logged out", a)
		}
	default:
		return fmt.Errorf("unknown auth command %q (login, status, logout)", sub)
	}
	return nil
}
