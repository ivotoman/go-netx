# <img src="./netx.svg" alt="netX" />

netX ("network extended") is a collection of small, focused, composable extensions to Go's `net` standard library.

Every building block is a drop-in `net.Conn` / `net.Listener` (or the `TaggedConn` variant), so they integrate with the standard library without heavy abstractions. They can be wired together in code or composed from URI strings via a pluggable driver pipeline.

- **Full API reference:** [pkg.go.dev/github.com/pedramktb/go-netx](https://pkg.go.dev/github.com/pedramktb/go-netx)
- **Architecture, invariants & change-hazards:** [`docs/internals/README.md`](docs/internals/README.md)

## Contents

- [Highlights](#highlights)
- [Installation](#installation)
- [Library usage](#library-usage)
- [CLI](#cli)
  - [Quick start](#quick-start)
  - [Install and build](#install-and-build)
  - [Example commands](#example-commands)
  - [Chain syntax reference](#chain-syntax-reference)

## Highlights

Each block is a `net.Conn`/`net.Listener` you can stack with the others (see the [godoc](https://pkg.go.dev/github.com/pedramktb/go-netx) for constructors and options):

- **Buffered & framed conns** — buffered read/write with explicit `Flush`, and a length-prefixed framing layer that preserves message boundaries over a stream (e.g. UDP over TCP).
- **Mux / MuxClient** — collapse an accepted-connection listener into a single conn (server) or an auto-redialing conn (client), transparently cycling on EOF.
- **Demux / DemuxClient** — session-multiplex many virtual conns over one underlying conn using a fixed-length ID prefix.
- **Poll conn** — turn a request-response transport into a persistent bidirectional stream (the basis for tunneling over poll-only protocols like DNS).
- **Tagged conns** — `TaggedConn` carries an opaque tag from the read path to the matching write path, for request/response correlation.
- **Runtime-routable server & tunneling** — `Server[ID]` routes accepted conns to handlers you register/swap at runtime; `Tun`/`TunMaster[ID]` relay two conns bidirectionally.
- **Driver / wrapper pipeline** — compose transports and wrappers into type-checked chains, in code or from URI strings.
- **Transports & obfuscation** — `tcp`, `udp`, and `icmp` transports; TLS/uTLS/DTLS/PSK, AES-GCM, SSH, and DNS-tunnel (`dnst`) wrappers.

## Installation

```bash
go get github.com/pedramktb/go-netx@latest
```

```go
import netx "github.com/pedramktb/go-netx"
```

Heavier protocol implementations and the pluggable drivers live in separate, independently-versioned modules. A driver only activates when its package is **blank-imported**:

```go
import (
    _ "github.com/pedramktb/go-netx/drivers/tls"    // registers the "tls" driver
    _ "github.com/pedramktb/go-netx/drivers/dnst"   // registers the "dnst" driver
    // ... drivers/{aesgcm,dtls,dtlspsk,ssh,tlspsk,utls}
)
```

The core wrappers (`buf`, `frame`, `mux`, `demux`, `poll`, `split`) are registered automatically by the root module.

## Library usage

The recommended entry point is the URI/Scheme API: a `<transport>+<wrapper>...://<address>` string is parsed into a type-checked pipeline (see [Chain syntax reference](#chain-syntax-reference)). The chain is validated at parse time and rejected immediately if the wrapper types don't compose.

```go
ctx := context.Background()

// Server: TCP, framed, AES-GCM encrypted, buffered for write coalescing.
var lu netx.ListenerURI
_ = lu.UnmarshalText([]byte("tcp+buf{r=8192,w=8192}+frame+aesgcm{key=<hex>}://:9000"))
ln, _ := lu.Listen(ctx)

// Client: dial the matching chain.
var du netx.DialerURI
_ = du.UnmarshalText([]byte("tcp+buf{r=8192,w=8192}+frame+aesgcm{key=<hex>}://server:9000"))
conn, _ := du.Dial(ctx)
```

The individual building blocks are also usable directly as plain `net.Conn`/`net.Listener` values (`netx.NewBufConn`, `netx.NewFrameConn`, `netx.NewMux`, `netx.NewDemux`, `netx.NewPollConn`, `netx.Server`, `netx.Tun`, ...). See the [godoc](https://pkg.go.dev/github.com/pedramktb/go-netx) for the full API and [`docs/internals/`](docs/internals/README.md) for the data-flow diagrams, capability interfaces, and the invariants you must not break when composing them.

## CLI

The `netx` CLI (module `cli/`) exposes a `tun` subcommand that relays between two chainable endpoints: it listens on the `--from` chain and dials the `--to` chain, relaying bytes between them.

### Quick start

1. Install the CLI:

   ```bash
   go install github.com/pedramktb/go-netx/cli/cmd/netx@latest
   ```

2. Compose `--from` (listen) and `--to` (dial) URIs. All keys/certs/PSKs are **hex-encoded**; quote the URIs so the shell does not mangle `+`, `{`, `}`, or `,`:

   ```bash
   netx tun \
     --from "tcp+tls{cert=$(xxd -p server.crt | tr -d '\n'),key=$(xxd -p server.key | tr -d '\n')}://:9000" \
     --to   "udp+aesgcm{key=$(openssl rand -hex 16)}://127.0.0.1:5555"
   ```

3. Adjust verbosity with `--log debug|info|warn|error` (default `info`); `Ctrl+C` triggers a graceful shutdown.

Flags:

| Flag | Description |
|---|---|
| `--from <uri>` | Incoming (listen) chain. **Required.** |
| `--to <uri>` | Peer (dial) chain. **Required.** |
| `--eager` | Pre-dial the `--to` upstream at startup to warm the obfuscation handshake before the first inbound connection. |
| `--log <level>` | `debug` \| `info` \| `warn` \| `error` (default `info`). |

> Secrets in URIs are redacted from logs (replaced with a short protocol fingerprint), so `--from`/`--to` can be logged safely.

### Install and build

```bash
go install github.com/pedramktb/go-netx/cli/cmd/netx@latest   # install the binary
task build                                                    # cross-compile binaries + c-shared libs (see Taskfile.yml)
```

### Example commands

All chains below are taken from the e2e suite (`Taskfile.yml`), which round-trips real traffic through them. Replace `<hex...>` with hex-encoded material (`xxd -p file`, `openssl rand -hex N`). The obfuscation lives on the server's `--from` and the client's `--to`; the plain side forwards to/from the real service.

```bash
# TLS-terminating front: encrypted inbound -> plain upstream service
netx tun --from "tcp+tls{cert=<hexPEM>,key=<hexPEM>}://:9000" --to "tcp://service:8080"
# matching client: plain local listener -> TLS to the server (cert = SPKI pin, optional)
netx tun --from "tcp://:9000" --to "tcp+tls{cert=<hexPEM>}://server:9000"

# AES-GCM over TCP, buffered + framed
netx tun --from "tcp://:7000" --to "tcp+buf{r=8192,w=8192}+frame+aesgcm{key=<hex16>}://server:7000"

# DTLS-PSK over UDP (PSK clients must send an identity)
netx tun --from "udp://:4444" --to "udp+dtlspsk{identity=client1,key=<hex>}://server:4444"

# DNS tunnel — the SAME stateless-tunnel stack on both ends.
# Server is authoritative for the domain; client points --to at a recursive resolver that delegates it.
netx tun --from "udp+mux+dnst{domain=t.example.com}+demux{id=0000}+poll+split+frame://:53" --to "tcp://service:8080"
netx tun --from "tcp://:1080" --to "udp+mux+dnst{domain=t.example.com}+demux{id=0000}+poll+split+frame://resolver:53"
```

### Chain syntax reference

A chain is `<transport>+<wrapper1>{params}+<wrapper2>...://host:port`. Wrappers apply left-to-right; the chain is type-checked at parse time and must compose into a listener (server) or dialer (client), so **order matters**.

**Transports:** `tcp`, `udp`, `icmp` (ICMP tunnels over Echo Request/Reply; requires `CAP_NET_RAW`/root).

**Core wrappers** (auto-registered):

| Wrapper | Purpose | Params |
|---|---|---|
| `buf` | Buffered read/write (coalesces frame writes) | `r`, `w` (sizes, optional) |
| `frame` | 2-byte length-prefixed framing for packet semantics over a stream | *(none)* |
| `mux` | Server: fan-in a listener; client: auto-redialing conn | *(none)* |
| `demux` | Session multiplex over one conn | `id` (hex, **required on both ends** — sets session-ID length), `accq` (accept queue, default 1), `rq` (session read queue, default 128) |
| `poll` | Request-response → persistent bidirectional stream | `interval` (client only), `timeout` (server only), `sendq`, `recvq` |
| `split` | Chunk writes to the downstream `MaxWrite` limit (used in the DNS stack) | *(none)* |

**Driver wrappers** (activate via blank import of `drivers/<name>`):

| Wrapper | Purpose | Params |
|---|---|---|
| `aesgcm` | AES-GCM with passive IV/seq exchange | `key` (hex AES key: 16/24/32 bytes) |
| `tls` | TLS 1.3 | server: `cert`, `key`; client: `cert` (SPKI pin) **or** `servername` |
| `utls` | uTLS client-fingerprint camouflage (client only) | `cert` (pin) or `servername`; `hello` (`chrome`\|`firefox`\|`ios`\|`android`\|`safari`\|`edge`\|`randomized`\|`randomizednoalpn`; default `chrome`) |
| `dtls` | DTLS 1.3 | server: `cert`, `key`; client: `cert` or `servername`; tuning: `mtu`, `flightinterval`, `nobackoff`, `resume`, `skipcookie` (server) |
| `tlspsk` | TLS 1.2 PSK (`TLS_PSK_WITH_AES_256_CBC_SHA`) | `key`; `identity` (**required**, client) |
| `dtlspsk` | DTLS PSK (`TLS_PSK_WITH_AES_128_GCM_SHA256`) | `key`; `identity` (**required**, client); tuning: `mtu`, `flightinterval`, `nobackoff`, `skipcookie`, `resume` |
| `dnst` | DNS tunnel encoding (Base32 in the query QNAME; replies in TXT) | `domain` (required); `maxw` (server write cap, default 765) |
| `ssh` | SSH `direct-tcpip` tunneling | server: `key` (required), plus `pub` or `pass`; client: `pub` (required), plus `key` or `pass` |

**Notes:**

- All passwords, keys, and certificates are **hex-encoded** (drivers `hex.DecodeString` them); certs/keys are the hex of the PEM bytes.
- Client-side `cert` on `tls`/`utls`/`dtls` switches off default chain validation and pins the server's SubjectPublicKeyInfo against the provided certificate — connections fail if the server presents a different key.
- Composition order is enforced: e.g. `split` must sit directly above the `MaxWrite`-bearing layer, and `buf` directly below `frame` for write coalescing — see [`docs/internals/`](docs/internals/README.md).
- See [`docs/mux-tag-poll.md`](docs/mux-tag-poll.md) for the mux/demux/poll/tagged data-flow diagrams.
```
