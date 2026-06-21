module github.com/pedramktb/go-netx/drivers/trojan

go 1.25.7

require (
	github.com/pedramktb/go-netx v1.4.0
	github.com/pedramktb/go-netx/proto/trojan v0.0.0-00010101000000-000000000000
)

require (
	github.com/pedramktb/go-netx/proto/prepend v0.0.0-00010101000000-000000000000 // indirect
	github.com/pedramktb/go-netx/proto/socksaddr v0.0.0-00010101000000-000000000000 // indirect
)

// proto/* modules are unpublished (no tags yet); resolve them from the local tree
// until they are tagged/published alongside this driver. The go.work workspace
// also maps these, but explicit replaces keep `go build`/`go test` of this module
// working without the workspace (e.g. when the relay vendors it).
replace (
	github.com/pedramktb/go-netx/proto/prepend => ../../proto/prepend
	github.com/pedramktb/go-netx/proto/socksaddr => ../../proto/socksaddr
	github.com/pedramktb/go-netx/proto/trojan => ../../proto/trojan
)
