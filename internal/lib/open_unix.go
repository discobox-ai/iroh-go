//go:build !windows

package lib

import (
	"fmt"

	"github.com/ebitengine/purego"
)

// Open loads the iroh library and returns its handle.
func Open() (uintptr, error) {
	path, err := Path()
	if err != nil {
		return 0, err
	}
	// RTLD_NOW so missing symbols surface here rather than at first call;
	// RTLD_GLOBAL because iroh's TLS stack resolves symbols across objects.
	handle, err := purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return 0, fmt.Errorf("iroh: loading %s: %w", path, err)
	}
	return handle, nil
}
