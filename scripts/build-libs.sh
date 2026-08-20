#!/usr/bin/env bash
# Builds the iroh cdylib for every platform this machine can target and
# installs each one into its module under libs/.
#
# Usage: scripts/build-libs.sh [<platform>...]
# With no arguments it attempts every platform.
#
# Six of the eight platforms cross-compile from any host given cargo-zigbuild
# and cargo-xwin. The two macOS targets do not, and cannot be fixed by
# installing something: iroh links the SystemConfiguration and CoreFoundation
# frameworks, which ship only in Apple's SDK, and Apple licenses that SDK for
# use on Apple hardware. On top of that, arm64 dylibs must be at least ad-hoc
# signed before macOS will load them. So darwin is built on a Mac -- by a
# developer running this script there, or by the build-libs workflow.
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

# builder_ready reports whether this machine can drive a given builder, and
# explains what is missing when it cannot.
builder_ready() {
	case "$1" in
	zig)
		if have cargo-zigbuild && have zig; then return 0; fi
		echo "cargo-zigbuild and zig (cargo install --locked cargo-zigbuild; https://ziglang.org/download/)"
		return 1
		;;
	xwin)
		# cargo-xwin supplies Microsoft's CRT and SDK but not the compiler:
		# cc-rs invokes llvm-lib and clang-cl from LLVM, and ring assembles
		# its x86_64 Windows assembly with nasm. Check for all three, or the
		# failure surfaces deep inside ring's build script instead.
		missing=""
		have cargo-xwin || missing="$missing cargo-xwin (cargo install --locked cargo-xwin)"
		have nasm || missing="$missing nasm"
		if ! have llvm-lib && ! { have clang && [ -x "$(dirname "$(readlink -f "$(command -v clang)")")/llvm-lib" ]; }; then
			missing="$missing llvm/clang (llvm-lib not found; on Ubuntu it lives in /usr/lib/llvm-*/bin)"
		fi
		[ -z "$missing" ] && return 0
		echo "${missing# }"
		return 1
		;;
	native)
		if [ "$(host_os)" = darwin ]; then return 0; fi
		echo "a macOS host (Apple's SDK is not redistributable, so this cannot be cross-compiled)"
		return 1
		;;
	esac
}

build_cmd() {
	case "$1" in
	zig) echo "cargo zigbuild" ;;
	xwin) echo "cargo xwin build" ;;
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

	if ! missing="$(builder_ready "$builder")"; then
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
