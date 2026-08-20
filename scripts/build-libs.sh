#!/usr/bin/env bash
# Builds the iroh cdylib for every platform this machine can target and
# installs each one into its module under libs/.
#
# Usage: scripts/build-libs.sh [<platform>...]
# With no arguments it attempts every platform.
#
# The four Linux targets cross-compile from any host given cargo-zigbuild.
# macOS and Windows are built on a host of that OS.
#
# macOS cannot be cross-compiled at all: iroh links the SystemConfiguration
# and CoreFoundation frameworks, which ship only in Apple's SDK, and Apple
# licenses that SDK for use on Apple hardware. arm64 dylibs also need at
# least ad-hoc signing before macOS will load them.
#
# Windows can be, in principle, with cargo-xwin. In practice ring does not
# build that way: cargo-xwin passes clang-cl style /imsvc include flags while
# cc-rs invokes plain clang, which rejects them. Native MSVC is free on
# public repos and simply works, so that is what the workflow uses.
#
# Cross-compiling also only proves a library links, not that it works. Ship
# libraries that the smoke job in .github/workflows/build-libs.yml has
# actually run the Go suite against on that platform.
set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=targets.sh
. "$repo/scripts/targets.sh"

host_os() {
	case "$(uname -s)" in
	Linux) echo linux ;;
	Darwin) echo darwin ;;
	MINGW* | MSYS* | CYGWIN*) echo windows ;;
	*) echo unknown ;;
	esac
}

have() { command -v "$1" >/dev/null 2>&1; }

# builder_ready reports whether this machine can drive a given builder for a
# given platform, and explains what is missing when it cannot.
builder_ready() {
	case "$1" in
	zig)
		if have cargo-zigbuild && have zig; then return 0; fi
		echo "cargo-zigbuild and zig (cargo install --locked cargo-zigbuild; https://ziglang.org/download/)"
		return 1
		;;
	native)
		want="${2%%_*}"
		if [ "$(host_os)" = "$want" ]; then return 0; fi
		case "$want" in
		darwin) echo "a macOS host (Apple's SDK is not redistributable, so this cannot be cross-compiled)" ;;
		windows) echo "a Windows host (ring does not build under cargo-xwin; see the notes at the top of this script)" ;;
		*) echo "a $want host" ;;
		esac
		return 1
		;;
	esac
}

build_cmd() {
	case "$1" in
	zig) echo "cargo zigbuild" ;;
	native) echo "cargo build" ;;
	esac
}

requested=("$@")
built=()
skipped=()
failed=()

for entry in $IROH_TARGETS; do
	IFS='|' read -r platform target builder <<<"$entry"

	if [ ${#requested[@]} -gt 0 ]; then
		match=no
		for want in "${requested[@]}"; do
			[ "$want" = "$platform" ] && match=yes
		done
		[ "$match" = yes ] || continue
	fi

	if ! missing="$(builder_ready "$builder" "$platform")"; then
		echo "skip  $platform -- needs $missing"
		skipped+=("$platform")
		continue
	fi

	echo
	echo "==> $platform ($target)"
	if CARGO_CMD="$(build_cmd "$builder")" "$repo/scripts/build-lib.sh" "$target" "$platform"; then
		built+=("$platform")
	else
		echo "FAILED $platform"
		failed+=("$platform")
	fi
done

echo
if [ ${#built[@]} -gt 0 ]; then
	echo "built:   ${built[*]}"
else
	echo "built:   none"
fi
if [ ${#skipped[@]} -gt 0 ]; then
	echo "skipped: ${skipped[*]}"
fi
if [ ${#failed[@]} -gt 0 ]; then
	echo "failed:  ${failed[*]}"
	exit 1
fi
if [ ${#skipped[@]} -gt 0 ]; then
	echo
	echo "The skipped platforms come from the build-libs workflow. See docs/RELEASING.md."
fi
