module github.com/pedramktb/go-netx/proto/ss2022

go 1.25.7

require (
	github.com/pedramktb/go-netx/proto/socksaddr v0.0.0-00010101000000-000000000000
	lukechampine.com/blake3 v1.4.1
)

require github.com/klauspost/cpuid/v2 v2.0.9 // indirect

// Unpublished sibling module — resolve locally until tagged/published.
replace github.com/pedramktb/go-netx/proto/socksaddr => ../socksaddr
