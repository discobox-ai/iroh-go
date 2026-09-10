---
name: release
description: Cut an iroh-go release — decide whether the native libraries need rebuilding, run build-libs, tag the eight platform modules and the root, and prove the result resolves the way a consumer resolves it. Use when the user wants to release, tag, ship a version, or publish iroh-go.
allowed-tools: Bash, Read, Glob, Grep, Edit, Write
metadata:
  argument-hint: "[version]"
---

# Release

iroh-go is one root module plus eight per-platform modules under `libs/`, each
carrying a prebuilt native library. A release is a set of git tags, and the
libraries are content in git: whatever is committed when
`libs/darwin_arm64/v0.3.0` is tagged is exactly what a user downloads.

`docs/RELEASING.md` is the source of truth for *why* each step is what it is.
This skill is the order to do them in and the checks that catch the ways it
goes wrong. Read the doc when a step surprises you; do not restate it here.

**The trap that governs everything**: the root `go.mod` keeps `replace`
directives pointing at the local `libs/` directories. Go applies them only when
this repo is the main module, so they are inert for consumers — and they mask a
missing or mistyped libs tag completely. The repo builds, the suite passes, and
a consumer's `go get` fails. *Nothing you can run inside this repository proves
a release works.* Step 5's verification from outside is the only proof, and it
is a gate, not a formality.

## Running this

Invoking this skill authorizes the happy path end to end: reading tags,
dispatching and watching `build-libs`, pulling what it commits, creating and
pushing tags, editing the root's requirements, and committing that edit.
Report progress in short updates; do not stop for routine confirmation.

Stop and ask when:

- the working tree is dirty in a way this skill did not create;
- CI is not green on the commit being released;
- `build-libs` fails, or commits nothing when `rust/` did move;
- the consumer verification in step 5 fails — never tag around it;
- a tag being created already exists;
- the remedy would move or delete a published tag;
- the version is genuinely unclear after the rules below.

Pushing a tag is the point of no return: a tag is public the moment it lands
and the module version built from it cannot be un-published. If the user did
not name a version, say which one you inferred and why before pushing anything.

## 0. Preconditions

```bash
cd "$(git rev-parse --show-toplevel)"
git fetch --all --tags
git status --short
git rev-parse HEAD origin/main
```

Require: on `main`, clean, and level with `origin/main`. `.claude/` is this
skill's own home and is tracked, so it should not appear as untracked; if it
does, you are on a commit from before the skill landed.

If `libs/` is dirty, it is a local `make lib` and never part of a release —
`git checkout origin/main -- libs/` puts it back. Only the `build-libs`
workflow commits those files; `ci.yml`'s `no-hand-built-libs` job fails any
pull request that touches them.

CI must be green on the exact commit being released:

```bash
gh run list --branch main --limit 5
```

A red `committed-libs` job specifically means the Go bindings and the committed
libraries disagree on the ABI — which is the normal state after a change under
`rust/` and before `build-libs` has run. Step 3 is what clears it; note it and
carry on rather than treating it as a blocker.

That tolerance is for `committed-libs` and nothing else, so check what actually
failed rather than the run's overall conclusion:

```bash
gh run view <run-id> --json jobs --jq '.jobs[] | select(.conclusion=="failure") | .name'
```

`committed-libs` alone is expected here. Any other name is a red CI run and a
reason to stop.

## 1. Which version

```bash
git tag | sort -V | tail
```

Everything is `v0.x` today and **the root and the libs modules move together**.
Pick the next version from what changed since the last root tag:

- anything under `rust/`, or a new exported Go API → bump the **minor**
- documentation, tests, or internal Go changes only → bump the **patch**

An ABI bump (`ABI_VERSION` in `rust/irohgo-ffi/src/lib.rs`, `ABIVersion` in
`internal/ffi/ffi.go`) is always a minor at least. The two must agree; if they
do not, that is a bug to fix before releasing, not a version question.

`docs/RELEASING.md` §2 records the scheme that starts at `v1`, where the libs
adopt iroh's own major and minor. Do not apply it while still on `v0`.

## 2. Do the libraries need rebuilding?

This decides whether steps 3 and 4 happen at all.

```bash
last=$(git tag --list 'libs/linux_amd64/v*' | sort -V | tail -1)
git diff --stat "$last..HEAD" -- rust/
```

**Empty** — the libraries are byte-for-byte still correct. Skip steps 3 and 4
entirely: the existing libs tags stand and the root keeps requiring the
versions it already requires. Go straight to step 5 and tag the root alone. A
repo whose root is at `v0.4.0` while every libs module is still at `v0.1.0` is
the normal steady state, not a mistake.

**Non-empty** — the full path. Continue.

## 3. Build the native libraries

Only when step 2 said `rust/` moved.

```bash
gh workflow run build-libs.yml --ref main
```

Take the run id from what that prints. Do **not** reach for
`gh run list --limit 1`: a dispatched run is not registered instantly, so the
newest run right after a dispatch is often the *previous* build-libs run, and
watching a run that finished weeks ago reports a success that has nothing to do
with you. If you must look it up, filter by time and confirm the run started
after your dispatch:

