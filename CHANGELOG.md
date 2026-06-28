# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/)
and this project adheres to [Semantic Versioning](https://semver.org/).

> **Tag namespaces.** This is a multi-module workspace; each module is versioned and
> tagged independently: the root library `vX.Y.Z`, the CLI `cli/vX.Y.Z` (the installed
> `netx` binary + c-shared library), and per-module `drivers/<name>/vX.Y.Z` and
> `proto/<name>/vX.Y.Z`. A release "wave" bumps them bottom-up in lockstep
> (root → proto → drivers → cli — see `docs/internals/modules-cli.md`). Sections below
> are dated by wave and list the tags they cover. Older entries (≤ v1.2.0) predate the
> multi-module split and track the root module only.

---

## [Unreleased]

Branch work not yet in a tagged release (post `cli/v1.1.3`).

### Added

- `netx tun --eager`: pre-dial the `--to` upstream at startup to warm the obfuscation handshake before the first inbound connection (`cli/internal/eager.go`).
- `NETX_READY` readiness signal: `netx tun` emits a (secret-redacted) "started" log line once the listener is bound and serving, so embedders and the c-shared library can detect bind+serve.
- DTLS/DTLSPSK handshake tuning params: `mtu`, `flightinterval`, `nobackoff`, `skipcookie` (server), `resume`.

### Changed

- URI secrets (keys, certs, PSKs) are redacted in logs via driver-supplied protocol fingerprints (`Redacted()` across `uri.go`/`scheme.go`/`wrap.go`); info-level logs are kept secret-free.
- Added exported `netx.SPKIPinVerifier` (root module), shared by the tls/utls/dtls client drivers. This is an **additive** API addition: on release, bump the root module to a new **minor** version and re-tag `drivers/{tls,utls,dtls}` to require it (bottom-up, per CLAUDE.md).

### Fixed

- darwin: bind the netx dial socket to the physical interface (`IP_BOUND_IF`) so tunnels relay correctly inside an `NEPacketTunnelProvider` (`cli/internal/tun_darwin.go`).
- `netx tun`: log the upstream-dial lifecycle to aid connect diagnosis.
- `demux`: fixed a `close of closed channel` panic when a session is closed after (or concurrently with) the parent demux (`demuxSess.Close`).
- `demux`: fixed buffer aliasing in `demuxSess.Write`/`taggedDemuxSess.Write` that could corrupt a still-queued inbound payload and race concurrent writes; each write now builds a fresh frame buffer.
- `frame`: `FrameConn.Write` now rejects payloads larger than `MaxPacketSize` (65535) instead of silently truncating the 2-byte length header and desyncing the stream.
- `poll`: the `interval` parameter now rejects non-positive durations at parse time (a zero/negative interval removed the idle throttle, causing a poll flood).
- `Server`/`TunMaster`: a connection routed concurrently with `Close`/`Shutdown` is now closed instead of being tracked after the force-close pass, preventing a leaked connection and relay goroutine.

### Security

- TLS/DTLS/uTLS certificate pinning (client `cert=` param) now pins the **leaf** certificate only and rejects an empty peer chain. Previously the verifier matched the pinned SPKI at **any** chain position, letting an active MITM present `[attacker_leaf, pinned_cert]` and pass the pin under `InsecureSkipVerify`; the pin is also now a real SHA-256 digest (was a no-op `sha256.New().Sum`). **Behavior change:** a deployment that pinned an intermediate/CA on a multi-cert chain is now rejected — pin the leaf instead.

## [v1.4.0 · cli/v1.1.0–v1.1.3 · drivers,proto v1.1.x] - 2026-04-04

### Changed

- Reworked connection handling and parameter management across the wrapper pipeline: consistent param parsing/validation and connection lifecycle. (PR #32)
- Windows ARM64 c-shared library cross-compilation (PR #35, in `cli/v1.1.3`).

## [v1.3.0 · cli/v1.0.0 · drivers,proto v1.0.0] - 2026-02-22

### Added

- DNS tunneling: the `dnst` driver + `proto/dnst` (Base32-over-DNS; payload in the query QNAME, replies in TXT records) and the full stateless-tunnel stack `mux`+`dnst`+`demux`+`poll`+`split`+`frame`. (PR #27)

### Changed

- Reorganized into a dependency-light root library plus independently-versioned `proto/*` (heavy protocol implementations) and `drivers/*` (registration adapters) modules; the `tls`/`ssh`/`aesgcm`/`dtls`/`dtlspsk`/`tlspsk`/`utls` drivers are now separate modules activated by blank import. (PR #27)
- Moved CLI and library code under the `cli/` module and into `internal` to prevent `go install` of library internals: `cmd/netx_lib` → `cli/internal/lib`, `internal/cli` → `cli/internal`, `internal/tools/e2e` → `cli/internal/e2e`. (PR #27)

## [v1.2.0] - 2025-11-01

> Paths in this entry reflect the pre-split layout; several were relocated in v1.3.0 (see above).

### Added

- Library support via new `cmd/netx_lib` C-compatible shared library exposing `Netx` and `NetxInterrupt` with callback hooks for stdout/stderr. (PR #9)
- Refactored CLI into reusable `internal/cli` package to run programmatically with configurable I/O and args. (PR #9)
- ICMP transport support: `icmp` can now be used in URIs and through `Listen`/`Dial` (includes ICMP listener and client/server wrappers). (PR #9)
- End-to-end helpers/tests for the library interface under `internal/tools/e2e/lib`. (PR #9)
- Build tasks and CI steps to produce shared libraries for Linux, Windows, and Android; optional static archives for macOS/iOS. (PR #9)

### Changed

- `cmd/netx/main.go` now delegates to `internal/cli.Run`. (PR #9)
- Taskfile restructured: added `test:e2e:*` tasks, library build tasks, and unique artifact names. (PR #9)
- GitHub Actions updated: consolidated lint/test/build, added library builds, and automated release publishing. (PR #9)
- Dependency upgrades: `refraction-networking/utls` v1.8.1, `spf13/cobra` v1.10.1, `klauspost/compress` v1.18.1, and `spf13/pflag` v1.0.10. (PR #9)

### Fixed

- Correct half-duplex relay direction and error messages in `Tun.Relay`. (PR #9)
- Improved error logging in CLI and in tun dial path. (PR #9)

## [v1.1.0] - 2025-10-16

### Added

- `uri` package to marshal/unmarshal chain URIs for programmatic dial/listen support.
- SSH connection wrapper.
- `netx` CLI command.
- `netx tun` subcommand for composing listener/dialer chains with transports and wrappers.

## [v1.0.0] - 2025-09-20

### Added

- Initial release
