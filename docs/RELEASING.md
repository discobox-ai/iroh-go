# Releasing

This repository is a multi-module Go repository: the root module
`github.com/discobox-ai/iroh-go` plus one module per platform under `libs/`.
Splitting them is what keeps `go build` from downloading every platform's
native library.

For day-to-day development the root `go.mod` points at the local directories
with `replace` directives. A release swaps those for published versions.

## 0. Do the libraries need rebuilding at all?

Usually not. A libs module is a `go.mod`, a `lib.go` that never changes, and
the compiled library -- so its content moves only when something under `rust/`
does. A release that touches only Go code reuses the libs tags already
published, and steps 1 and 2 are skipped entirely: the root `go.mod` keeps
requiring the versions it already requires.

```
git diff --stat <last libs tag>..HEAD -- rust/
```

Nothing there? Go straight to step 3 and tag the root module alone. A
repository where the root is at `v0.4.0` and every libs module is still at
`v0.1.0` is the normal steady state, not a mistake.

The release profile builds reproducibly, so re-running the workflow when
nothing changed is harmless -- the binaries come out byte-identical and the
commit job reports "libraries unchanged" and pushes nothing. It just wastes
runner time.

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
platforms. That is the cost of embedding them in per-platform modules. In
return `go build` fetches only the platform it is building, about 6MB
compressed; `go get` and `go mod tidy` fetch all eight once, because Go records
checksums across every platform.

Only the workflow commits them, with `git add -f`, and only after every one
has passed its smoke job.

They are listed in `.gitignore`, but do not rely on that: ignore rules do not
apply to tracked files, so once the workflow has committed them a `git add -A`
after a local `make lib` will happily commit yours too. That is not
hypothetical -- it is how a locally-built library for one platform, never
smoke-tested anywhere, once reached `main` alongside seven stale ones, and
passed CI because the one job checking committed libraries ran on precisely
that platform.

The `no-hand-built-libs` job in `ci.yml` is the real guard: it fails any pull
request that touches `libs/*/lib/`. If you have picked one up by accident,
`git checkout origin/main -- libs/` puts it back.

## 2. Tag the platform modules

*Only when step 0 said the Rust changed.*

**Today everything is `v0.x`, and the root and libs modules move together.**
Both are pre-1.0: the Go API is still growing (no `Incoming`/0-RTT, watchers or
`ServicesClient` yet), and a `v1` on the libs would commit their import paths
for the whole iroh 1.x line before that is worth promising.

The exact upstream version is not in the tag and does not need to be. It is
baked into each library from `rust/Cargo.lock` at build time, reported by
`iroh.IrohVersion()`, and pinned by `TestIrohVersionMatchesTheLockfile` so it
cannot drift from what is actually compiled in.

**When these bindings reach `v1`**, the libs modules adopt iroh's major and
minor, with the patch reserved for changes to the shim in `rust/irohgo-ffi`:

| iroh at the time | libs tag |
| ---------------- | -------- |
| 1.0.x            | `v1.0.0`, then `v1.0.1` for shim fixes |
| 1.2.x            | `v1.2.0` |
| 2.0.x            | `v2.0.0`, and the module paths gain a `/v2` suffix |

The patch stays ours because a libs module is iroh *plus* the shim, and the
shim moves on its own schedule -- it needed a correctness fix while iroh stood
still. Tying it to upstream would leave two different libraries both claiming
one version, one of them with a bug. Note that the first `v1` libs tag has to
match iroh's minor *at that moment*, so it will not necessarily be `v1.0.0`.

Tag each libs module with its directory as the prefix:

```
V=v0.1.0
for p in linux_amd64 linux_amd64_musl linux_arm64 linux_arm64_musl \
         darwin_amd64 darwin_arm64 windows_amd64 windows_arm64; do
  git tag "libs/$p/$V"
done
git push --tags
```

## 3. Tag the root module

If the libs were re-tagged in step 2, point the root at the new versions
first; otherwise leave the requirements alone -- they already name the libs
versions still in use.

The root `go.mod` keeps its `replace` directives -- Go applies them only when
this repo is the main module, so contributors build against the libraries they
just built with `make lib`, while consumers resolve the published versions and
ignore the replaces entirely.

What must change each release is the *required* versions:

```
V=v0.1.0
for p in linux_amd64 linux_amd64_musl linux_arm64 linux_arm64_musl \
         darwin_amd64 darwin_arm64 windows_amd64 windows_arm64; do
  go mod edit -require "github.com/discobox-ai/iroh-go/libs/$p@$V"
done
```

Because the replaces mask a missing tag, a build here proves nothing about a
consumer's. Verify the way a consumer resolves it, in a scratch module outside
this repo:

```
cd "$(mktemp -d)" && go mod init check
GOFLAGS=-mod=mod go get github.com/discobox-ai/iroh-go@v0.1.0
cat > main.go <<'GO'
package main

import ("fmt"; "github.com/discobox-ai/iroh-go")

func main() { v, err := iroh.IrohVersion(); fmt.Println(v, err) }
GO
CGO_ENABLED=0 go run .
```

Then tag the root module:

```
git tag v0.1.0
git push --tags
```

## 4. Nothing to restore

The `replace` block stays in `main` permanently. It is inert for consumers, so
there is no add-and-remove dance around each release -- only the required
versions move.

## Bumping iroh

Change the version in `rust/irohgo-ffi/Cargo.toml`, run `cargo update -p iroh`,
and check whether anything in `rust/irohgo-ffi/src/` needs adjusting. If iroh's
major or minor moved, the next libs tag follows it -- see the table above. If the C
ABI changes shape at all — a signature, a constant, an error kind's number —
bump `ABI_VERSION` in `rust/irohgo-ffi/src/lib.rs` *and* `ABIVersion` in
`internal/ffi/ffi.go`. The loader compares them, so a mismatched pair reports a
clear error instead of crashing.
