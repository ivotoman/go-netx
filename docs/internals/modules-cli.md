# Workspace Modules, CLI & FFI

Internals reference for the multi-module `go.work` workspace, the `cli/` cobra
binary, and the c-shared FFI library. Code references cite **symbol + file**, not
line numbers (line numbers rot on every refactor). Where the repo is internally
inconsistent (e.g. the CHANGELOG vs. the actual layout), it is flagged as
AMBIGUITY.

The single most important section is **Change hazards** (last). Read it before
touching any shared root type, adding a driver, or editing the FFI library.

---

## Module map & dependency graph

The workspace is declared in `go.work` (`go 1.25.7`) and contains **13 modules**:
the root lib `.`, the binary+FFI `./cli`, three `./proto/*` (aesgcm, dnst, ssh),
and eight `./drivers/*` (aesgcm, dnst, dtls, dtlspsk, ssh, tls, tlspsk, utls).

`go.work` lists every local module, so **within this repo all cross-module
imports resolve to the on-disk source, NOT the versions pinned in each
`go.mod`**. That masking is the root cause of the release-ordering hazard
(Change hazard #1) — explained once there.

### Internal dependency edges (who requires which sibling @ which version)

Read from each module's `go.mod` `require` stanza. **`proto/ssh` is the only
module with no root dependency** (it pulls only `golang.org/x/crypto` + `x/sys`),
so a breaking root change does not force a `proto/ssh` bump.

| Module | Requires sibling(s) @ version | Source |
|---|---|---|
| root `go-netx` | (none — only external) | `go.mod` require |
| `proto/aesgcm` | root `go-netx` @ **v1.4.0** | `proto/aesgcm/go.mod` |
| `proto/dnst` | root `go-netx` @ **v1.4.0** | `proto/dnst/go.mod` |
| `proto/ssh` | **NONE** (no root dep) | `proto/ssh/go.mod` |
| `drivers/aesgcm` | root @ **v1.4.0**, `proto/aesgcm` @ **v1.1.0** | `drivers/aesgcm/go.mod` |
| `drivers/dnst` | root @ **v1.4.0**, `proto/dnst` @ **v1.1.0** | `drivers/dnst/go.mod` |
| `drivers/dtls` | root @ **v1.4.0** (+ pion/dtls/v3) | `drivers/dtls/go.mod` |
| `drivers/dtlspsk` | root @ **v1.4.0** (+ pion/dtls/v3) | `drivers/dtlspsk/go.mod` |
| `drivers/ssh` | root @ **v1.4.0**, `proto/ssh` @ **v1.1.0** (+ x/crypto) | `drivers/ssh/go.mod` |
| `drivers/tls` | root @ **v1.4.0** | `drivers/tls/go.mod` |
| `drivers/tlspsk` | root @ **v1.4.0** (+ raff/tls-ext, raff/tls-psk) | `drivers/tlspsk/go.mod` |
| `drivers/utls` | root @ **v1.4.0** (+ refraction/utls) | `drivers/utls/go.mod` |
| `cli` | root @ **v1.4.0**, all 8 `drivers/*` @ **v1.1.1**, all 3 `proto/*` @ **v1.1.0** (indirect), `spf13/cobra` v1.10.2, `miekg/dns` v1.1.72 | `cli/go.mod` require |

`cli` is the only module that aggregates everything: it imports root, all 8
drivers directly, and the 3 proto modules transitively (`// indirect`). The
`cli/go.sum` pins confirm what an out-of-workspace build uses: drivers @ v1.1.1,
proto @ v1.1.0, root @ v1.4.0.

### Notable external dependencies (per module)

- root: `pion/transport/v3 v3.1.1`, `golang.org/x/net v0.52.0` (`go.mod`).
- `drivers/dtls` & `drivers/dtlspsk`: `pion/dtls/v3 v3.1.2`.
- `drivers/tlspsk`: `raff/tls-ext v1.0.0`, `raff/tls-psk v1.0.0`.
- `drivers/utls`: `refraction-networking/utls v1.8.2`.
- `proto/dnst` & `cli`: `miekg/dns v1.1.72`.
- `cli`: `spf13/cobra v1.10.2`.

---

## Independent versioning & the release-ordering hazard

### Tag conventions

Modules are versioned and tagged **independently** (verified via `git tag -l`):

- Root: bare `vX.Y.Z` — currently up to `v1.4.0`.
- Submodules: `<module-path>/vX.Y.Z`, e.g. `proto/aesgcm/v1.1.0`,
  `drivers/tls/v1.1.1`, `cli/v1.1.3`.

This is the standard Go multi-module monorepo scheme: the tag prefix (directory
path) selects which `go.mod` the version applies to. The CI release job fires
only on `cli/v*` tags (see CI/CD).

Current published heads (from tags): root `v1.4.0`; `proto/*` `v1.1.0`;
`drivers/*` `v1.1.1`; `cli` `v1.1.3`.

> AMBIGUITY: the `cli` module never self-versions (a module never requires
> itself), so the CLI's "version" lives only in the git tag and in the cobra
> `Version: "dev"` string (`Run` in `cli/internal/root.go`) — the built binary
> always reports `dev`, never the tag. Do not rely on `netx --version`.

### CHANGELOG

`CHANGELOG.md` (Keep-a-Changelog + SemVer) is multi-module aware: its header
documents the per-module tag namespaces and the bottom-up release wave (and links
back to this doc), and each dated section labels the wave it covers
(e.g. `## [v1.4.0 · cli/v1.1.0–v1.1.3 · drivers,proto v1.1.x]`). It is current
through **v1.4.0** plus an `[Unreleased]` section for the post-`cli/v1.1.3` branch
work (the `--eager`, redaction, IP_BOUND_IF, and DTLS-tuning changes). The
pre-split paths (`cmd/netx_lib`, `internal/cli`, `internal/tools/e2e`) appear only
inside the v1.2.0/v1.3.0 historical entries — and the v1.3.0 entry documents the
rename to today's `cli/internal/lib`, `cli/internal`, `cli/internal/e2e`. So those
old paths are historical record, not a stale map of the current tree.

### Required bump/release order

Because of the `go.work` masking (Change hazard #1), `task test`/`task build`
compile against the working tree, never the published pins. When a change crosses
a module boundary (a new/changed root API consumed downstream), modules must be
**bumped and released bottom-up** — each downstream `go.mod` must point at the
new upstream tag before it can itself be released:

```
1. root go-netx              -> tag vX.Y.Z (bare)
2. proto/aesgcm, proto/dnst  -> bump root require, tag proto/<name>/vA.B.C
   (proto/ssh only if it gains a root dep — currently none)
3. drivers/*                 -> bump root (and proto/* for aesgcm,dnst,ssh),
                                tag drivers/<name>/vA.B.C
4. cli                       -> bump root + all 8 drivers/* + 3 proto/* requires,
                                tag cli/vX.Y.Z (triggers the release workflow)
```

Skipping a level (releasing `cli` against an unreleased root) is impossible for
external `go get` but invisible locally thanks to `go.work`.

---

## Build & test orchestration (Taskfile.yml)

Orchestration is in `Taskfile.yml` (Task v3).

| Target | What it does |
|---|---|
| `default` | `task --list` |
| `deps` | installs `golangci-lint@latest`, `go mod download` |
| `lint` | `golangci-lint run` |
| `test` | iterates **every workspace module** via `go list -m -f '{{.Dir}}'` and runs `go test -v ./...` in each |
| `build` | cross-compiles all binaries + shared libs (matrix below) |
| `build:lib:darwin` | macOS/iOS static `c-archive` libs (needs Apple toolchains) |
| `test:e2e:tun` | full tunnel e2e harness; `lib=true` variant tests the FFI lib |

### Cross-compile matrix & required toolchains (`build`)

Binaries (`go build`, CGO off): linux amd64/arm64, windows amd64/arm64, macOS
amd64/arm64.

Shared libraries (`-buildmode=c-shared`, **`CGO_ENABLED=1`**) — each needs a
specific cross C compiler:

| Target | CC required |
|---|---|
| linux amd64 `.so` | `x86_64-linux-gnu-gcc` |
| linux arm64 `.so` | `aarch64-linux-gnu-gcc` |
| windows amd64 `.dll` | `x86_64-w64-mingw32-gcc` |
| windows arm64 `.dll` | `aarch64-w64-mingw32-clang` (llvm-mingw) |
| android amd64 `.so` | `$ANDROID_NDK_HOME/.../x86_64-linux-android26-clang` |
| android arm64 `.so` | `$ANDROID_NDK_HOME/.../aarch64-linux-android26-clang` |

`build:lib:darwin` produces `c-archive` `.a` files for macOS amd64/arm64 and iOS
device/simulator, driven by `xcrun`-discovered SDK paths and per-arch
`CFLAGS`/`LDFLAGS`. It is a separate task because it requires a macOS host (CI
runs it on `macos-latest`). Both `build` and `build:lib:darwin` use Task's
`sources`/`generates` for incremental rebuild caching.

> NOTE: there is no macOS shared library in `build` — only binaries. macOS/iOS
> `c-shared`/`c-archive` are exclusively in `build:lib:darwin`.

### e2e harness flow (`test:e2e:tun`)

The target is a single large bash script:

1. **Variant select**: the `lib` var defaults to `"false"`, `enum`-restricted to
   `"true"|"false"`.
   - `lib=false`: builds the cobra binary `go build -o .e2e/netx ./cli/cmd/netx`.
   - `lib=true`: builds the c-shared lib `libnetx.so` from `./cli/internal/lib`,
     then a thin runner `netx_lib` from `./cli/internal/e2e/lib` linked against
     it via `CGO_CFLAGS`/`CGO_LDFLAGS=-L.e2e -lnetx`. `$NETX` is set accordingly.
2. **Library path**: sets `DYLD_LIBRARY_PATH` (Darwin) or `LD_LIBRARY_PATH`
   (else) so the runner finds `libnetx.so` at runtime.
3. **Cert/key generation**: openssl RSA cert, PSK hex, AES hex, two ed25519 SSH
   keypairs, x25519 reality keys.
4. **Build helpers with `-tags e2e`**: `tcp_echo`, `udp_echo`, `tcp_client`,
   `udp_client`, `dns_resolver`.
5. **Start echo servers + mock DNS resolver.**
6. **Start server-side and client-side `netx tun` chains** — one pair per
   protocol, all backgrounded: tls, dtls, dtlspsk, **dtls-tuned** and
   **dtlspsk-tuned** (the `mtu`/`flightinterval`/`skipcookie`/`resume`
   fast-obfuscation params), aesgcm-tcp, aesgcm-udp, frame, tlspsk, ssh, utls,
   dnst, plus a **`--eager` dtls client** (`CDTLSE`) exercising the warm
   pre-dial path. REALITY server/client pairs are commented out.
7. **`sleep 2`, then run client probes** comparing echoed payload to input;
   tally pass/fail. Probes include `DTLS_TUNED`, `DTLSPSK_TUNED`, `DTLS_EAGER`.
8. **Cleanup** via `pkill netx / netx_lib / tcp_echo / udp_echo / dns_resolver`
   and `exit` non-zero if any failure (`[ "$fail" -eq 0 ]`).

### Hard-coded e2e port allocation

Ports are fixed, not dynamically allocated (the `TE`/`STLS`/`CTLS`/... shell vars
in the harness): echo `48080/48081`; server tunnels in `49000–49951`; client
tunnels in `50000–50050`. DNST uses `SDNST_AUTH=49950` (authoritative netx) and
`SDNST_RES=49951` (mock resolver). Any external process holding one of these
ports makes the run flaky/fail (see Change hazards).

CI runs **both** variants back-to-back: `task test:e2e:tun` then
`task test:e2e:tun lib=true` (`.github/workflows/lint_test_and_build.yml`).

---

## CLI architecture

### Entry point (`cli/cmd/netx/main.go`)

`main` is `os.Exit(internal.Run(signal.NotifyContext(Background, SIGINT,
SIGTERM)))` — the `(ctx, cancel)` from `NotifyContext` spread directly into
`internal.Run`'s first two params, so SIGINT/SIGTERM cancel the context and
trigger graceful shutdown in `runTun`. The file blank-imports all 8 drivers (see
Driver activation).

### Programmatic core (`cli/internal/root.go`)

`Run(ctx, cancel, opts ...Option) (exitCode int)` is the reusable engine shared
by the binary and the FFI lib. Config is a `cfg` struct (`args`, `out`, `err`)
defaulting to `os.Args[1:]`, `os.Stdout`, `os.Stderr`; options `WithArgs`,
`WithOut`, `WithErr` override it.

`Run` builds the root cobra command:
- `Use: "netx [command]"`, `Version: "dev"` (static), `SilenceUsage/Errors: true`.
- `PersistentPreRunE` parses `--log` and installs a `slog` text handler writing
  to `cfg.out`.
- Wires `SetArgs/SetOut/SetErr` from `cfg`.
- Overrides the help func to append `uriFormat` after default help.
- Persistent flag `--log` (default `info`).
- `cmd.AddCommand(tun(cancel))` — the only subcommand.
- Returns `1` on execute error, `0` otherwise.

`parseLogLevel` maps `debug/info/warn|warning/error` (case-insensitive, trimmed)
to `slog.Level`; empty == info; an unknown value returns a non-nil error (which
`PersistentPreRunE` propagates, failing the command), not a level.

### `tun` subcommand (`cli/internal/tun.go`)

`tun(cancel context.CancelFunc) *cobra.Command`:
- Defends against a nil `cancel` by substituting a no-op.
- Flags: `--from` and `--to` (both `<uri>`, both **required** via
  `MarkFlagRequired`), and `--eager` (`BoolVar`, default `false`).
- `RunE` pulls ctx from the command (falling back to `context.Background()`),
  calls `runTun(ctx, cancel, from, to, eager)`, and on error joins the error with
  `cmd.Help()`.

`runTun(ctx, cancel, from, to string, eager bool) error`:
1. Unmarshals `from` into `netx.ListenerURI` and `to` into `netx.DialerURI`
   (these drive the chain parser — see `docs/internals/pipeline.md`).
2. `fromURI.Listen(ctx)` opens the listener (closed on return).
3. Defines `dialPeer`, which dials `toURI` with
   `netx.WithDialConfig(net.Dialer{Control: dialControl})` — see the darwin
   `dialControl` note below.
4. **Eager warm pre-dial** (when `--eager`): stores a `deferredConn` in
   `warmPeer atomic.Pointer[deferredConn]` (`eager.go`) the moment the listener
   binds, so the obfuscation handshake to the upstream runs/completes *before*
   the first inbound packet. See "Eager pre-connect" below.
5. Creates `netx.TunMaster[struct{}]{}` (single anonymous route key) and
   registers one route. The handler first tries to claim the warm conn via
   `warmPeer.Swap(nil)`; on a miss (or in non-eager mode) it falls back to a lazy
   per-conn `dialPeer` (on failure: log `slog.Error("dial tun")`, close the
   inbound conn, reject the route). On the lazy path it logs `slog.Info("netx tun
   upstream dialed")`. `TunMaster.SetRoute` (override in `tun.go`) wraps the
   handler to spawn `Tun.Relay` per connection.
6. Serves in a goroutine; on a non-`ErrServerClosed` error it logs `slog.Error
   ("serve error")` and calls `cancel()` to bring the process down.
7. Emits the readiness log line (below), then blocks on `<-ctx.Done()`. On
   shutdown it **closes any unconsumed warm peer** via `warmPeer.Swap(nil)` (leak
   avoidance for an abandoned speculative pre-connect), then `tm.Shutdown` with a
   **3s** timeout (`context.WithTimeout(Background, 3*time.Second)` in `runTun`).

#### Readiness signal (embedder contract)

`runTun` logs `slog.Info("netx tun started", "listen", ..., "from", ..., "to",
...)` once the listener is bound and serving. **Embedders latch on this exact
message** — the c-shared lib forwards info-level slog to a C callback, and a host
(e.g. a macOS NEPacketTunnelProvider) waits for this line to know the relay is
up. There is **no `NETX_READY` token in the tree**; this log line *is* the
contract. Do not rename it. (On the lazy path, `slog.Info("netx tun upstream
dialed")` and, in eager mode, `slog.Info("netx tun upstream pre-warmed")` give
per-session lifecycle signals between "started" and "tunnel closed".)

#### Secret redaction in logs

The `"from"`/`"to"` fields above are rendered via `URI.Redacted()` (`uri.go`),
not `String()`. `Redacted` -> `Scheme.Redacted` (`scheme.go`) ->
`Wrappers.Redacted`/`Wrapper.Redacted` (`wrap.go`), which masks the value of each
param a driver declares in `Wrapper.SecretParams` (keys, passphrases) with a
protocol-standard fingerprint — keeping the protocol chain visible without
leaking key material. **Security invariant: never log a raw URI via `String()`;
use `Redacted()`** for any user-visible / FFI-forwarded stream. (`String()`
stays round-trippable through `UnmarshalText`; `Redacted()` is not.)

#### darwin IP_BOUND_IF dial control

`dialControl` is a no-op everywhere except darwin (`tun_other.go` vs
`tun_darwin.go`, build-tag split). On macOS netx runs **inside** the
NEPacketTunnelProvider, whose sockets are scoped to the tunnel it serves; without
an explicit bind, netx's outbound (e.g. UDP) socket loops back into the tunnel
and the upstream handshake never completes (rx=0). `dialControl` binds the socket
to the primary physical interface via `IP_BOUND_IF`/`IPV6_BOUND_IF`
(`primaryPhysicalIfaceIndex` picks the lowest-index, up, non-loopback `en*` iface
with a usable IPv4). It is **fail-open**: if no iface is found or the bind fails,
the dial proceeds unbound rather than erroring. It is threaded into the upstream
dial via `netx.WithDialConfig` in `dialPeer` — an engineer rewiring the dial must
preserve that wiring or the macOS tunnel use case breaks.

#### Eager pre-connect (`cli/internal/eager.go`)

`--eager` speculatively pre-dials the upstream at relay start so the (often slow)
obfuscation handshake is warm before any inbound traffic. Building blocks:

- `deferredConn` — a `net.Conn` whose `Read`/`Write` block (via `await`) until
  the background dial settles, then proxy to the real conn or return the dial
  error. The first inbound datagram is naturally held by the blocked `Write`
  until the upstream is ready: **`io.CopyBuffer` reads the next datagram only
  after the prior `Write` returns**, so no separate buffer is needed (extra
  datagrams queue in the inbound conn's read buffer). Preserve this back-pressure
  invariant if you refactor `deferredConn`.
- `newDeferredConn` — starts dialing immediately with **bounded-backoff retry**
  (5 attempts, backoff starting `200ms`, doubling, capped at `2s`; no sleep after
  the final attempt) so a transient failure on a lossy link doesn't abandon the
  warm upstream. Logs `netx tun upstream pre-warmed` on success or `... pre-dial
  failed after retries` on failure (`net.ErrClosed` from teardown is silent).
- `stubAddr` — placeholder `net.Addr` so `RemoteAddr`/`LocalAddr` return
  synchronously before the dial settles (`TunMaster.SetRoute` dereferences
  `Peer.RemoteAddr()` immediately).
- Lifecycle: the warm conn is consumed by the **first** inbound connection
  (`warmPeer.Swap(nil)`); later connections fall back to a lazy dial; an
  unconsumed warm conn is **closed on `ctx.Done()`** by `runTun`. `Close` aborts
  an in-flight dial via the stored `cancel` and drops a conn that wins a race
  against `Close`.

Help text: `tunExample` (`tun.go`, attached as the `tun` command's `Example`) and
the large `uriFormat` string (`cli/internal/help.go`, appended by the root help
func in `Run`) document the chain URI grammar
(`<transport>+<layer>{params}+...://<addr>`), transports (tcp/udp/icmp), and every
layer's params. The cert/key-as-hex requirement and SPKI-pinning behavior are
documented in `uriFormat`.

---

## c-shared library / FFI (`cli/internal/lib/main.go`)

`package main` with a cgo preamble defining a C callback typedef and a nil-safe
invoker:

```c
typedef void (*netx_callback_t)(const char* msg);
static inline void netx_call_callback(netx_callback_t cb, const char* msg) {
    if (cb != NULL) cb(msg);
}
```

`func main() {}` is empty — required for `-buildmode=c-shared`/`c-archive`. It
blank-imports all 8 drivers (must stay in sync with the binary — see Driver
activation).

### Exported symbols

- `//export Netx` — `Netx(id *C.char, argc C.int, argv **C.char, outCB, errCB
  C.netx_callback_t) C.int`.
- `//export NetxInterrupt` — `NetxInterrupt(id *C.char) C.int`.

### `Netx` flow

1. **argv parsing**: if `argc>0 && argv!=nil`, reinterprets `argv` as
   `(*[1<<28]*C.char)(...)[:length:length]` (a fixed huge upper bound sliced to
   `length = int(argc)`) and converts each non-nil entry with `C.GoString`,
   stopping at the first nil pointer. Safe only if the caller passes a correct
   `argc` (see hazards).
2. **id -> cancelable context**: `eID = C.GoString(id)`; `ctx, cancel :=
   context.WithCancel(Background())`; `registerActiveCall(eID, cancel)`.
3. **cleanup**: deferred `removeActiveCall(eID, entry)` then `cancel()`.
4. **run**: calls `internal.Run(ctx, cancel, WithArgs(parsedArgs),
   WithOut(&outWriter{outCB}), WithErr(&errWriter{errCB}))` and returns the exit
   code as `C.int`.

### `NetxInterrupt` flow

nil id -> return 0; otherwise `interruptActiveCall(C.GoString(id))` -> return 1 if
a call was found and cancelled, else 0.

### The active-call map

```go
type activeEntry struct { cancel context.CancelFunc }
var (
    activeMu    sync.RWMutex
    activeCalls = make(map[string]*activeEntry)
)
```

- `registerActiveCall(id, cancel)`: under `activeMu.Lock()`, stores a new entry;
  **if a previous entry existed for the same id, it cancels the previous one
  after unlocking** — so calling `Netx` twice with the same id pre-empts the
  earlier call. Returns the new entry (used as an identity token).
- `removeActiveCall(id, entry)`: deletes the map slot **only if it still points
  at this exact entry** (`activeCalls[id] == entry`). This guards against a newer
  call's entry being clobbered by an older call's deferred cleanup.
- `interruptActiveCall(id)`: under lock, finds+deletes the entry, then (after
  unlock) cancels it; returns whether one was found.
- The map is declared `sync.RWMutex` but **every operation uses `Lock()`** — the
  RW distinction is unused, so don't assume read-parallelism exists.

### Callback writers & C memory

`outWriter`/`errWriter` each hold a `C.netx_callback_t`. Their `Write(p []byte)`:
- if `len(p)!=0 && cb!=nil`: `msg := C.CString(string(p))`, `defer
  C.free(unsafe.Pointer(msg))`, then `C.netx_call_callback(cb, msg)`.
- **always** also writes to the real `os.Stdout`/`os.Stderr`.

Memory contract: each callback message is a freshly `C.CString`-allocated
NUL-terminated buffer freed immediately after the synchronous callback returns.
**The C callback MUST treat `msg` as valid only for the duration of the call** —
it is freed on return, so the consumer must copy if it needs to retain it.

### Concurrency

Multiple concurrent `Netx` calls with **distinct ids** run independently; same
id => the newer pre-empts the older (`registerActiveCall`). Callback invocation
happens on the goroutine doing the `Write`; the C side must be thread-safe if it
shares state across calls.

### e2e runner (`cli/internal/e2e/lib/main.go`)

A thin host that links the shared lib. It declares the two `extern` C prototypes,
uses a fixed handle `"e2e"`, spins a goroutine that calls `NetxInterrupt` on
SIGINT/SIGTERM, marshals `os.Args[1:]` into a C `argv` (`C.CString` each, freed
via defer), and calls `C.Netx(...)` with nil callbacks. This is what
`test:e2e:tun lib=true` builds and runs as `$NETX`.

---

## Driver activation (blank-import requirement)

Drivers self-register in `init()`: each calls `netx.Register("<name>", ...)`.
`netx.Register` (`driver.go`) stores the driver under its name in a global map
and **panics on duplicate registration** (and on a nil driver).

Registered names by source (the 8 driver modules), `init` in each file:

| Driver module | Registered scheme | Source |
|---|---|---|
| `drivers/aesgcm` | `aesgcm` | `init` in `drivers/aesgcm/aesgcm.go` |
| `drivers/dnst` | `dnst` | `init` in `drivers/dnst/dnst.go` |
| `drivers/dtls` | `dtls` | `init` in `drivers/dtls/dtls.go` |
| `drivers/dtlspsk` | `dtlspsk` | `init` in `drivers/dtlspsk/dtlspsk.go` |
| `drivers/ssh` | `ssh` | `init` in `drivers/ssh/ssh.go` |
| `drivers/tls` | `tls` | `init` in `drivers/tls/tls.go` |
| `drivers/tlspsk` | `tlspsk` | `init` in `drivers/tlspsk/tlspsk.go` |
| `drivers/utls` | `utls` | `init` in `drivers/utls/utls.go` |

Core layers/transports register from the **root** module's own `init()`s and so
are always available wherever root is imported: `buf` (`buffered_conn.go`),
`demux` (`demux.go`), `frame` (`frame_conn.go`), `mux` (`mux.go`), `poll`
(`poll_conn.go`), `split` (`split_conn.go`); transports `tcp`/`udp`/`icmp` are
handled in `transport.go`.

**Because registration is `init()`-driven, a driver only activates if its package
is imported.** The cli pulls them in via blank imports in **two** files that must
stay in sync:

- `cli/cmd/netx/main.go` (the binary)
- `cli/internal/lib/main.go` (the FFI lib)

### What breaks if you forget one

- Blank-import a new driver in **only** `main.go`: the **binary** supports it but
  the **shared library** (and `test:e2e:tun lib=true`) does NOT —
  `fromURI/toURI.UnmarshalText` or `Listen/Dial` will fail at runtime with
  `uri: unknown driver "<name>"` (`GetDriver` in `driver.go`), not at compile
  time.
- Forget both: the driver is dead weight; URIs referencing it fail at runtime.
- The failure is **silent at build time** because the new driver module still
  compiles standalone; only a URI exercising the scheme reveals it. There is no
  test asserting the two import lists match. (`cli/internal/tun_test.go` — the
  only CLI-internal test — blank-imports only `drivers/aesgcm` and does NOT guard
  this invariant.)

---

## CI/CD (.github/workflows)

### `lint_test_and_build.yml`

Triggers: PRs to `main`, pushes to `main`, **tags matching
`cli/v[0-9]+.[0-9]+.[0-9]+`**, and `workflow_dispatch`. Top-level perms
`contents: read`.

Jobs:
1. **`lint-test-and-build`** (`ubuntu-latest`):
   - Installs cross C toolchains: `gcc-x86-64-linux-gnu`, `gcc-aarch64-linux-gnu`,
     `gcc-mingw-w64-x86-64`.
   - Installs **llvm-mingw** (pinned `LLVM_MINGW_VERSION=20260421`) for windows
     arm64 and symlinks `aarch64-w64-mingw32-*` into `/usr/local/bin`.
   - `setup-go` from `go.mod`, `setup-task` v3.
   - Runs `task deps`, `task lint`, `task test`, `task test:e2e:tun`,
     `task test:e2e:tun lib=true`, `task build`.
   - Uploads `build/` as artifact named `${{ github.sha }}`
     (`if-no-files-found: error`, 7-day retention).
2. **`build-lib-darwin`** (`macos-latest`): `task deps`, `task build:lib:darwin`;
   uploads `build/` as `darwin-${{ github.sha }}`. It does NOT install Android NDK
   toolchains, so the Android `.so` targets only build in the ubuntu job (and only
   if `$ANDROID_NDK_HOME` is set — see hazards).
3. **`release`** (`ubuntu-latest`): gated by `if: github.event_name == 'push' &&
   startsWith(github.ref, 'refs/tags/cli/')`, `needs: [lint-test-and-build,
   build-lib-darwin]`, perms `contents: write`. Downloads both artifacts into
   `build/` and publishes a GitHub Release via `softprops/action-gh-release@v2`
   with `files: build/**`.

> Release implication: **only `cli/v*` tags publish artifacts.** Tagging root,
> proto, or drivers does NOT run the release job (it's not in the trigger filter,
> and the release `if` requires `refs/tags/cli/`). The build matrix is keyed to
> the `cli` module's outputs.

### `codeql.yml`

CodeQL Advanced. Triggers on push/PR to `main` and a weekly cron `31 0 * * 0`.
Matrix analyzes `actions` (build-mode none) and `go` (build-mode **autobuild**).
`go` runs on ubuntu; perms include `security-events: write`. Autobuild relies on
`go.work` to compile the whole workspace.

---

## Change hazards (MOST IMPORTANT)

Ordered roughly by likelihood × blast radius.

1. **Release-ordering / `go.work` masking (highest).** Editing a shared root
   symbol (`Driver`/`Wrapper` in `driver.go`/`wrap.go`, `Tun`/`TunMaster` in
   `tun.go`, the URI/Scheme/Transport types) compiles and tests green locally
   because `go.work` uses on-disk source for every sibling — **even though each
   `go.mod` still pins root `v1.4.0`** (e.g. `drivers/tls/go.mod`). So
   `task test`/`task build`/`task test:e2e:tun` never exercise the published
   pins; external `go get` consumers of `drivers/*@v1.1.1` get root `v1.4.0` and
   break if your unreleased change broke the root API. Mitigation: after any
   cross-module API change, bump+tag bottom-up (root -> `proto/aesgcm`,
   `proto/dnst` -> all `drivers/*` -> `cli`) and verify each `go.mod` `require`
   points at the new tag before tagging the next level. `proto/ssh` has no root
   dep so it is exempt unless that changes.

2. **Forgetting a blank import in ONE of the two mains.** A new driver must be
   blank-imported in BOTH `cli/cmd/netx/main.go` AND `cli/internal/lib/main.go`.
   Miss the lib file and the FFI build / `test:e2e:tun lib=true` lacks the driver;
   miss `main.go` and the binary lacks it. Failure is **runtime-only**
   (`uri: unknown driver`, `GetDriver` in `driver.go`), never a compile error,
   and nothing tests that the two lists agree. Also add the new driver to
   `cli/go.mod` `require` and re-tidy.

3. **FFI callback memory lifetime.** `outWriter`/`errWriter` free the `C.CString`
   immediately after the synchronous callback returns. A C consumer that stores
   the pointer instead of copying gets a use-after-free. Do not make the Go side
   async or hold the pointer past the call.

4. **FFI concurrency / id collisions.** `activeCalls` is keyed by the
   caller-supplied id. A second `Netx` with the same id **cancels the first**
   (`registerActiveCall`). Distinct concurrent calls need distinct ids.
   `removeActiveCall` only deletes if the entry identity still matches, so
   deferred cleanup of an old call won't evict a newer one — preserve this
   identity check if you refactor. (The `RWMutex` is only ever `Lock()`ed — see
   the active-call map subsection.)

5. **argv parsing assumes a correct `argc`.** `Netx` slices `argv` to
   `length = int(argc)`. A caller passing an `argc` larger than the real array
   (with no nil terminator before the end) reads out of bounds. The nil-pointer
   early-break only helps if the caller nil-terminates. Document the contract for
   FFI consumers.

6. **e2e hard-coded ports.** All e2e ports are fixed shell vars in the harness
   (`48080/48081`, `49xxx`, `50xxx`). Any local/CI process on those ports makes
   `test:e2e:tun` flaky or fail; there is no port-retry. Adding a new protocol
   pair means picking unused ports in these ranges by hand. The harness also
   relies on a flat `sleep 2` for readiness, so slow CI can race.

7. **Toolchain drift in cross builds.** `task build` hard-codes specific CCs and
   CI installs exact packages plus a pinned `llvm-mingw`
   (`LLVM_MINGW_VERSION=20260421`). Android targets need `$ANDROID_NDK_HOME` to be
   set — it is **not** set anywhere in CI, so `task build` would fail the Android
   steps if that env var is absent (verify before relying on Android artifacts).
   `build:lib:darwin` needs a macOS host + Xcode SDKs. Bumping the llvm-mingw
   version or the apt package names must keep the `aarch64-w64-mingw32-clang`
   symlink working.

8. **Eager / darwin dial wiring.** `--eager` pre-dials the upstream and parks it
   in `warmPeer` (`tun.go`); the unconsumed warm conn is closed on `ctx.Done()` —
   refactoring shutdown must keep that `warmPeer.Swap(nil)` close or it leaks a
   dialed conn. `deferredConn` relies on `io.CopyBuffer` back-pressure (Write
   blocks until the dial settles) instead of an explicit buffer — don't break
   that contract. The upstream dial is wired with `netx.WithDialConfig(net.Dialer
   {Control: dialControl})`; on darwin `dialControl` (`tun_darwin.go`) binds the
   socket to the physical iface (`IP_BOUND_IF`) and is **fail-open** — drop that
   wiring and the macOS NEPacketTunnelProvider use case silently regresses
   (rx=0).

9. **Don't log raw URIs.** Info logs (and the FFI callback that forwards them)
   render URIs via `URI.Redacted()`, which masks `SecretParams` values. Switching
   a log call to `String()`, or adding a new URI-bearing log without `Redacted()`,
   leaks key material. Keep secret params declared in each driver's
   `Wrapper.SecretParams`.

10. **Stale/misleading metadata.** `netx --version` always prints `dev`
    (`Run` in `cli/internal/root.go`), never the `cli/vX.Y.Z` tag — don't use it
    to identify a release. `CHANGELOG.md` is current through v1.4.0 (+ an
    `[Unreleased]` section) and is multi-module aware, but only the pre-split
    v1.2.0/v1.3.0 entries name the old `cmd/netx_lib`/`internal/cli` paths (kept
    as historical record) — keep it updated per release if you depend on it. The
    REALITY e2e server/client pairs are commented out in the harness, so REALITY
    is unverified end-to-end.

11. **Release trigger scope.** Only `cli/v*` tags publish artifacts; the artifact
    set is whatever `task build` + `task build:lib:darwin` produce. If you add a
    new build target to the Taskfile, add it to that job's outputs or it won't
    ship. The `release` job `needs` both build jobs, so a macOS build failure
    blocks the release even though the binaries are fine.

---

## Related docs

- [`pipeline.md`](pipeline.md) — the wrapper/scheme chain pipeline that
  `ListenerURI.Listen` / `DialerURI.Dial` drive (the engine behind `--from`/
  `--to`).
- [`server-tun.md`](server-tun.md) — `Server[ID]` / `TunMaster` / `Tun.Relay`
  internals used by `runTun`.
- [`drivers-proto.md`](drivers-proto.md) — per-driver and per-proto wrapper
  details, including each driver's `SecretParams`.
