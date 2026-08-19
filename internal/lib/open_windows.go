//go:build windows

package lib

import (
	"fmt"
	"syscall"
)

// Open loads the iroh library and returns its handle.
func Open() (uintptr, error) {
	path, err := Path()
	if err != nil {
		return 0, err
	}
	handle, err := syscall.LoadLibrary(path)
	if err != nil {
		return 0, fmt.Errorf("iroh: loading %s: %w", path, err)
	}
	return uintptr(handle), nil
}
