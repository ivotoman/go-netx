module github.com/pedramktb/go-netx/drivers/ss2022

go 1.25.7

require (
	github.com/pedramktb/go-netx v1.4.0
	github.com/pedramktb/go-netx/proto/socksaddr v0.0.0-00010101000000-000000000000
	github.com/pedramktb/go-netx/proto/ss2022 v0.0.0-00010101000000-000000000000
)

require (
	github.com/klauspost/cpuid/v2 v2.0.9 // indirect
	lukechampine.com/blake3 v1.4.1 // indirect
)

// Unpublished sibling modules — resolve locally until tagged/published.
replace (
	github.com/pedramktb/go-netx/proto/socksaddr => ../../proto/socksaddr
	github.com/pedramktb/go-netx/proto/ss2022 => ../../proto/ss2022
)
