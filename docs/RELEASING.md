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

The workflow builds the four Linux platforms on a Linux runner, and macOS and
Windows on hosts of their own OS. It then runs the Go suite on real hardware
for all eight, and only then commits them to the branch you dispatched from.

Locally, `make libs` does as much as your machine can:

```
make libs                              # everything this host can target
make libs PLATFORMS="linux_arm64"      # or specific ones
```

It needs [cargo-zigbuild](https://github.com/rust-cross/cargo-zigbuild) and
[zig](https://ziglang.org/download/):

```
cargo install --locked cargo-zigbuild
```

That covers the four Linux targets — gnu and musl on both architectures —
from any host.

**macOS and Windows are built on a host of that OS.**

macOS cannot be cross-compiled, and no amount of tooling fixes it: iroh links
the `SystemConfiguration` and `CoreFoundation` frameworks, which ship only in
Apple's SDK, and Apple licenses that SDK for use on Apple hardware. Ad-hoc code
signing is required for arm64 dylibs to load at all, too.

Windows can be cross-compiled in principle, with
[cargo-xwin](https://github.com/rust-cross/cargo-xwin), but `ring` does not
build that way: cargo-xwin passes clang-cl style `/imsvc` include flags while
`cc-rs` invokes plain `clang`, which rejects them. Native MSVC is free on
public repos and simply works, so the workflow uses a Windows runner. If you
want to revisit the cross path, that flag mismatch is the thing to solve.

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

The libs modules carry compiled Rust, so their versions track **iroh's major
and minor**, with the patch reserved for changes to the shim in
`rust/irohgo-ffi`:

| iroh    | libs tag             |
| ------- | -------------------- |
| 1.0.3   | `v1.0.0`             |
| 1.0.3   | `v1.0.1` (shim fix)  |
| 1.0.4   | `v1.0.2` (shim same, upstream patch) |
| 1.1.0   | `v1.1.0`             |
| 2.0.0   | `v2.0.0`, and the module paths gain a `/v2` suffix |

The patch digit is ours because a libs module is iroh *plus* our shim, and the
shim changes on its own schedule -- it has already needed a correctness fix
while iroh stood still. Tying the patch to upstream would leave two different
libraries both claiming to be "1.0.3", one of them with a bug.

iroh's own patch digit is therefore not in the tag. It is not lost: the exact
locked version is baked into the library at build time and reported by
`iroh.IrohVersion()`, and `TestIrohVersionMatchesTheLockfile` asserts it
against `rust/Cargo.lock` so it cannot drift.

**The root module versions separately**, on its own Go API, and stays at `v0.x`
until that API settles. A repository where `libs/linux_amd64` is at `v1.0.0`
and the root is at `v0.2.0` is correct: they promise different things.

Tag each libs module with its directory as the prefix:

```
V=v1.0.0
for p in linux_amd64 linux_amd64_musl linux_arm64 linux_arm64_musl \
         darwin_amd64 darwin_arm64 windows_amd64 windows_arm64; do
  git tag "libs/$p/$V"
done
git push --tags
```

## 3. Point the root module at the published versions

Drop the `replace` block and set the real versions:

```
V=v1.0.0
for p in linux_amd64 linux_amd64_musl linux_arm64 linux_arm64_musl \
         darwin_amd64 darwin_arm64 windows_amd64 windows_arm64; do
  go mod edit -dropreplace "github.com/discobox-ai/iroh-go/libs/$p"
  go mod edit -require "github.com/discobox-ai/iroh-go/libs/$p@$V"
done
go mod tidy
```

Verify against the published modules:

```
CGO_ENABLED=0 go build ./...
CGO_ENABLED=0 go test ./...
make cross
```

Commit, then tag the root module on its own line -- `v0.2.0`, not the libs
version:

```
git tag v0.2.0
git push --tags
```

## 4. Restore the replaces for development

The `replace` block goes back on `main` after the release tag, so contributors
keep building against their locally built libraries.

## Bumping iroh

Change the version in `rust/irohgo-ffi/Cargo.toml`, run `cargo update -p iroh`,
and check whether anything in `rust/irohgo-ffi/src/` needs adjusting. If iroh's
major or minor moved, the next libs tag follows it -- see the table above. If the C
ABI changes shape at all — a signature, a constant, an error kind's number —
bump `ABI_VERSION` in `rust/irohgo-ffi/src/lib.rs` *and* `ABIVersion` in
`internal/ffi/ffi.go`. The loader compares them, so a mismatched pair reports a
clear error instead of crashing.
