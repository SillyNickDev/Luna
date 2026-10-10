package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"luna/internal/config"
	"luna/internal/event"
	"luna/internal/perms"
	"luna/internal/store"
	"luna/internal/twitch"
)

func loadRanks(st *store.Store) (*perms.Ranks, error) {
	rows, err := st.Ranks()
	if err != nil {
		return nil, err
	}
	kept := map[string]perms.Rank{}
	for _, r := range rows {
		if rank, err := perms.ParseRank(r.Rank); err == nil {
			kept[r.UserID] = rank
		}
	}
	return perms.NewRanks(kept), nil
}

func setRank(ranks *perms.Ranks, st *store.Store, id, login string, rank perms.Rank, by string) error {
	ranks.Set(id, rank)
	row := store.RankRow{UserID: id, Login: login, SetBy: by}
	if rank > perms.Moderator {
		row.Rank = rank.String()
	}
	return st.SetRank(row)
}

// withRanks stamps the sender's rank on chat events, makes a lead_moderator badge
// a head mod, and drops kept ranks when Twitch unmods someone. Simulated events never change ranks.
func withRanks(ranks *perms.Ranks, st *store.Store, publish func(event.Event)) func(event.Event) {
	return func(ev event.Event) {
		var d struct {
			User struct {
				ID    string   `json:"id"`
				Login string   `json:"login"`
				Roles []string `json:"roles"`
			} `json:"user"`
		}
		json.Unmarshal(ev.Data, &d)
		u := d.User
		switch ev.Type {
		case "twitch.chat.message":
			if !ev.Simulated && slices.Contains(u.Roles, "lead_moderator") && ranks.Get(u.ID) < perms.HeadMod {
				slog.Info("lead moderator badge seen; now head mod", "login", u.Login)
				if err := setRank(ranks, st, u.ID, u.Login, perms.HeadMod, "lead_moderator badge"); err != nil {
					slog.Error("saving rank failed", "err", err)
				}
			}
			var m map[string]json.RawMessage
			var um map[string]any
			if json.Unmarshal(ev.Data, &m) == nil && json.Unmarshal(m["user"], &um) == nil {
				um["rank"] = ranks.Resolve(u.ID, u.Roles).String()
				m["user"], _ = json.Marshal(um)
				ev.Data, _ = json.Marshal(m)
			}
		case "twitch.channel.moderator.remove":
			if !ev.Simulated && ranks.Get(u.ID) > perms.Moderator {
				slog.Info("unmodded on twitch; luna rank dropped", "login", u.Login)
				if err := setRank(ranks, st, u.ID, u.Login, perms.Viewer, "twitch unmod"); err != nil {
					slog.Error("saving rank failed", "err", err)
				}
			}
		}
		publish(ev)
	}
}

func policyFrom(c *config.Config) *perms.Policy {
	p := perms.DefaultPolicy()
	for k, v := range c.Perms.SameRankOverride {
		if r, err := perms.ParseRank(k); err == nil {
			p.SameRankOverride[r] = v
		}
	}
	return p
}

// permsCmd serves `luna perms show | set <user> <rank> | check <user> <capability>`.
func permsCmd(ctx context.Context, args []string, ranks *perms.Ranks, st *store.Store, tw *twitch.Client, policy *perms.Policy) (string, error) {
	if len(args) == 0 {
		args = []string{"show"}
	}
	switch {
	case args[0] == "show" && len(args) == 1:
		rows, err := st.Ranks()
		if err != nil {
			return "", err
		}
		var b strings.Builder
		b.WriteString("broadcaster and moderators come from Twitch; Luna keeps:\n")
		if len(rows) == 0 {
			b.WriteString("  no head mods or Luna mods yet\n")
		}
		for _, r := range rows {
			fmt.Fprintf(&b, "  %-9s %-25s set by %s on %s\n", r.Rank, r.Login, r.SetBy, r.Updated.Format(time.DateOnly))
		}
		return strings.TrimRight(b.String(), "\n"), nil

	case (args[0] == "set" || args[0] == "check") && len(args) == 3:
		if tw == nil {
			return "", errors.New("needs twitch: run luna auth login, then restart luna run")
		}
		login := strings.ToLower(strings.TrimPrefix(args[1], "@"))
		id, err := tw.UserByLogin(ctx, login)
		if err != nil {
			return "", err
		}
		isMod, err := tw.IsModerator(ctx, id)
		if err != nil {
			return "", err
		}
		roles := []string{}
		if id == tw.UserID() {
			roles = append(roles, "broadcaster")
		} else if isMod {
			roles = append(roles, "moderator")
		}

		if args[0] == "check" {
			r := ranks.Resolve(id, roles)
			if policy.Can(id, r, args[2]) {
				return fmt.Sprintf("yes: %s is %s, which has %s", login, r, args[2]), nil
			}
			return fmt.Sprintf("no: %s is %s, which doesn't have %s", login, r, args[2]), nil
		}

		rank, err := perms.ParseRank(args[2])
		if err != nil {
			return "", err
		}
		switch {
		case rank == perms.Broadcaster || id == tw.UserID():
			return "", errors.New("the broadcaster rank is fixed")
		case rank > perms.Moderator && !isMod:
			return "", fmt.Errorf("%s isn't a moderator on Twitch; mod them first (Twitch is the floor)", login)
		}
		if err := setRank(ranks, st, id, login, rank, "cli"); err != nil {
			return "", err
		}
		return fmt.Sprintf("%s is now %s", login, ranks.Resolve(id, roles)), nil
	}
	return "", errors.New("usage: luna perms show | set <user> <rank> | check <user> <capability>")
}
