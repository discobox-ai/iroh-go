# Releasing

This repository is a multi-module Go repository: the root module
`github.com/discobox-ai/iroh-go` plus one module per platform under `libs/`.
Splitting them is what keeps `go build` from downloading every platform's
native library.

For day-to-day development the root `go.mod` points at the local directories
with `replace` directives. A release swaps those for published versions.

## 1. Build and commit the native libraries

Run the **build-libs** workflow (Actions → build-libs → Run workflow). It is
manual on purpose: each run rewrites eight ~14 MB binaries, and binaries do not
delta-compress, so wiring it to every push would add ~115 MB of permanent git
history per change. Correctness on every push is already covered by `ci.yml`,
which builds a fresh library from your Rust changes and runs the whole Go suite
against it.

The workflow builds the six cross-compilable platforms on Linux, the two macOS
ones on a Mac, runs the Go suite on real hardware for all eight, and only then
commits them to the branch you dispatched from.

Locally, `make libs` does as much as your machine can:

```
make libs                              # everything this host can target
make libs PLATFORMS="linux_arm64"      # or specific ones
```

It needs [cargo-zigbuild](https://github.com/rust-cross/cargo-zigbuild) (plus
[zig](https://ziglang.org/download/)) and
[cargo-xwin](https://github.com/rust-cross/cargo-xwin):

```
cargo install --locked cargo-zigbuild cargo-xwin
```

The Windows targets need a little more: cargo-xwin supplies Microsoft's CRT
and SDK but not the compiler, so LLVM and nasm have to be present too (cc-rs
calls `llvm-lib` and `clang-cl`, and ring assembles its x86_64 Windows
assembly with nasm). On Ubuntu:

```
sudo apt-get install -y clang llvm lld nasm
export PATH="$(dirname "$(readlink -f "$(command -v clang)")"):$PATH"
```

`build-libs.sh` checks for all of this up front and names whatever is missing.

That covers Linux gnu and musl on both architectures, and Windows on both —
cargo-xwin targets the MSVC ABI without a Windows machine.

**macOS cannot be cross-compiled**, and no amount of tooling fixes it. iroh
links the `SystemConfiguration` and `CoreFoundation` frameworks, which ship
only in Apple's SDK, and Apple licenses that SDK for use on Apple hardware.
Ad-hoc code signing is required for arm64 dylibs to load at all, too. Build
those two on a Mac, or let the workflow do it.

Whatever you build locally, remember that cross-compiling proves a library
*links*, not that it *works* — these are `dlopen`'d at runtime with no
fallback. Only ship libraries the workflow's smoke jobs have actually run the
Go suite against.

The libraries are committed to git — roughly 15 MB each, ~120 MB across
platforms. That is the cost of embedding them in per-platform modules; users
only ever download the one module their build needs.

They are also listed in `.gitignore`, so a local `make lib` build cannot be
committed by hand. Only the workflow commits them, with `git add -f`, and only
after every one has passed its smoke job. If you ever do need to commit one
yourself, `git add -f libs/<platform>/lib/` is the deliberate override.

## 2. Tag the platform modules

Each `libs/<platform>` module is tagged with its directory as the prefix:

```
for p in linux_amd64 linux_amd64_musl linux_arm64 linux_arm64_musl \
         darwin_amd64 darwin_arm64 windows_amd64 windows_arm64; do
  git tag "libs/$p/v0.1.0"
done
git push --tags
```

## 3. Point the root module at the published versions

Drop the `replace` block and set the real versions:

```
for p in linux_amd64 linux_amd64_musl linux_arm64 linux_arm64_musl \
         darwin_amd64 darwin_arm64 windows_amd64 windows_arm64; do
  go mod edit -dropreplace "github.com/discobox-ai/iroh-go/libs/$p"
  go mod edit -require "github.com/discobox-ai/iroh-go/libs/$p@v0.1.0"
done
go mod tidy
```

Verify against the published modules, with the workspace out of the way:

```
CGO_ENABLED=0 go build ./...
CGO_ENABLED=0 go test ./...
make cross
```

Commit, then tag the root module:

```
git tag v0.1.0
git push --tags
```

## 4. Restore the replaces for development

The `replace` block goes back on `main` after the release tag, so contributors
keep building against their locally built libraries.

## Bumping iroh

Change the version in `rust/irohgo-ffi/Cargo.toml`, run `cargo update -p iroh`,
and check whether anything in `rust/irohgo-ffi/src/` needs adjusting. If the C
ABI changes shape at all — a signature, a constant, an error kind's number —
bump `ABI_VERSION` in `rust/irohgo-ffi/src/lib.rs` *and* `ABIVersion` in
`internal/ffi/ffi.go`. The loader compares them, so a mismatched pair reports a
clear error instead of crashing.
