# The platforms iroh-go ships, and how each one is built.
#
# Sourced by build-libs.sh; kept in one place so the local build and the CI
# workflow cannot drift apart.
#
# Fields: <go platform>|<rust target>|<builder>
#
# Builders:
#   zig     cargo-zigbuild. Cross-compiles from anywhere. The `.2.17` suffix
#           on the gnu targets pins a glibc floor so the library runs on
#           distributions far older than the machine that built it.
#   xwin    cargo-xwin. Cross-compiles to the MSVC ABI from anywhere by
#           fetching Microsoft's CRT and Windows SDK.
#   native  Plain cargo, host only. Used for macOS, which cannot be
#           cross-compiled -- see build-libs.sh for why.
IROH_TARGETS="
linux_amd64|x86_64-unknown-linux-gnu.2.17|zig
linux_arm64|aarch64-unknown-linux-gnu.2.17|zig
linux_amd64_musl|x86_64-unknown-linux-musl|zig
linux_arm64_musl|aarch64-unknown-linux-musl|zig
windows_amd64|x86_64-pc-windows-msvc|xwin
windows_arm64|aarch64-pc-windows-msvc|xwin
darwin_amd64|x86_64-apple-darwin|native
darwin_arm64|aarch64-apple-darwin|native
"