```bash
gh run list --workflow build-libs.yml --limit 5 --json databaseId,createdAt,status
gh run watch <the id you confirmed>
```

`gh run watch` is one long-lived process against a token that may be shorter
lived than the run. If it dies with `HTTP 401: Bad credentials`, the run is
still going — get another credential and watch again.

It builds the four Linux targets on a Linux runner and macOS and Windows on
hosts of their own OS, runs the Go suite on real hardware for all eight, and
only then commits the libraries. Expect it to take a while; that is three
operating systems and eight smoke jobs.

Then take what it wrote:

```bash
git pull --ff-only
git log --oneline -1
```

`git log --oneline -1` is the test: a run that rebuilt the libraries leaves
`libs: rebuild native libraries from <sha>` at the tip, naming the commit it
built from. If instead the tip is unchanged — the run reported "libraries
unchanged" and committed nothing — while step 2 said `rust/` moved, **stop**:
the build is not reproducing what you think it is.

Step 4 tags *this* commit. Step 5 adds one more commit on top and the root tag
lands on that, so the libs tags and the root tag deliberately name different
commits — the libraries are what was built and smoke-tested, the root is what
requires them.

## 4. Tag the eight platform modules

Only when step 3 ran. The platform list comes from `scripts/targets.sh`, which
exists so the build and the workflow cannot drift; do not hardcode it here or
copy the list out of `RELEASING.md`.

Run this as one subshell, so a guard that trips stops the block instead of
killing the shell you are sitting in:

```bash
V=v0.3.0   # the version from step 1
(
  set -e
  . scripts/targets.sh
  platforms=$(printf '%s\n' "$IROH_TARGETS" | awk -F'|' 'NF {print $1}')
  [ "$(printf '%s\n' "$platforms" | wc -l)" -eq 8 ] || { echo "expected 8 platforms"; exit 1; }
  for p in $platforms; do
    if git rev-parse -q --verify "refs/tags/libs/$p/$V" >/dev/null; then
      echo "libs/$p/$V already exists"; exit 1
    fi
  done
  for p in $platforms; do git tag "libs/$p/$V"; done
  git push origin $(for p in $platforms; do echo "libs/$p/$V"; done)
)
```

Push all eight or none. A partial set is the failure this whole skill is shaped
around: it is invisible from inside the repo.

## 5. Point the root at them, prove it, then tag it

If step 4 ran, move the root's *required* versions. The `replace` block stays
exactly where it is — it is inert for consumers and permanent in `main`.

```bash
V=v0.3.0
. scripts/targets.sh
for p in $(printf '%s\n' "$IROH_TARGETS" | awk -F'|' 'NF {print $1}'); do
  go mod edit -require "github.com/discobox-ai/iroh-go/libs/$p@$V"
done
git status --short          # go.mod and nothing else, before -a sweeps it up
git diff --stat go.mod
git commit -am "Release $V"
git push origin main
```

Give the commit a body saying what is in the release, the way `Release v0.2.0`
does: it is the closest thing this repository has to a changelog.

**Now prove it the way a consumer sees it**, from a scratch module outside this
repository, where the `replace` directives do not apply. This is the gate, and
it runs **before** the root tag exists — so it asks for the commit you just
pushed, not for the version:

```bash
sha=$(git rev-parse HEAD)
tmp=$(mktemp -d) && cd "$tmp" && go mod init check
GOFLAGS=-mod=mod go get "github.com/discobox-ai/iroh-go@$sha" || { echo "GATE FAILED"; }
cat > main.go <<'GO'
package main

import (
	"fmt"

	iroh "github.com/discobox-ai/iroh-go"
)

func main() {
	version, err := iroh.IrohVersion()
	if err != nil {
		panic(err)
	}
	fmt.Println("iroh", version)
}
GO
CGO_ENABLED=0 go run . || echo "GATE FAILED"
```

Asking for the commit resolves it as a pseudo-version, and Go then reads that
commit's own `go.mod` — the one you just edited — so all eight `libs/*@$V`
have to resolve for this to succeed. It loads a real native library through
purego, which proves three things at once: every libs tag resolves, the library
for this platform downloads, and the Go bindings and that library agree on the
ABI. A failure here is a release that would break on `go get`. Do not tag the
root; fix what is missing.

**Do not ask for `@$V` before the tag exists.** It is the obvious thing to type
and it damages the release: the failed lookup is cached as a negative result by
`sum.golang.org`, and the same command then keeps failing for minutes *after*
you push the tag, with a 404 from the checksum database — a hard failure on a
release that is in fact correct, at the worst possible moment.

Only once the gate has passed:

```bash
git tag "$V"
git push origin "$V"
```

A tag takes a moment to become resolvable. Re-running the check against `@$V`
straight afterwards can 404 from the proxy or the checksum database; that is
propagation, not failure. Wait a minute and try again before believing it.

## 6. Nothing to restore

The `replace` block stays in `main`. There is no add-and-remove dance around a
release — only the required versions move.

Report at the end: the version, whether the libraries were rebuilt or reused,
the tags pushed, and the iroh version the verification printed.
