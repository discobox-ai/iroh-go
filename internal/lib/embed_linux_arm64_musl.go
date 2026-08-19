//go:build linux && arm64 && musl && !iroh_nolibs

package lib

import plat "github.com/discobox-ai/iroh-go/libs/linux_arm64_musl"

func init() {
	embedded = plat.Lib
	platform = plat.Platform
}
