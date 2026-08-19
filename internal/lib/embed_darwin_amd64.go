//go:build darwin && amd64 && !iroh_nolibs

package lib

import plat "github.com/discobox-ai/iroh-go/libs/darwin_amd64"

func init() {
	embedded = plat.Lib
	platform = plat.Platform
}
