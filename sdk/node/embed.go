// Package nodesdk embeds the TypeScript SDK so the daemon can mount it into Node capsules at /sdk.
package nodesdk

import (
	"embed"

	"luna/internal/sdkfs"
)

//go:embed boot.mjs register.mjs luna/*.ts
var files embed.FS

// Install writes the SDK under base/node-<hash>/ (once per SDK version) and returns that folder.
func Install(base string) (string, error) { return sdkfs.Install(files, base, "node") }
