//go:build windows && amd64 && !iroh_nolibs

package lib

import plat "github.com/discobox-ai/iroh-go/libs/windows_amd64"

func init() {
	embedded = plat.Lib
	platform = plat.Platform
}
