// Package irohlib carries the prebuilt iroh cdylib for darwin_amd64.
//
// It exists as its own Go module so that building for one platform only
// downloads that platform's library rather than every platform's.
package irohlib

import "embed"

// Platform names the target this module was built for.
const Platform = "darwin_amd64"

// Lib holds the cdylib as lib/iroh_go.lib, plus lib/iroh_go.lib.sha256.
//
//go:embed lib
var Lib embed.FS
