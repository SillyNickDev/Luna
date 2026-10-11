package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"luna/internal/config"
	"luna/internal/deepl"
	"luna/internal/secrets"
	"luna/internal/spotify"
	"luna/internal/twitch"
)

var accounts = []string{"broadcaster", "bot"}

func auth(p paths, sub string, rest []string) error {
	cfg, err := config.Load(p.config)
	if err != nil {
		return err
	}
	if len(rest) == 1 && rest[0] == "spotify" {
		return spotifyAuth(p, cfg, sub)
	}
	if len(rest) == 1 && rest[0] == "deepl" {
		return deeplAuth(p, sub)
	}
	which := accounts
	if len(rest) == 1 && twitch.Scopes[rest[0]] != nil {
		which = rest[:1]
	} else if len(rest) > 0 {
		return fmt.Errorf("unknown account %q (broadcaster, bot, spotify or deepl)", strings.Join(rest, " "))
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
			var missing []string
			for _, sc := range twitch.Scopes[a] {
				if !slices.Contains(t.Scopes, sc) {
					missing = append(missing, sc)
				}
			}
			if len(missing) > 0 {
				fmt.Printf("%-12s missing %s: run luna auth login %s\n", "", strings.Join(missing, " "), a)
			}
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

// spotifyAuth is luna auth login|status|logout spotify.
func spotifyAuth(p paths, cfg *config.Config, sub string) error {
	store := secrets.Open(p.data)
	switch sub {
	case "login":
		t, err := spotify.Login(context.Background(), cfg.Spotify.ClientID, func(u string) {
			fmt.Printf("Open this link and allow Luna (it waits up to 5 minutes):\n%s\n", u)
			exec.Command("xdg-open", u).Start() // best effort; the link above always works
		})
		if err != nil {
			return err
		}
		if err := spotify.SaveToken(store, t); err != nil {
			return err
		}
		fmt.Println("Logged in to Spotify. Restart `luna run` to connect.")
	case "status":
		t, err := spotify.LoadToken(store)
		if err != nil {
			fmt.Printf("%-12s not logged in\n", "spotify:")
			return nil
		}
		fmt.Printf("%-12s logged in, scopes %s\n", "spotify:", strings.Join(t.Scopes, " "))
	case "logout":
		return store.Delete("spotify")
	default:
		return errors.New("usage: luna auth login|status|logout spotify")
	}
	return nil
}

// deeplAuth is luna auth login|status|logout deepl: the API key, kept like a login.
func deeplAuth(p paths, sub string) error {
	store := secrets.Open(p.data)
	switch sub {
	case "login":
		fmt.Print("Paste your DeepL API key (from deepl.com/your-account/keys; it won't show): ")
		key, err := readSecret()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if _, _, err := deepl.Translate(ctx, key, "hello", "DE"); err != nil {
			return fmt.Errorf("that key doesn't work: %w", err)
		}
		if err := store.Set("deepl", []byte(key)); err != nil {
			return err
		}
		fmt.Println("DeepL key saved and working. Restart `luna run` to use it.")
	case "status":
		if _, err := store.Get("deepl"); err != nil {
			fmt.Printf("%-12s no key\n", "deepl:")
		} else {
			fmt.Printf("%-12s key saved\n", "deepl:")
		}
	case "logout":
		return store.Delete("deepl")
	default:
		return errors.New("usage: luna auth login|status|logout deepl")
	}
	return nil
}
