// Package lib locates the iroh cdylib and opens it.
//
// The library is normally embedded in a per-platform module (see
// ../../libs) and extracted once into a hash-named cache directory, the same
// approach turso-go uses. Two escape hatches exist: the IROH_GO_LIBRARY
// environment variable points at a library directly, and the iroh_nolibs
// build tag drops the embedded copy so callers can supply their own.
package lib

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// EnvLibrary names a library file to load instead of the embedded one.
const EnvLibrary = "IROH_GO_LIBRARY"

// EnvCacheDir overrides where the embedded library is extracted.
const EnvCacheDir = "IROH_GO_CACHE_DIR"

// EnvVerify forces a full hash check of an already-extracted library. Off by
// default because it costs a full read of a ~15MB file on every start; the
// cache path already contains the hash, so only truncation is a real risk and
// the size check covers that.
const EnvVerify = "IROH_GO_VERIFY_LIB"

// Set by the embed_*.go file matching the build platform.
var (
	embedded fs.FS
	platform string
)

// embeddedName is the path inside the per-platform module. It is deliberately
// extension-free: the file is renamed on extraction to whatever the local
// dynamic loader expects.
const embeddedName = "lib/iroh_go.lib"

// ErrNoEmbeddedLibrary means this build has no library to extract, either
// because the platform is unsupported or because iroh_nolibs was set.
var ErrNoEmbeddedLibrary = errors.New("iroh: no embedded library for " + runtime.GOOS + "/" + runtime.GOARCH)

// Platform reports the platform tag of the embedded library, or "" if there
// is none.
func Platform() string { return platform }

// Path returns a filesystem path to the iroh library, extracting the embedded
// copy if needed.
func Path() (string, error) {
	if p := os.Getenv(EnvLibrary); p != "" {
		if _, err := os.Stat(p); err != nil {
			return "", fmt.Errorf("iroh: %s=%s: %w", EnvLibrary, p, err)
		}
		return p, nil
	}
	return extract()
}

// libraryFilename is the name the extracted library is given on disk. Only
// Windows genuinely cares, but matching local convention keeps stack traces
// and ldd output readable.
func libraryFilename() (string, error) {
	switch runtime.GOOS {
	case "darwin", "ios":
		return "libiroh_go.dylib", nil
	case "windows":
		return "iroh_go.dll", nil
	case "linux", "android", "freebsd", "netbsd":
		return "libiroh_go.so", nil
	default:
		return "", fmt.Errorf("iroh: unsupported operating system %q", runtime.GOOS)
	}
}

func extract() (string, error) {
	if embedded == nil {
		return "", ErrNoEmbeddedLibrary
	}

	data, err := fs.ReadFile(embedded, embeddedName)
	if err != nil {
		return "", fmt.Errorf("%w: run `make lib` to build one: %v", ErrNoEmbeddedLibrary, err)
	}

	want, err := embeddedHash(data)
	if err != nil {
		return "", err
	}

	filename, err := libraryFilename()
	if err != nil {
		return "", err
	}

	// The hash is in the path, so a new library version never collides with
	// a stale extraction and concurrent processes on the same version agree
	// on the same file.
	dir := filepath.Join(cacheRoot(), "iroh-go", platform+"-"+want[:16])
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("iroh: creating library cache %s: %w", dir, err)
	}
	path := filepath.Join(dir, filename)

	if ok, err := usable(path, len(data), want); err != nil {
		return "", err
	} else if ok {
		return path, nil
	}

	if err := writeAtomic(path, data); err != nil {
		return "", err
	}
	return path, nil
}

// embeddedHash prefers the sidecar written at build time and falls back to
// hashing the bytes, so a locally built library without a sidecar still works.
func embeddedHash(data []byte) (string, error) {
	if raw, err := fs.ReadFile(embedded, embeddedName+".sha256"); err == nil {
		got := strings.TrimSpace(string(raw))
		// The sidecar may be in `sha256sum` format: "<hash>  <name>".
		if i := strings.IndexAny(got, " \t"); i > 0 {
			got = got[:i]
		}
		if len(got) != 64 {
			return "", fmt.Errorf("iroh: embedded sha256 sidecar is malformed (%d chars)", len(got))
		}
		sum := sha256.Sum256(data)
		if want := hex.EncodeToString(sum[:]); want != got {
			return "", fmt.Errorf("iroh: embedded library does not match its sha256 sidecar (have %s, want %s)", want, got)
		}
		return got, nil
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// usable reports whether the cached file can be reused as-is.
func usable(path string, size int, want string) (bool, error) {
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("iroh: inspecting cached library %s: %w", path, err)
	}
	if info.Size() != int64(size) {
		// A truncated or half-written extraction from a killed process.
		return false, nil
	}
	if os.Getenv(EnvVerify) == "" {
		return true, nil
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("iroh: verifying cached library %s: %w", path, err)
	}
	sum := sha256.Sum256(onDisk)
	if got := hex.EncodeToString(sum[:]); got != want {
		return false, fmt.Errorf("iroh: cached library %s is corrupt (have %s, want %s)", path, got, want)
	}
	return true, nil
}

// writeAtomic writes via a unique temporary file and renames, so that two
// processes extracting concurrently cannot observe a partial library.
func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".iroh_go-*")
	if err != nil {
		return fmt.Errorf("iroh: creating temporary library file: %w", err)
	}
	name := tmp.Name()
	defer os.Remove(name)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("iroh: writing library to %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("iroh: closing %s: %w", name, err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(name, 0o755); err != nil {
			return fmt.Errorf("iroh: making %s executable: %w", name, err)
		}
	}
	if err := os.Rename(name, path); err != nil {
		// On Windows a rename over a library another process already mapped
		// fails; if that file is the right size it is the same content.
		if info, statErr := os.Stat(path); statErr == nil && info.Size() == int64(len(data)) {
			return nil
		}
		return fmt.Errorf("iroh: installing library at %s: %w", path, err)
	}
	return nil
}

func cacheRoot() string {
	if d := os.Getenv(EnvCacheDir); d != "" {
		return d
	}
	if d, err := os.UserCacheDir(); err == nil {
		return d
	}
	return os.TempDir()
}
