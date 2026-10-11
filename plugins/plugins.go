// Package plugins lists the first-party plugins built into the luna binary. Each runs on
// the luna runtime as `luna <entry>`, sandboxed like any other capsule.
package plugins

import (
	"strings"

	"luna/bundle"
	"luna/plugins/avatar"
	"luna/plugins/giveaways"
	"luna/plugins/hrc"
	"luna/plugins/points"
	"luna/plugins/stream"
)

type Plugin struct {
	Names    []string // install names: the first is canonical (luna capsule install <name>)
	Entry    string
	Manifest []byte
	Main     func()
}

var All = []Plugin{
	{[]string{"core", "essentials", "mod", "events", "shoutouts"}, "__bundle", bundle.Manifest, bundle.Main},
	{[]string{"points"}, "__points", points.Manifest, points.Main},
	{[]string{"giveaways"}, "__giveaways", giveaways.Manifest, giveaways.Main},
	{[]string{"stream"}, "__stream", stream.Manifest, stream.Main},
	{[]string{"avatar"}, "__avatar", avatar.Manifest, avatar.Main},
	{[]string{"hrc", "heartrate"}, "__hrc", hrc.Manifest, hrc.Main},
}

// Find looks a plugin up by install name, with or without the "luna." prefix.
func Find(name string) (Plugin, bool) {
	name = strings.TrimPrefix(name, "luna.")
	for _, p := range All {
		for _, n := range p.Names {
			if n == name {
				return p, true
			}
		}
	}
	return Plugin{}, false
}

// ByEntry finds the plugin a capsule's argv[1] names.
func ByEntry(entry string) (Plugin, bool) {
	for _, p := range All {
		if p.Entry == entry {
			return p, true
		}
	}
	return Plugin{}, false
}
