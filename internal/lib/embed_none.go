//go:build iroh_nolibs || !((linux && (amd64 || arm64)) || (darwin && (amd64 || arm64)) || (windows && (amd64 || arm64)))

package lib

// No library is embedded for this build. Path falls back to IROH_GO_LIBRARY,
// and otherwise reports ErrNoEmbeddedLibrary.
