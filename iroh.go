// Package iroh provides Go bindings for iroh, a peer-to-peer QUIC library
// where endpoints are dialled by public key rather than by address.
//
// # No CGO
//
// These bindings do not use CGO. A prebuilt Rust shared library ships in a
// per-platform module, is extracted to a cache directory on first use, and is
// called through [purego]. That means CGO_ENABLED=0 builds work, and cross
// compiling needs no C toolchain:
//
//	CGO_ENABLED=0 GOOS=windows GOARCH=arm64 go build ./...
//
// # Getting started
//
//	ep, err := iroh.Bind(ctx, iroh.Options{ALPNs: [][]byte{[]byte("my/1")}})
//	defer ep.Close(ctx)
//
//	ticket, _ := ep.Ticket()      // share this with a peer
//
//	conn, err := ep.Connect(ctx, addr, []byte("my/1"))
//	send, recv, err := conn.OpenBi(ctx)
//	io.Copy(send, os.Stdin)
//	send.Close()
//	data, err := io.ReadAll(recv)
//
// Streams implement [io.Reader] and [io.Writer], so they compose with the
// standard library. Every call that waits takes a [context.Context], and
// cancelling one really does cancel the operation inside iroh rather than
// merely abandoning it.
//
// # Escape hatches
//
// Set IROH_GO_LIBRARY to load a specific library file instead of the
// embedded one, or build with the iroh_nolibs tag to omit the embedded copy
// entirely. IROH_GO_CACHE_DIR overrides where the library is extracted.
//
// [purego]: https://github.com/ebitengine/purego
package iroh

import (
	"github.com/discobox-ai/iroh-go/internal/ffi"
	"github.com/discobox-ai/iroh-go/internal/lib"
)

// LogLevel selects the verbosity of iroh's own tracing output, which is
// written to standard error.
type LogLevel int32

const (
	LogOff LogLevel = iota
	LogError
	LogWarn
	LogInfo
	LogDebug
	LogTrace
)

// SetLogLevel turns on iroh's internal logging.
//
// Only the first call has an effect, because the underlying library installs
// a process-global subscriber.
func SetLogLevel(level LogLevel) error {
	return ffi.SetLogLevel(int32(level))
}

// LibraryPath returns the path of the loaded native library. Useful when
// diagnosing which build is actually in use.
func LibraryPath() (string, error) { return lib.Path() }

// LibraryPlatform reports the platform tag of the embedded library, or "" if
// this build has none and relies on IROH_GO_LIBRARY.
func LibraryPlatform() string { return lib.Platform() }
