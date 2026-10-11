// Package pysdk embeds the Python SDK so the daemon can mount it into Python capsules at /sdk.
package pysdk

import (
	"embed"

	"luna/internal/sdkfs"
)

//go:embed all:luna
var files embed.FS

// Install writes the SDK under base/python-<hash>/ (once per SDK version) and returns that folder.
func Install(base string) (string, error) { return sdkfs.Install(files, base, "python") }
