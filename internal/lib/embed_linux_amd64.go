//go:build linux && amd64 && !musl && !iroh_nolibs

package lib

import plat "github.com/discobox-ai/iroh-go/libs/linux_amd64"

func init() {
	embedded = plat.Lib
	platform = plat.Platform
}
