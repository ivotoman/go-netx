# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

netX ("network extended") is a collection of small, composable extensions to Go's `net` standard library: buffered/framed conns, a session multiplexer (mux/demux), polling conns, tagged conns, a runtime-routable server, tunneling, and a pluggable driver/wrapper pipeline for composing all of these from URI strings. This file covers the workspace layout, the wrapper-pipeline architecture, and cross-cutting workflows.

## Authoritative internals + doc drift

**`docs/internals/` is the change-safety reference** — read [`docs/internals/README.md`](docs/internals/README.md) before modifying any subsystem. It documents each building block's invariants, dependencies, and "change hazards" (what breaks elsewhere when you touch X). One doc per subsystem (pipeline, mux, demux, poll-tagged, stream-transforms, server-tun, icmp, drivers-proto, modules-cli). **Cite code by symbol + file, not `file:line`** — line numbers rot on every refactor (this doc set has been bitten by it).

⚠️ **The prose docs were re-audited and corrected against the code (2026-06-24); a few in-code comment/help quirks remain — trust the source over those strings.** The code is correct, but: `frame_conn.go` comments say the frame header is "4-byte" when it is **2-byte** (`uint16`); `buffered_conn.go` names nonexistent `WithBuf{Writer,Reader}Size` (real options: `WithBufWrite`/`WithBufRead`); and `cli/internal/help.go` advertises `frame{maxsize}` / `aesgcm{maxpacket}` params that don't exist (`frame` takes no params; `aesgcm` takes only `key`). See the "Documentation drift" section of `docs/internals/README.md` for the canonical list.

Known bugs, cleanups, and improvement opportunities are tracked in [`docs/IMPROVEMENTS.md`](docs/IMPROVEMENTS.md) — an impact×urgency backlog (check it before touching a subsystem; several Q1 items are active correctness/security bugs).

## Commands

