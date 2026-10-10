// Package perms implements the rank ladder, capabilities and override rules (Permissions tab).
// Identity is always the Twitch user ID, never a login or display name.
package perms

import (
	"fmt"
	"strings"
)

type Rank int

const (
	Viewer Rank = iota
	Moderator
	LunaMod
	HeadMod
	Broadcaster
)

var rankNames = [...]string{"viewer", "moderator", "luna-mod", "head-mod", "broadcaster"}

func (r Rank) String() string { return rankNames[r] }

func ParseRank(s string) (Rank, error) {
	for i, n := range rankNames {
		if n == s {
			return Rank(i), nil
		}
	}
	return 0, fmt.Errorf("unknown rank %q (want one of %s)", s, strings.Join(rankNames[:], ", "))
}

// minRank is the default capability table: the lowest rank holding each capability.
var minRank = map[string]Rank{
	"chat.commands.use":       Viewer,
	"mod.delete":              Moderator,
	"mod.timeout":             Moderator,
	"mod.warn":                Moderator,
	"mod.ban":                 Moderator,
	"mod.undo.own":            Moderator,
	"commands.edit":           Moderator,
	"mod.undo.others":         LunaMod,
	"filters.edit":            LunaMod,
	"giveaway.run":            LunaMod,
	"points.adjust":           LunaMod,
	"stream.edit":             LunaMod,
	"luna.chat_cli":           LunaMod,
	"mod.lock":                HeadMod,
	"perms.approve_luna_mods": HeadMod,
	"perms.grant":             Broadcaster,
	"capsule.install":         Broadcaster,
	"config.security":         Broadcaster,
}

// broadcasterOnly capabilities can never be granted lower, so a compromised mod can't escalate.
func broadcasterOnly(c string) bool {
	return minRank[c] == Broadcaster || strings.HasPrefix(c, "auth.")
}

type Policy struct {
	RankGrants       map[Rank]map[string]bool   // broadcaster's per-rank adjustments
	UserGrants       map[string]map[string]bool // per Twitch user ID
	SameRankOverride map[Rank]bool
	Lockdown         bool // ranks below head mod act as moderator, grants ignored
}

func DefaultPolicy() *Policy {
	return &Policy{SameRankOverride: map[Rank]bool{LunaMod: true, HeadMod: true}}
}

func (p *Policy) effective(r Rank) Rank {
	if p.Lockdown && r == LunaMod {
		return Moderator
	}
	return r
}

func (p *Policy) Can(userID string, r Rank, capability string) bool {
	locked := p.Lockdown && r < HeadMod
	r = p.effective(r)
	switch {
	case r == Broadcaster:
		return true
	case broadcasterOnly(capability):
		return false
	case strings.HasPrefix(capability, "mod.") && r < Moderator:
		return false // Twitch is the floor: no mod powers for someone Twitch hasn't modded
	}
	if !locked {
		if ok, set := p.UserGrants[userID][capability]; set {
			return ok
		}
		if ok, set := p.RankGrants[r][capability]; set {
			return ok
		}
	}
	min, known := minRank[capability]
	return known && r >= min
}

// Action is a recorded action as far as overriding it is concerned.
type Action struct {
	ActorID   string
	ActorRank Rank
	Locked    bool
	LockRank  Rank // only this rank or above may change a locked action
}

// CanOverride applies the four override rules plus locks. The error says why not.
func (p *Policy) CanOverride(userID string, r Rank, a Action) error {
	r = p.effective(r)
	switch {
	case r == Broadcaster:
		return nil
	case a.Locked && r < a.LockRank:
		return fmt.Errorf("locked by a %s", a.LockRank)
	case userID == a.ActorID, r > a.ActorRank:
		return nil
	case r == a.ActorRank && p.SameRankOverride[r]:
		return nil
	case r == a.ActorRank:
		return fmt.Errorf("same-rank override is off for %s", r)
	default:
		return fmt.Errorf("made by a %s", a.ActorRank)
	}
}
