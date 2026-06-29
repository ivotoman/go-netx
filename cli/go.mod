module github.com/pedramktb/go-netx/cli

go 1.25.7

require (
	github.com/miekg/dns v1.1.72
	github.com/pedramktb/go-netx v1.4.0
	github.com/pedramktb/go-netx/drivers/aesgcm v1.1.1
	github.com/pedramktb/go-netx/drivers/dnst v1.1.1
	github.com/pedramktb/go-netx/drivers/dtls v1.1.1
	github.com/pedramktb/go-netx/drivers/dtlspsk v1.1.1
	github.com/pedramktb/go-netx/drivers/ss2022 v0.0.0-00010101000000-000000000000
	github.com/pedramktb/go-netx/drivers/ssh v1.1.1
	github.com/pedramktb/go-netx/drivers/tls v1.1.1
	github.com/pedramktb/go-netx/drivers/tlspsk v1.1.1
	github.com/pedramktb/go-netx/drivers/trojan v0.0.0-00010101000000-000000000000
	github.com/pedramktb/go-netx/drivers/utls v1.1.1
	github.com/spf13/cobra v1.10.2
	golang.org/x/sys v0.42.0
)

require (
	github.com/andybalholm/brotli v1.2.1 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/klauspost/compress v1.18.5 // indirect
	github.com/klauspost/cpuid/v2 v2.0.9 // indirect
	github.com/pedramktb/go-netx/proto/aesgcm v1.1.0 // indirect
	github.com/pedramktb/go-netx/proto/dnst v1.1.0 // indirect
	github.com/pedramktb/go-netx/proto/prepend v0.0.0-00010101000000-000000000000 // indirect
	github.com/pedramktb/go-netx/proto/socksaddr v0.0.0-00010101000000-000000000000 // indirect
	github.com/pedramktb/go-netx/proto/ss2022 v0.0.0-00010101000000-000000000000 // indirect
	github.com/pedramktb/go-netx/proto/ssh v1.1.0 // indirect
	github.com/pedramktb/go-netx/proto/trojan v0.0.0-00010101000000-000000000000 // indirect
	github.com/pion/dtls/v3 v3.1.2 // indirect
	github.com/pion/logging v0.2.4 // indirect
	github.com/pion/transport/v3 v3.1.1 // indirect
	github.com/pion/transport/v4 v4.0.1 // indirect
	github.com/raff/tls-ext v1.0.0 // indirect
	github.com/raff/tls-psk v1.0.0 // indirect
	github.com/refraction-networking/utls v1.8.2 // indirect
	github.com/spf13/pflag v1.0.10 // indirect
	golang.org/x/crypto v0.49.0 // indirect
	golang.org/x/mod v0.34.0 // indirect
	golang.org/x/net v0.52.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/tools v0.43.0 // indirect
)

// Unpublished Stage-1 proxy-carrier modules — resolve locally until tagged/published
// (then drop these replaces and bump the requires to real versions, like the v1.1.1
// drivers above).
replace (
	github.com/pedramktb/go-netx/drivers/ss2022 => ../drivers/ss2022
	github.com/pedramktb/go-netx/drivers/trojan => ../drivers/trojan
	github.com/pedramktb/go-netx/proto/prepend => ../proto/prepend
	github.com/pedramktb/go-netx/proto/socksaddr => ../proto/socksaddr
	github.com/pedramktb/go-netx/proto/ss2022 => ../proto/ss2022
	github.com/pedramktb/go-netx/proto/trojan => ../proto/trojan
)
