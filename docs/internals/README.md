# go-netx Internals — Change-Safety Guide

This directory documents the **building blocks, their dependencies, and the invariants you must not break**. It exists so that a change in one place does not silently break a peer, a downstream layer, or an external module consumer. Each subsystem has its own file; this index gives the map, the cross-cutting contracts that span subsystems, and a consolidated hazard list.

> **Source is the source of truth.** This guide was produced by reading the code at `file:line`, not the prose docs. Several statements in the top-level `README.md`, in `docs/mux-tag-poll.md`, and in some in-file doc-comments are **stale** — see [Documentation drift](#documentation-drift) before trusting them.

## Subsystem map

| Doc | Covers | Source |
|---|---|---|
| [pipeline.md](pipeline.md) | Driver registry, `Wrapper` typed pipeline, `Scheme`/`URI` parsing & chain validation, how to add a driver | `driver.go`, `wrap.go`, `scheme.go`, `uri.go`, `transport.go`, `dial.go` |
| [mux.md](mux.md) | `Mux` (server → `TaggedConn` fan-in/router) and `MuxClient` (client → EOF-cycling `net.Conn`) | `mux.go`, `mux_client.go` |
| [demux.md](demux.md) | Session multiplexer over one conn; `Demux`, `TaggedDemux`, `DemuxClient`; ID-prefix framing & queues | `demux*.go` |
| [poll-tagged.md](poll-tagged.md) | `PollConn` (request-response → persistent stream) and the `TaggedConn`/`TaggedPipe` tag contract | `poll_conn.go`, `tagged_conn.go`, `tagged_pipe.go` |
| [stream-transforms.md](stream-transforms.md) | `BufConn`, `FrameConn`, `SplitConn` and the `Flush`/`MaxWrite` capability interfaces | `buffered_conn.go`, `frame_conn.go`, `split_conn.go` |
| [server-tun.md](server-tun.md) | `Server[ID]` routing & lifecycle, `Tun`/`TunMaster[ID]` relay | `server.go`, `tun.go`, `logger.go` |
| [icmp.md](icmp.md) | ICMP Echo Request/Reply transport, custom listener, raw-socket constraints | `icmp_conn.go`, `icmp_listener.go`, `ip.go`, `packet.go` |
| [drivers-proto.md](drivers-proto.md) | All `drivers/*` registration adapters and `proto/*` protocol conns; param matrix; security patterns | `drivers/*`, `proto/*` |
| [modules-cli.md](modules-cli.md) | go.work module graph, independent versioning & release ordering, CLI, c-shared FFI library, CI | `go.work`, all `go.mod`, `Taskfile.yml`, `cli/*`, `.github/workflows/*` |

## How the layers compose

The DNS tunnel is the canonical full stack and exercises almost every block. Server and client chains (from the e2e harness, `Taskfile.yml`):

```
server:  udp + mux + dnst{domain} + demux{id} + poll + split + frame  →  udp (peer)
client:  udp                                                          →  udp + mux + dnst + demux + poll + split + frame
```

Read that as a pipeline of types (see [pipeline.md](pipeline.md) for the transition rules):

```
net.Listener ──mux──▶ TaggedConn ──dnst──▶ TaggedConn ──demux──▶ net.Listener (sessions)
                                                                   each session: net.Conn ──poll──▶ ──split──▶ ──frame──▶ net.Conn
```

The pipeline parser validates these type transitions **at parse time** and rejects a chain that does not end in a `Listener` (server) or `Dialer` (client).

## Cross-cutting contracts (break these and something far away breaks)

### 1. Wire formats — changing any breaks interop with already-deployed peers
- **Frame header:** 2-byte big-endian `uint16` length prefix (`frame_conn.go:71,75,94,95`). *Not* 4-byte despite comments. No max-size enforcement: a payload > 65535 is silently truncated by `uint16(len(p))` (`frame_conn.go:95`).
- **Demux session ID:** fixed-length raw byte prefix `[id][payload]`; the length **and** value must match on both ends (`demux.go:201`, `demux_client.go:55-62`). A length mismatch silently misroutes.
- **aesgcm:** 12-byte IV; last 8 bytes XOR an 8-byte big-endian sequence counter; no extra framing (`proto/aesgcm/aesgcm_conn.go`). Changing the IV/seq scheme risks GCM nonce reuse.
- **dnst:** base32 (std alphabet, no padding), 63-byte DNS labels, 255-byte TXT, 253-byte QNAME cap; client and server `maxQNAMEPayload` math must stay in sync (`proto/dnst/dnst_conn.go`).
- **ICMP:** Echo Request (client) / Echo Reply (server) carrying data in the Echo `Data` field; client identifier is a constant `1`, peers are demuxed by **source IP only**, server echoes the last-seen id/seq (`icmp_conn.go:52,102-107,136-148`).
- **PSK/TLS suites are pinned:** `tlspsk` = TLS 1.2 `TLS_PSK_WITH_AES_256_CBC_SHA`; `dtlspsk` = `TLS_PSK_WITH_AES_128_GCM_SHA256`; `tls`/`utls` are TLS 1.3 only. Changing a suite breaks interop (`drivers/*`).

### 2. Capability interfaces — satisfied structurally (by method presence), so stacking order matters
- **Flush capability:** `type BufConn interface { net.Conn; Flush() error }` (`buffered_conn.go:58`). Implemented **only** by `bufConn`; consumed **only** by `FrameConn`, which flushes after writing header+payload to coalesce them (`frame_conn.go:106-110`). `FrameConn`/`SplitConn` embed the `net.Conn` *interface* and therefore do **not** re-expose `Flush`. ⇒ put `buf` directly **below** `frame`.
- **MaxWrite capability:** an inline, unnamed `interface{ MaxWrite() uint16 }` duplicated across ≥6 files. Providers: `proto/dnst` (computed from domain; server default 765), `proto/aesgcm`, demux sessions, `PollConn`. Consumers: `SplitConn` (**errors if `MaxWrite()`==0 or absent** — `split_conn.go:47`), `demux` (subtracts the ID length; errors if `MaxWrite <= idMask`), `aesgcm`, `poll`. `SplitConn` deliberately drops `MaxWrite` (it has satisfied the limit). ⇒ `split` must sit directly **above** the `MaxWrite`-bearing transport, with no `net.Conn`-embedding layer (e.g. `frame`) in between hiding it.

### 3. The tag contract (request/response correlation)
`TaggedConn` (`tagged_conn.go:11`) carries an opaque `tag any` from the read path to the matching write path. Invariant: **one `WriteTagged` consumes exactly one tag produced by a prior `ReadTagged`.** `TaggedDemux` enforces the read→write hand-off via a per-session tag queue (`demux_tagged.go:200-266`); a `Write` with no pending tag *blocks*, and any read/write imbalance correlates a DNS response to the wrong request. Most implementations guard `if tag != nil` before dereferencing — **except** plain `dnst` `serverConn.ReadTagged` (`proto/dnst/dnst_conn.go:115`), which panics on a nil tag (latent bug; keep the guard convention when editing).

### 4. Registration & module wiring
- `netx.Register` **panics** on a nil driver or a duplicate name (`driver.go:18-23`). The registered name *is* the URI keyword and is matched lowercase.
- A driver only activates when its package is **blank-imported**. A new driver must be added to **both** `cli/cmd/netx/main.go` and `cli/internal/lib/main.go` — miss one and that artifact (binary *or* FFI lib) lacks it with **no compile error**, only a runtime `uri: unknown driver`.
- The same driver name must produce a correct `Wrapper` for **both** `listener=true` and `listener=false`; the flag is threaded through every `UnmarshalText` and the driver call (`uri.go:43`→`scheme.go:69`→`wrap.go:71,272,300`).

### 5. Independent module versioning (release ordering)
`go.work` builds every module from local source, so a change to a shared root type **compiles and tests green locally** while every `go.mod` still pins the old root version — external `go get` consumers then break. Bump and tag **bottom-up**: `root` → `proto/aesgcm`,`proto/dnst` → all `drivers/*` → `cli`. (`proto/ssh` is exempt — it has no root dependency.) See [modules-cli.md](modules-cli.md) for the full graph.

## Consolidated top change hazards

Highest-leverage things to check before editing (each links to detail):

1. **Adding/altering a `Wrapper` capability requires 4 edits in lockstep** — the struct field, `InputTypes`, `OutputFor`, and `Apply` (`wrap.go`); a new `PipeType` also needs `PipeType.String()`. Mismatch → false parse-rejection or runtime `incompatible type`. ([pipeline.md](pipeline.md))
2. **Wire-format edits break deployed peers** — frame header width/endianness, demux ID length, aesgcm IV/seq, dnst encoding, ICMP framing, PSK suites. (§1 above)
3. **Capability stacking order** — moving `buf`/`frame`/`split` relative to each other or to a `MaxWrite` transport silently disables flush-coalescing or `MaxWrite` chunking. (§2 above)
4. **Tag accounting** — unbalanced read/write tag flow corrupts response routing; nil-tag deref panics. (§3 above; [poll-tagged.md](poll-tagged.md), [demux.md](demux.md))
5. **`closed()` lifecycle** — a matched `Server` handler that never calls `closed()` makes `Shutdown` wait the full deadline; calling it twice deadlocks a goroutine on `closeCooldown` (`server.go`). ([server-tun.md](server-tun.md))
6. **Queue sizing is loss/HoL, not throttling** — demux/mux/poll queues drop on full or cause head-of-line blocking depending on the layer; "fixing" a drop by making a send blocking can stall a single read loop and starve all sessions. ([demux.md](demux.md), [mux.md](mux.md))
7. **Concurrency-safety differs per conn type** — `frameConn` guards Read+Write with `rmu`/`wmu`, but `bufConn` and `splitConn` have **no write lock**; concurrent writes corrupt data. ([stream-transforms.md](stream-transforms.md))
8. **Blank-import drift & release ordering** (§4, §5 above) — silent at compile time, breaks at runtime or for external consumers.
9. **Raw-socket privileges & per-peer demux for ICMP** — needs `CAP_NET_RAW`/root; two clients behind one NAT IP collapse into one peer conn. ([icmp.md](icmp.md))
10. **Security regressions in drivers** — don't decouple `VerifyPeerCertificate` from `InsecureSkipVerify`, weaken PSK suites, or change one side of an SPKI comparison without the other. ([drivers-proto.md](drivers-proto.md))

## Documentation drift

Discovered while writing this guide — the **code is correct; these docs/comments are stale**. Worth fixing, but until then, do not trust them:

- `frame_conn.go:4,52` comment and top-level `README.md` say the frame header is **4-byte**; it is **2-byte** (`uint16`).
- `README.md` references `NewFramedConn`, `WithMaxFrameSize`, `ErrFrameTooLarge`, and a `maxsize` frame param — none exist. The constructor is `NewFrameConn`; the `frame` driver accepts no params.
- `README.md` and `buffered_conn.go:84` reference `WithBufSize`/`WithBufReaderSize`/`WithBufWriterSize`; the real options are `WithBufRead`/`WithBufWrite` (params `r`/`w`).
- `README.md` and `docs/mux-tag-poll.md` describe `Mux` (server) as adapting a `net.Listener` into a single `net.Conn`; it actually produces a `TaggedConn` fan-in router (`mux.go` header comment is correct; the README mentions a `WithMuxClientRemoteAddr` option that does not exist).
- `README.md` lists a `WithPollBufSize` poll option; it does not exist — the poll buffer is fixed at `MaxPacketSize` (65535).
- The CLI `uriFormat` help text and `README.md` advertise a `reality` driver; no such driver is registered (it is commented out in the e2e harness).
- `proto/dnst/dnst_conn.go` declares `package netx` rather than a `dnst`/`dnstproto` name (inconsistent with the other proto modules).
- `spkiVerifier` (in `drivers/tls`, `drivers/dtls`, `drivers/utls`) computes `sha256.New().Sum(rawSPKI)`, which appends the hash-of-empty to the raw SPKI bytes rather than hashing the SPKI. It is self-consistent (both ends compute it the same way, so pinning still functions as a raw-SPKI equality check) but is misleadingly named and not the SHA-256 pin it appears to be.
- `netx --version` always prints `dev` (`cli/internal/root.go:56`); `CHANGELOG.md` is stale (last entry v1.2.0) and contains dead paths.
