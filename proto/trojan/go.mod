module github.com/pedramktb/go-netx/proto/trojan

go 1.25.7

require (
	github.com/pedramktb/go-netx/proto/prepend v0.0.0-00010101000000-000000000000
	github.com/pedramktb/go-netx/proto/socksaddr v0.0.0-00010101000000-000000000000
)

// Unpublished sibling modules — resolve locally until tagged/published.
replace (
	github.com/pedramktb/go-netx/proto/prepend => ../prepend
	github.com/pedramktb/go-netx/proto/socksaddr => ../socksaddr
)
