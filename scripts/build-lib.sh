#!/usr/bin/env bash
# Builds the iroh cdylib and installs it into the matching per-platform Go
# module, together with the sha256 sidecar the loader checks.
#
# Usage: scripts/build-lib.sh [<rust-target> <go-platform>]
# With no arguments it builds for the host.
#
# CARGO_CMD overrides the build command, for cross compilers that replace the
# `build` subcommand: CARGO_CMD="cargo zigbuild", CARGO_CMD="cargo xwin build".
set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

host_platform() {
	local os arch
	case "$(uname -s)" in
	Linux) os=linux ;;
	Darwin) os=darwin ;;
	MINGW* | MSYS* | CYGWIN*) os=windows ;;
	*)
		echo "unsupported host $(uname -s)" >&2
		exit 1
		;;
	esac
	case "$(uname -m)" in
	x86_64 | amd64) arch=amd64 ;;
	arm64 | aarch64) arch=arm64 ;;
	*)
		echo "unsupported host $(uname -m)" >&2
		exit 1
		;;
	esac
	echo "${os}_${arch}"
}

if [ $# -eq 2 ]; then
	target="$1"
	platform="$2"
	cargo_args=(--target "$target")
	# cargo-zigbuild accepts a glibc floor as a target suffix
	# (x86_64-unknown-linux-gnu.2.17) but still writes its output under the
	# plain triple.
	out_dir="$repo/rust/target/${target%%.*}/release"
else
	target=""
	platform="$(host_platform)"
	cargo_args=()
	out_dir="$repo/rust/target/release"
fi

# The rust std library for a cross target has to be present. Harmless to
# repeat, and it saves both developers and CI from tracking the triples
# separately from scripts/targets.sh.
if [ -n "$target" ] && command -v rustup >/dev/null 2>&1; then
	rustup target add "${target%%.*}" >/dev/null
fi

# A cdylib for a musl target is only shared if crt-static is switched off.
case "$target" in
*-musl) export RUSTFLAGS="${RUSTFLAGS:-} -C target-feature=-crt-static" ;;
esac

echo "building iroh cdylib for $platform${target:+ ($target)}"
(cd "$repo/rust" && ${CARGO_CMD:-cargo build} --release "${cargo_args[@]}")

case "$platform" in
darwin_*) built="$out_dir/libiroh_go.dylib" ;;
windows_*) built="$out_dir/iroh_go.dll" ;;
*) built="$out_dir/libiroh_go.so" ;;
esac

if [ ! -f "$built" ]; then
	echo "expected $built to exist after the build" >&2
	exit 1
fi

dest="$repo/libs/$platform/lib"
mkdir -p "$dest"
cp "$built" "$dest/iroh_go.lib"

# sha256sum on Linux, shasum on macOS.
if command -v sha256sum >/dev/null 2>&1; then
	(cd "$dest" && sha256sum iroh_go.lib >iroh_go.lib.sha256)
else
	(cd "$dest" && shasum -a 256 iroh_go.lib >iroh_go.lib.sha256)
fi

ls -la "$dest/iroh_go.lib"
cat "$dest/iroh_go.lib.sha256"
