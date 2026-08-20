module github.com/discobox-ai/iroh-go

go 1.26.0

require (
	github.com/discobox-ai/iroh-go/libs/darwin_amd64 v0.1.0
	github.com/discobox-ai/iroh-go/libs/darwin_arm64 v0.1.0
	github.com/discobox-ai/iroh-go/libs/linux_amd64 v0.1.0
	github.com/discobox-ai/iroh-go/libs/linux_amd64_musl v0.1.0
	github.com/discobox-ai/iroh-go/libs/linux_arm64 v0.1.0
	github.com/discobox-ai/iroh-go/libs/linux_arm64_musl v0.1.0
	github.com/discobox-ai/iroh-go/libs/windows_amd64 v0.1.0
	github.com/discobox-ai/iroh-go/libs/windows_arm64 v0.1.0
	github.com/ebitengine/purego v0.10.2
)

// The per-platform library modules live in this repo. Go applies these only
// when this repo is the main module, so contributors build against libraries
// they built themselves while consumers resolve the required versions above
// and ignore this block. See docs/RELEASING.md.
replace (
	github.com/discobox-ai/iroh-go/libs/darwin_amd64 => ./libs/darwin_amd64
	github.com/discobox-ai/iroh-go/libs/darwin_arm64 => ./libs/darwin_arm64
	github.com/discobox-ai/iroh-go/libs/linux_amd64 => ./libs/linux_amd64
	github.com/discobox-ai/iroh-go/libs/linux_amd64_musl => ./libs/linux_amd64_musl
	github.com/discobox-ai/iroh-go/libs/linux_arm64 => ./libs/linux_arm64
	github.com/discobox-ai/iroh-go/libs/linux_arm64_musl => ./libs/linux_arm64_musl
	github.com/discobox-ai/iroh-go/libs/windows_amd64 => ./libs/windows_amd64
	github.com/discobox-ai/iroh-go/libs/windows_arm64 => ./libs/windows_arm64
)
