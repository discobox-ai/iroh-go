//go:build darwin && arm64 && !iroh_nolibs

package lib

import plat "github.com/discobox-ai/iroh-go/libs/darwin_arm64"

func init() {
	embedded = plat.Lib
	platform = plat.Platform
}
