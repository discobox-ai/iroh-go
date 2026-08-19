package ffi

import (
	"errors"
	"unsafe"
)

// NewError builds the error type the public package exposes. The public
// package installs it during its own initialisation, which keeps the error
// type out of this internal package's API without forcing a conversion at
// every call site.
//
// It is only ever read while servicing a call, which cannot happen before
// package initialisation has finished.
var NewError = func(kind int32, msg string) error {
	return errors.New(msg)
}

// takeError consumes an error handle produced by a failed call.
func takeError(handle uint64) error {
	if handle == 0 {
		return NewError(KindInternal, "iroh: call failed without reporting a reason")
	}
	var (
		kind   int32
		msgPtr unsafe.Pointer
		msgLen uintptr
	)
	c.errorTake(handle, unsafe.Pointer(&kind), unsafe.Pointer(&msgPtr), unsafe.Pointer(&msgLen))
	return NewError(kind, string(takeBytes(msgPtr, msgLen)))
}

// bytePtr points at the first byte of b, or nil when b is empty. The Rust
// side treats a null pointer with zero length as an empty slice.
func bytePtr(b []byte) unsafe.Pointer {
	if len(b) == 0 {
		return nil
	}
	return unsafe.Pointer(&b[0])
}

// takeBytes copies a buffer the library allocated into Go memory and frees
// the original. The result is always Go-owned, so callers may retain it.
func takeBytes(ptr unsafe.Pointer, n uintptr) []byte {
	if ptr == nil || n == 0 {
		return nil
	}
	out := make([]byte, n)
	copy(out, unsafe.Slice((*byte)(ptr), n))
	c.freeBytes(ptr, n)
	return out
}

// takeString is takeBytes for text results.
func takeString(ptr unsafe.Pointer, n uintptr) string {
	return string(takeBytes(ptr, n))
}