This repo uses [Task](https://taskfile.dev) (`Taskfile.yml`), not `make`.

```bash
task deps              # install golangci-lint + download module deps
task lint              # golangci-lint run
task test              # run `go test -v ./...` in EVERY workspace module
task build             # cross-compile CLI binaries + c-shared libs for all platforms
task test:e2e:tun      # spin up real tunnels (TLS/DTLS/SSH/aesgcm/dnst/...) and assert echo round-trips
task test:e2e:tun lib=true   # same e2e suite, but exercising the c-shared library build
```

⚠️ `task test:e2e:tun` binds **fixed host ports** (≈48080–50050) and runs `pkill netx` / `pkill netx_lib` on teardown — it will kill *all* `netx` processes on the machine and fails if those ports are already in use.

Because this is a **multi-module workspace**, `go test ./...` from the root only covers the root module. To test or run go commands against a specific submodule, `cd` into it first (`cd drivers/tls && go test ./...`). `task test` handles this by iterating `go list -m`.

Run a single test: `go test -run TestName ./...` from within the relevant module directory (e.g. `go test -run TestPollConn .` in the repo root for `poll_conn_test.go`).

CGO cross-compilation in `task build` requires the toolchains installed in `.github/workflows/lint_test_and_build.yml` (gcc-mingw, aarch64 gcc, llvm-mingw for Windows ARM64, Android NDK via `$ANDROID_NDK_HOME`). Plain `go build ./...` per module works without these.

## Workspace layout

`go.work` ties together independently-versioned modules. **Each is published and versioned separately** — changing a shared type in the root module means bumping the dependency in every dependent module's `go.mod`. Bump and tag **bottom-up**: root → `proto/*` → `drivers/*` → `cli` (root is currently v1.4.0, `proto/*` v1.1.x, `drivers/*` v1.1.x; the CLI is released as `cli/vX.Y.Z`). `proto/ssh` is exempt from the root bump — it has no root dependency.

- **Root** (`github.com/pedramktb/go-netx`) — the core library. Deliberately light on dependencies (only `pion/transport` for UDP and `golang.org/x/net`). All the core types live here as flat top-level `.go` files (`mux.go`, `demux.go`, `poll_conn.go`, `wrap.go`, `scheme.go`, `uri.go`, `server.go`, `tun.go`, ...). Core drivers `buf`, `frame`, `mux`, `demux`, `poll`, `split` self-register here via `init()`.
- **`proto/*`** (`aesgcm`, `dnst`, `ssh`) — heavier protocol *implementations* as `net.Conn`/`TaggedConn` types, isolated so the root stays dependency-light (e.g. `dnst` pulls in `miekg/dns`).
- **`drivers/*`** (`aesgcm`, `dnst`, `dtls`, `dtlspsk`, `ssh`, `tls`, `tlspsk`, `utls`) — thin adapters that `netx.Register(...)` a named driver, usually wiring a `proto/*` type into a `Wrapper`. Activated by **blank import**.
- **`cli/`** — the `netx` cobra CLI (`cli/cmd/netx`) and a c-shared library (`cli/internal/lib`, exporting `Netx`/`NetxInterrupt` for FFI). Both blank-import the full driver set. `cli/internal/e2e/*` holds echo servers/clients used by the e2e Taskfile target (gated behind the `e2e` build tag).

## The driver / wrapper pipeline (central abstraction)

Everything composable flows through one typed pipeline, defined in `wrap.go`, `scheme.go`, `uri.go`, `driver.go`, `transport.go`:

```
URI  = Scheme + "://" + Addr
Scheme = Transport + "+" wrapper1 + "+" wrapper2 + ...
Transport ∈ {tcp, udp, icmp}      (transport.go, dial.go — the base net.Listener/Dialer)
Wrapper = one transformation step  (wrap.go)
Driver = factory: (params, listener bool) -> Wrapper   (driver.go, global registry)
```

A **`Wrapper`** is a struct of optional function fields, one per `(inputType -> outputType)` transition across the four `PipeType`s: `Listener`, `Dialer`, `Conn`, `TaggedConn` (e.g. `ListenerToListener`, `ConnToTagged`, `DialerToConn`). A wrapper sets only the fields it supports. `Wrapper.Apply` type-switches on the runtime value; `Wrapper.OutputFor` reports the resulting type for chain validation.

`Wrappers.UnmarshalText` parses a `+`-joined chain and **validates type compatibility at parse time**: it threads a `currentType` starting at `Listener` (server side) or `Dialer` (client side) through each wrapper's `OutputFor`, and rejects the chain unless the final type is `Listener` (server) or `Dialer` (client). This is why an invalid chain fails immediately rather than at connection time.

The `listener bool` / `server bool` parameter is threaded through *every* `UnmarshalText` and driver call. The **same driver name produces different wrappers for the server vs client side** (e.g. `tls` server needs `cert`+`key` and wraps via `tls.Server`; `tls` client uses `tls.Client` with optional SPKI-pinning `cert` or a `servername`). Always check which side you're on when reading/writing a driver.

`ConnWrapListener` / `ConnWrapDialer` (in `wrap.go`) are the standard adapters that lift a simple `ConnToConn` function into `ListenerToListener` / `DialerToDialer` form — most drivers use these so one `connToConn` covers all three pipe positions.

### TaggedConn and the stateless-tunnel stack

`TaggedConn` (a `net.Conn` variant with `ReadTagged`/`WriteTagged`) carries an opaque tag from the read path to the write path. This exists for request-response protocols where a response must reuse the originating request's context — the canonical case being DNS, where the server can only send data as a reply to a query. The DNS tunnel composes as `mux + dnst + demux + poll + split + frame` (see `docs/mux-tag-poll.md` for the data-flow diagrams and `docs/internals/poll-tagged.md`/`demux.md` for the per-type invariants). When touching `dnst`, `poll_conn.go`, `demux_tagged.go`, or `tagged_*.go`, read those first — the layering is subtle.

## Adding a new driver

A new pluggable transform touches several places by design:

1. Create `drivers/<name>/` as its own module (`go.mod` requiring the root module, plus any `proto/<name>` you add).
2. In an `init()`, call `netx.Register("<name>", func(params, listener bool) (netx.Wrapper, error) { ... })`. Validate params (parse them in a `switch` with a `default:` case returning an `unknown <name> parameter %q` error — pattern in `drivers/tls/tls.go`), branch on `listener` for server vs client, set only the relevant `Wrapper` function fields. For cert/key/PSK params, populate `Wrapper.SecretParams` so they are redacted from logs.
3. Add the module path to `go.work`.
4. Blank-import it in **both** `cli/cmd/netx/main.go` and `cli/internal/lib/main.go` so the CLI and the shared library pick it up.
5. If it's a core, dependency-light transform, register it directly in the root module instead (like `split_conn.go`) — no separate module needed.

## Conventions

- All secrets in URIs — keys, certificates, passwords — are **hex-encoded strings** (drivers `hex.DecodeString` them). The e2e target shows the `xxd -p` / `openssl rand -hex` patterns.
- Driver param **keys** (not values) are lowercased and trimmed during parse (in `Wrapper.UnmarshalText`); unknown params should return an error, not be silently ignored (see existing drivers).
- `netx.Register` **panics** on a duplicate name or nil driver — registration is one-shot per process.
- The `Server[ID]`/`TunMaster[ID]` route handlers use copy-on-write maps; `SetRoute`/`RemoveRoute` are concurrency-safe. A handler must call its `closed()` callback exactly once when done so the server stops tracking the conn.
- A nil `Logger` falls back to `slog.Default()`.
