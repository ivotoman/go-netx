# Workspace Modules, CLI & FFI

Internals reference for the multi-module `go.work` workspace, the `cli/` cobra
binary, and the c-shared FFI library. Every claim below is grounded in
`file:line`. Where the repo is internally inconsistent (e.g. the CHANGELOG vs.
the actual layout), it is flagged as AMBIGUITY.

The single most important thing in this doc is **Change hazards** (last section,
before Related docs). Read that before touching any shared root type, adding a
driver, or editing the FFI library.

---

## Module map & dependency graph

The workspace is declared in `go.work:3-17` and contains **13 modules**:

```
go.work (go 1.25.7)  — go.work:1
use (
  .                    -> github.com/pedramktb/go-netx              (root lib)
  ./cli                -> github.com/pedramktb/go-netx/cli          (binary + FFI)
  ./proto/aesgcm       -> .../proto/aesgcm
  ./proto/dnst         -> .../proto/dnst
  ./proto/ssh          -> .../proto/ssh
  ./drivers/aesgcm     -> .../drivers/aesgcm
  ./drivers/dnst       -> .../drivers/dnst
  ./drivers/dtls       -> .../drivers/dtls
  ./drivers/dtlspsk    -> .../drivers/dtlspsk
  ./drivers/ssh        -> .../drivers/ssh
  ./drivers/tls        -> .../drivers/tls
  ./drivers/tlspsk     -> .../drivers/tlspsk
  ./drivers/utls       -> .../drivers/utls
)
```

`go.work` lists every local module, so **within this repo all cross-module
imports resolve to the on-disk source, NOT the versions pinned in each
`go.mod`**. That masking is the root cause of the release-ordering hazard (next
section).

### Internal dependency edges (who requires which sibling @ which version)

Read from each module's `go.mod` `require` stanza:

| Module | Requires sibling(s) @ version | Source |
|---|---|---|
| root `go-netx` | (none — only external) | `go.mod:5-10` |
| `proto/aesgcm` | root `go-netx` @ **v1.4.0** | `proto/aesgcm/go.mod:5` |
| `proto/dnst` | root `go-netx` @ **v1.4.0** | `proto/dnst/go.mod:7` |
| `proto/ssh` | **NONE** (no root dep) | `proto/ssh/go.mod:5-7` |
| `drivers/aesgcm` | root @ **v1.4.0**, `proto/aesgcm` @ **v1.1.0** | `drivers/aesgcm/go.mod:6-7` |
| `drivers/dnst` | root @ **v1.4.0**, `proto/dnst` @ **v1.1.0** | `drivers/dnst/go.mod:6-7` |
| `drivers/dtls` | root @ **v1.4.0** (+ pion/dtls/v3) | `drivers/dtls/go.mod:6-7` |
| `drivers/dtlspsk` | root @ **v1.4.0** (+ pion/dtls/v3) | `drivers/dtlspsk/go.mod:6-7` |
| `drivers/ssh` | root @ **v1.4.0**, `proto/ssh` @ **v1.1.0** (+ x/crypto) | `drivers/ssh/go.mod:6-8` |
| `drivers/tls` | root @ **v1.4.0** | `drivers/tls/go.mod:5` |
| `drivers/tlspsk` | root @ **v1.4.0** (+ raff/tls-ext, raff/tls-psk) | `drivers/tlspsk/go.mod:6-8` |
| `drivers/utls` | root @ **v1.4.0** (+ refraction/utls) | `drivers/utls/go.mod:6-7` |
| `cli` | root @ **v1.4.0**, all 8 `drivers/*` @ **v1.1.1**, all 3 `proto/*` @ **v1.1.0** (indirect), `spf13/cobra` v1.10.2, `miekg/dns` v1.1.72 | `cli/go.mod:5-39` |

Layered view (arrows = "requires"):

```
                       root go-netx (v1.4.0)
                      /     |       |       \
        proto/aesgcm  proto/dnst   |   (dtls,dtlspsk,tls,
            |             |        |    tlspsk,utls require
            v             v        |    root directly, no proto)
   drivers/aesgcm   drivers/dnst   |
                                   |
   drivers/ssh -> proto/ssh (proto/ssh has NO root dep)
                                   |
                       all 8 drivers/* + 3 proto/* (indirect)
                                   |
                                   v
                                  cli  (cobra binary + c-shared FFI lib)
```

Key structural facts:

- **`proto/ssh` is the odd one out**: it requires no sibling at all
  (`proto/ssh/go.mod:5-7` — only `golang.org/x/crypto` + `x/sys`). The other two
  proto modules (`proto/aesgcm`, `proto/dnst`) DO require root. So a breaking
  root change does NOT force a `proto/ssh` bump, but does force `proto/aesgcm`
  and `proto/dnst` bumps.
- **`cli` is the only module that aggregates everything.** It imports root, all
  8 drivers directly (`cli/go.mod:7-15`), and the 3 proto modules transitively
  (`cli/go.mod:23-25`, marked `// indirect`).
- The `cli` go.sum confirms the pins actually used when built outside the
  workspace: drivers @ v1.1.1, proto @ v1.1.0, root @ v1.4.0
  (verified in `cli/go.sum`).

### Notable external dependencies (per module)

- root: `pion/transport/v3 v3.1.1`, `golang.org/x/net v0.52.0` (`go.mod:6-7`).
- `drivers/dtls` & `drivers/dtlspsk`: `pion/dtls/v3 v3.1.2`
  (`drivers/dtls/go.mod:7`, `drivers/dtlspsk/go.mod:7`).
- `drivers/tlspsk`: `raff/tls-ext v1.0.0`, `raff/tls-psk v1.0.0`
  (`drivers/tlspsk/go.mod:7-8`).
- `drivers/utls`: `refraction-networking/utls v1.8.2` (`drivers/utls/go.mod:7`).
- `proto/dnst` & `cli`: `miekg/dns v1.1.72` (`proto/dnst/go.mod:6`,
  `cli/go.mod:6`).
- `cli`: `spf13/cobra v1.10.2` (`cli/go.mod:16`).

---

## Independent versioning & the release-ordering hazard

### Tag conventions

Modules are versioned and tagged **independently**. Verified tag prefixes
(`git tag -l`):

- Root: bare `vX.Y.Z` — currently up to `v1.4.0`.
- Submodules: `<module-path>/vX.Y.Z`, e.g. `proto/aesgcm/v1.1.0`,
  `drivers/tls/v1.1.1`, `cli/v1.1.3`.

This is the standard Go multi-module monorepo tagging scheme: the tag prefix
(directory path) selects which `go.mod` the version applies to. The CI release
job fires specifically on `cli/v*` tags (see CI/CD section).

Current published heads (from tags): root `v1.4.0`; `proto/*` `v1.1.0`;
`drivers/*` `v1.1.1`; `cli` `v1.1.3`.

> AMBIGUITY: the prompt states CLI releases are tagged `cli/vX.Y.Z` and the repo
> confirms `cli/v1.0.0..v1.1.3`. The `cli/go.mod` does not self-version (a module
> never requires itself), so the CLI's "version" lives only in the git tag and in
> the cobra `Version: "dev"` string (`cli/internal/root.go:56`) — the built
> binary always reports `dev`, never the tag. Do not rely on `netx --version`
> reflecting the release.

### CHANGELOG conventions

`CHANGELOG.md:5-6` declares Keep-a-Changelog + SemVer. Entries are grouped under
`## [vX.Y.Z] - date` with `Added/Changed/Fixed` subsections and PR references
(`CHANGELOG.md:10-45`). The CHANGELOG tracks the **root** line (latest entry is
`v1.2.0`, `CHANGELOG.md:10`) and is **stale**: it stops at v1.2.0 while root is
already tagged v1.4.0, and it does NOT track per-submodule versions.

> AMBIGUITY: the CHANGELOG describes paths that no longer exist — `cmd/netx_lib`,
> `internal/cli`, `internal/tools/e2e/lib` (`CHANGELOG.md:14,17,22`). The actual
> layout is `cli/internal/lib`, `cli/internal`, `cli/internal/e2e/lib`. Treat the
> CHANGELOG as historical narrative, not a map of the current tree.

### The masking mechanism (why local tests lie)

Because `go.work` (`go.work:3-17`) replaces all sibling imports with on-disk
source, **`task test`, `task build`, and `task test:e2e:tun` all compile against
the current working tree, never the published versions pinned in `go.mod`.**
Consequences:

- You can edit a shared root type (e.g. `netx.Wrapper`, `netx.Driver` in
  `driver.go:8`, or `TunMaster`/`Tun` in `tun.go`) and every local build/test
  still passes, because the drivers compile against your edited root — even
  though their `go.mod` still pins root `v1.4.0`.
- External consumers `go get`-ing `drivers/tls@v1.1.1` get root `v1.4.0`. If your
  unreleased change to root broke the API, those consumers break, but your CI
  (which uses the workspace) stayed green.

### Required bump/release order

When a change crosses a module boundary (e.g. a new/changed root API consumed by
proto/drivers/cli), modules must be **bumped and released bottom-up**, because
each downstream module's `go.mod` must point at the new upstream tag before it
can itself be released:

```
1. root go-netx           -> tag vX.Y.Z (bare)
2. proto/aesgcm, proto/dnst -> bump root require to vX.Y.Z, tag proto/<name>/vA.B.C
   (proto/ssh only if it gained a root dep — currently it has none)
3. drivers/*              -> bump root (and proto/* for aesgcm,dnst,ssh) requires,
                             tag drivers/<name>/vA.B.C
4. cli                    -> bump root + all 8 drivers/* + 3 proto/* requires,
                             tag cli/vX.Y.Z (this triggers the release workflow)
```

Skipping a level (e.g. releasing `cli` against an unreleased root) is impossible
for external `go get` but invisible locally thanks to `go.work`.

---

## Build & test orchestration (Taskfile.yml)

Orchestration is in `Taskfile.yml` (Taskfile v3, `Taskfile.yml:1`).

| Target | What it does | Source |
|---|---|---|
| `default` | `task --list` | `Taskfile.yml:4-6` |
| `deps` | installs `golangci-lint@latest`, `go mod download` | `Taskfile.yml:8-12` |
| `lint` | `golangci-lint run` | `Taskfile.yml:14-17` |
| `test` | iterates **every workspace module** via `go list -m -f '{{.Dir}}'` and runs `go test -v ./...` in each | `Taskfile.yml:19-27` |
| `build` | cross-compiles all binaries + shared libs (matrix below) | `Taskfile.yml:29-68` |
| `build:lib:darwin` | macOS/iOS static `c-archive` libs (needs Apple toolchains) | `Taskfile.yml:70-99` |
| `test:e2e:tun` | full tunnel e2e harness; `lib=true` variant tests the FFI lib | `Taskfile.yml:101-228` |

### Cross-compile matrix & required toolchains (`Taskfile.yml:29-68`)

Binaries (`go build`, CGO off): linux amd64/arm64, windows amd64/arm64, macOS
amd64/arm64 (`Taskfile.yml:34-35,40-41,46-47`).

Shared libraries (`-buildmode=c-shared`, **`CGO_ENABLED=1`**) — each needs a
specific cross C compiler:

| Target | CC required | Source |
|---|---|---|
| linux amd64 `.so` | `x86_64-linux-gnu-gcc` | `Taskfile.yml:37` |
| linux arm64 `.so` | `aarch64-linux-gnu-gcc` | `Taskfile.yml:38` |
| windows amd64 `.dll` | `x86_64-w64-mingw32-gcc` | `Taskfile.yml:43` |
| windows arm64 `.dll` | `aarch64-w64-mingw32-clang` (llvm-mingw) | `Taskfile.yml:44` |
| android amd64 `.so` | `$ANDROID_NDK_HOME/.../x86_64-linux-android26-clang` | `Taskfile.yml:49` |
| android arm64 `.so` | `$ANDROID_NDK_HOME/.../aarch64-linux-android26-clang` | `Taskfile.yml:50` |

`build:lib:darwin` produces `c-archive` `.a` files for macOS amd64/arm64 and iOS
device/simulator, driven by `xcrun`-discovered SDK paths and per-arch
`CFLAGS`/`LDFLAGS` (`Taskfile.yml:75-89`). It is a separate task because it
requires a macOS host (run on `macos-latest` in CI).

`build` uses Task's `sources`/`generates` (`Taskfile.yml:51-68`) for incremental
rebuild caching; `build:lib:darwin` similarly (`Taskfile.yml:90-99`).

> NOTE: there is no per-`GOOS` macOS shared library in `build` — only binaries.
> macOS/iOS `c-shared`/`c-archive` are exclusively in `build:lib:darwin`.

### e2e harness flow (`Taskfile.yml:101-228`)

The `test:e2e:tun` target is a single large bash script:

1. **Variant select** (`Taskfile.yml:103-126`): the `lib` var defaults to
   `"false"` and is `enum`-restricted to `"true"|"false"`
   (`Taskfile.yml:104-108`). 
   - `lib=false`: builds the cobra binary `go build -o .e2e/netx ./cli/cmd/netx`
     (`Taskfile.yml:123-124`).
   - `lib=true`: builds the c-shared lib `libnetx.so` from `./cli/internal/lib`,
     then builds a thin runner `netx_lib` from `./cli/internal/e2e/lib` linked
     against it via `CGO_CFLAGS`/`CGO_LDFLAGS=-L.e2e -lnetx`
     (`Taskfile.yml:116-121`). `$NETX` is set accordingly (`Taskfile.yml:139`).
2. **Library path** (`Taskfile.yml:130-137`): sets `DYLD_LIBRARY_PATH` (Darwin)
   or `LD_LIBRARY_PATH` (else) so the runner finds `libnetx.so` at runtime.
3. **Cert/key generation** (`Taskfile.yml:141-150`): openssl RSA cert, PSK hex,
   AES hex, two ed25519 SSH keypairs, x25519 reality keys.
4. **Build helpers with `-tags e2e`** (`Taskfile.yml:152-157`):
   `tcp_echo`, `udp_echo`, `tcp_client`, `udp_client`, `dns_resolver`.
5. **Start echo servers + mock DNS resolver** (`Taskfile.yml:166-171`).
6. **Start server-side and client-side `netx tun` chains** — one pair per
   protocol (tls/dtls/dtlspsk/aesgcm-tcp/aesgcm-udp/frame/tlspsk/ssh/utls/dnst),
   all backgrounded (`Taskfile.yml:173-197`). REALITY pairs are commented out
   (`Taskfile.yml:184,197,216`).
7. **`sleep 2`, then run client probes** comparing echoed payload to input;
   tally pass/fail (`Taskfile.yml:199-217`).
8. **Cleanup** via `pkill netx / netx_lib / tcp_echo / udp_echo / dns_resolver`
   and `exit` non-zero if any failure (`Taskfile.yml:219-227`).

### Hard-coded e2e port allocation (`Taskfile.yml:160-164`)

Ports are fixed, not dynamically allocated: echo `48080/48081`; server tunnels in
`49000–49951`; client tunnels in `50000–50050`. DNST uses `SDNST_AUTH=49950`
(authoritative netx) and `SDNST_RES=49951` (mock resolver). Any external process
holding one of these ports makes the run flaky/fail (see Change hazards).

CI runs **both** variants back-to-back: `task test:e2e:tun` then
`task test:e2e:tun lib=true` (`.github/workflows/lint_test_and_build.yml:44-45`).

---

## CLI architecture

### Entry point (`cli/cmd/netx/main.go`)

```go
func main() {
    os.Exit(internal.Run(signal.NotifyContext(context.Background(),
        syscall.SIGINT, syscall.SIGTERM)))
}
```
`cli/cmd/netx/main.go:20-22`. `signal.NotifyContext` returns `(ctx, cancel)`
which spread directly into `internal.Run(ctx, cancel, ...)` (the first two
positional params). SIGINT/SIGTERM cancel the context, which triggers graceful
shutdown in `runTun`. The file blank-imports all 8 drivers
(`cli/cmd/netx/main.go:10-17`) — see Driver activation.

### Programmatic core (`cli/internal/root.go`)

`Run(ctx, cancel, opts ...Option) (exitCode int)` (`cli/internal/root.go:39`) is
the reusable engine shared by the binary and the FFI lib. Config is a `cfg`
struct (`cli/internal/root.go:14-18`) with `args`, `out`, `err`, defaulting to
`os.Args[1:]`, `os.Stdout`, `os.Stderr` (`cli/internal/root.go:40-44`). Options:
`WithArgs`, `WithOut`, `WithErr` (`cli/internal/root.go:22-37`).

It builds the root cobra command (`cli/internal/root.go:52-67`):
- `Use: "netx [command]"`, `Version: "dev"` (static), `SilenceUsage/Errors:
  true`.
- `PersistentPreRunE` parses `--log` and installs a `slog` text handler writing
  to `cfg.out` (`cli/internal/root.go:59-66`).
- Wires `SetArgs/SetOut/SetErr` from `cfg` (`cli/internal/root.go:69-71`).
- Overrides the help func to append `uriFormat` after default help
  (`cli/internal/root.go:73-78`).
- Persistent flag `--log` (default `info`, `cli/internal/root.go:80`).
- `cmd.AddCommand(tun(cancel))` — the only subcommand
  (`cli/internal/root.go:82`).
- Returns `1` on execute error, `0` otherwise (`cli/internal/root.go:84-89`).

`parseLogLevel` (`cli/internal/root.go:92-105`) maps `debug/info/warn|warning/
error` (case-insensitive, trimmed) to `slog.Level`; empty == info; unknown ->
error.

### `tun` subcommand (`cli/internal/tun.go`)

`tun(cancel context.CancelFunc) *cobra.Command` (`cli/internal/tun.go:20`):
- Defends against nil cancel by substituting a no-op
  (`cli/internal/tun.go:24-26`).
- Flags `--from` and `--to`, both `<uri>` and both **required**
  (`cli/internal/tun.go:48-52`).
- `RunE` pulls ctx from the command (falling back to `context.Background()`),
  calls `runTun`, and on error joins the error with `cmd.Help()`
  (`cli/internal/tun.go:35-45`).

`runTun(ctx, cancel, from, to)` (`cli/internal/tun.go:57-101`):
1. Unmarshals `from` into `netx.ListenerURI` and `to` into `netx.DialerURI`
   (`cli/internal/tun.go:58-65`); these drive the chain parser
   (`uri.go:16-28`, `scheme.go`).
2. `fromURI.Listen(ctx)` opens the listener (`cli/internal/tun.go:67-71`).
3. Creates a `netx.TunMaster[struct{}]{}` (single anonymous route key,
   `cli/internal/tun.go:73`) and registers a route whose handler dials `toURI`
   per accepted conn and returns a `netx.Tun{Conn: conn, Peer: pconn}`
   (`cli/internal/tun.go:75-84`). `TunMaster.SetRoute` (`tun.go:86-109`) wraps
   the handler to spawn `Tun.Relay` per connection.
4. Serves in a goroutine; on non-`ErrServerClosed` error it logs and calls
   `cancel()` to bring the process down (`cli/internal/tun.go:86-91`).
5. Blocks on `<-ctx.Done()`, then `tm.Shutdown` with a **3s** timeout
   (`cli/internal/tun.go:95-99`).

Help text: `tunExample` (`cli/internal/tun.go:15-18`) and the large `uriFormat`
string (`cli/internal/help.go:3-43`) document the chain URI grammar
(`<transport>+<layer>{params}+...://<addr>`), supported transports
(tcp/udp/icmp), and every layer's params. The cert/key-as-hex requirement and
SPKI-pinning behavior are documented at `cli/internal/help.go:38-42`.

> NOTE: `uriFormat` lists `icmp` and `reality` semantics, but `reality` is not
> registered by any driver in this tree (see Driver activation) and its e2e
> probes are commented out. The help text is aspirational for reality.

---

## c-shared library / FFI (`cli/internal/lib/main.go`)

`package main` with a cgo preamble defining a C callback typedef and a nil-safe
invoker (`cli/internal/lib/main.go:3-12`):

```c
typedef void (*netx_callback_t)(const char* msg);
static inline void netx_call_callback(netx_callback_t cb, const char* msg) {
    if (cb != NULL) cb(msg);
}
```

`func main() {}` is empty (`cli/internal/lib/main.go:31`) — required for
`-buildmode=c-shared`/`c-archive`. It blank-imports all 8 drivers
(`cli/internal/lib/main.go:21-28`).

### Exported symbols

- `//export Netx` — `Netx(id *C.char, argc C.int, argv **C.char, outCB, errCB
  C.netx_callback_t) C.int` (`cli/internal/lib/main.go:101-131`).
- `//export NetxInterrupt` — `NetxInterrupt(id *C.char) C.int`
  (`cli/internal/lib/main.go:133-142`).

### `Netx` flow

1. **argv parsing** (`cli/internal/lib/main.go:103-114`): if `argc>0 && argv!=nil`,
   reinterprets `argv` as `(*[1<<28]*C.char)(...)[:length:length]` and converts
   each non-nil entry with `C.GoString`, stopping at the first nil pointer
   (`cli/internal/lib/main.go:108-113`). The `1<<28` is a fixed huge upper bound
   sliced down to `length` — safe as long as the caller passes a correct `argc`.
2. **id -> cancelable context** (`cli/internal/lib/main.go:116-118`): `eID =
   C.GoString(id)`; creates `ctx, cancel := context.WithCancel(Background())`;
   registers via `registerActiveCall(eID, cancel)`.
3. **cleanup** (`cli/internal/lib/main.go:119-122`): deferred
   `removeActiveCall(eID, entry)` then `cancel()`.
4. **run** (`cli/internal/lib/main.go:124-130`): calls `internal.Run(ctx, cancel,
   WithArgs(parsedArgs), WithOut(&outWriter{outCB}), WithErr(&errWriter{errCB}))`
   and returns the exit code as `C.int`.

### `NetxInterrupt` flow

`cli/internal/lib/main.go:133-142`: nil id -> return 0; otherwise
`interruptActiveCall(C.GoString(id))` -> return 1 if a call was found and
cancelled, else 0.

### The active-call map (`cli/internal/lib/main.go:59-99`)

```go
type activeEntry struct { cancel context.CancelFunc }
var (
    activeMu    sync.RWMutex
    activeCalls = make(map[string]*activeEntry)
)
```
(`cli/internal/lib/main.go:59-66`)

- `registerActiveCall(id, cancel)` (`:68-78`): under `activeMu.Lock()`, stores a
  new entry; **if a previous entry existed for the same id, it cancels the
  previous one after unlocking** — so calling `Netx` twice with the same id
  pre-empts the earlier call. Returns the new entry (used as an identity token).
- `removeActiveCall(id, entry)` (`:80-86`): deletes the map slot **only if it
  still points at this exact entry** (`activeCalls[id] == entry`). This guards
  against a newer call's entry being clobbered by an older call's deferred
  cleanup.
- `interruptActiveCall(id)` (`:88-99`): under lock, finds+deletes the entry, then
  (after unlock) cancels it; returns whether one was found.

### Callback writers & C memory (`cli/internal/lib/main.go:33-57`)

`outWriter`/`errWriter` each hold a `C.netx_callback_t`. Their `Write(p []byte)`:
- if `len(p)!=0 && cb!=nil`: `msg := C.CString(string(p))`, `defer
  C.free(unsafe.Pointer(msg))`, then `C.netx_call_callback(cb, msg)`
  (`:38-42`, `:51-55`).
- **always** also writes to the real `os.Stdout`/`os.Stderr` (`:43`, `:56`).

Memory contract: each callback message is a freshly `C.CString`-allocated
NUL-terminated buffer freed immediately after the synchronous callback returns
(`:40-41`). **The C callback MUST treat `msg` as valid only for the duration of
the call** — it is freed on return, so the consumer must copy if it needs to
retain it.

### Concurrency

- The map is guarded by `activeMu` (RWMutex) but every operation uses
  `Lock()`/write — the RW distinction is unused
  (`cli/internal/lib/main.go:71,81,89`).
- Multiple concurrent `Netx` calls with **distinct ids** run independently. Same
  id => the newer pre-empts the older (`registerActiveCall` cancels prev,
  `:74-76`).
- Callback invocation happens on the goroutine doing the `Write`; the C side must
  be thread-safe if it shares state across calls.

### e2e runner (`cli/internal/e2e/lib/main.go`)

A thin host that links the shared lib. It declares the two `extern` C prototypes
(`cli/internal/e2e/lib/main.go:6-7`), uses a fixed handle `"e2e"`
(`:19`), spins a goroutine that calls `interrupt()` (`NetxInterrupt`) on
SIGINT/SIGTERM (`:21-28`, `:53-57`), marshals `os.Args[1:]` into a C `argv`
(`C.CString` each, freed via defer, `:34-48`), and calls `C.Netx(...)` with nil
callbacks (`:49`). This is what `test:e2e:tun lib=true` builds and runs as
`$NETX` (`Taskfile.yml:120,139`).

---

## Driver activation (blank-import requirement)

Drivers self-register in `init()`: each driver calls `netx.Register("<name>",
...)` (e.g. `drivers/tls/tls.go:17`, all confirmed below). `netx.Register`
(`driver.go:15-25`) stores the driver under its name in a global map and
**panics on duplicate registration** (`driver.go:21-23`).

Registered names by source (the 8 driver modules):

| Driver module | Registered scheme | init at |
|---|---|---|
| `drivers/aesgcm` | `aesgcm` | `drivers/aesgcm/aesgcm.go:13` |
| `drivers/dnst` | `dnst` | `drivers/dnst/dnst.go:13` |
| `drivers/dtls` | `dtls` | `drivers/dtls/dtls.go:19` |
| `drivers/dtlspsk` | `dtlspsk` | `drivers/dtlspsk/dtlspsk.go:14` |
| `drivers/ssh` | `ssh` | `drivers/ssh/ssh.go:15` |
| `drivers/tls` | `tls` | `drivers/tls/tls.go:17` |
| `drivers/tlspsk` | `tlspsk` | `drivers/tlspsk/tlspsk.go:15` |
| `drivers/utls` | `utls` | `drivers/utls/utls.go:20` |

Core layers/transports register from the **root** module's own `init()`s and so
are always available wherever root is imported: `buf` (`buffered_conn.go:21`),
`demux` (`demux.go:25`), `frame` (`frame_conn.go:23`), `mux` (`mux.go:31`),
`poll` (`poll_conn.go:36`), `split` (`split_conn.go:17`); transports
`tcp`/`udp`/`icmp` are handled in `transport.go:9-13,50-58`.

**Because registration is `init()`-driven, a driver only activates if its package
is imported.** The cli pulls them in via blank imports in **two** files that must
stay in sync:

- `cli/cmd/netx/main.go:10-17` (the binary)
- `cli/internal/lib/main.go:21-28` (the FFI lib)

### What breaks if you forget one

- Add a new driver but blank-import it in **only** `main.go`: the **binary**
  supports it, but the **shared library** (`libnetx*.so/.dll/.a` and the
  `test:e2e:tun lib=true` runner) does NOT — `fromURI/toURI.UnmarshalText` or
  `Listen/Dial` will fail at runtime with `uri: unknown driver "<name>"`
  (`driver.go:31-32`), not at compile time.
- Forget both: the driver is dead weight; URIs referencing it fail at runtime.
- The failure is **silent at build time** because the new driver module still
  compiles standalone; only a URI exercising the scheme reveals it. There is no
  test asserting the two import lists match.

---

## CI/CD (.github/workflows)

### `lint_test_and_build.yml`

Triggers (`.github/workflows/lint_test_and_build.yml:4-13`): PRs to `main`,
pushes to `main`, **tags matching `cli/v[0-9]+.[0-9]+.[0-9]+`**, and
`workflow_dispatch`. Top-level perms `contents: read` (`:2-3`).

Jobs:
1. **`lint-test-and-build`** (`ubuntu-latest`, `:16-52`):
   - Installs cross C toolchains: `gcc-x86-64-linux-gnu`,
     `gcc-aarch64-linux-gnu`, `gcc-mingw-w64-x86-64` (`:19`).
   - Installs **llvm-mingw** (pinned `LLVM_MINGW_VERSION=20260421`) for windows
     arm64 and symlinks `aarch64-w64-mingw32-*` into `/usr/local/bin`
     (`:20-32`).
   - `setup-go` from `go.mod`, `setup-task` v3 (`:33-39`).
   - Runs `task deps`, `task lint`, `task test`, `task test:e2e:tun`,
     `task test:e2e:tun lib=true`, `task build` (`:41-46`).
   - Uploads `build/` as artifact named `${{ github.sha }}`
     (`if-no-files-found: error`, 7-day retention) (`:47-52`).
2. **`build-lib-darwin`** (`macos-latest`, `:54-73`): `task deps`,
   `task build:lib:darwin`; uploads `build/` as `darwin-${{ github.sha }}`
   (`:67-73`). Note: it does NOT install Android NDK toolchains, so the Android
   `.so` targets only build in the ubuntu job (and only if `$ANDROID_NDK_HOME`
   is set — see hazards).
3. **`release`** (`ubuntu-latest`, `:75-95`): gated by
   `if: github.event_name == 'push' && startsWith(github.ref, 'refs/tags/cli/')`
   (`:76`), `needs: [lint-test-and-build, build-lib-darwin]` (`:78`), perms
   `contents: write` (`:79-80`). Downloads both artifacts into `build/` and
   publishes a GitHub Release via `softprops/action-gh-release@v2` with
   `files: build/**` (`:82-95`).

> Release implication: **only `cli/v*` tags publish artifacts.** Tagging root,
> proto, or drivers does NOT run the release job (it isn't even part of the
> trigger filter `:11-12`, and the release `if` requires `refs/tags/cli/`,
> `:76`). The build matrix is keyed to the `cli` module's outputs.

### `codeql.yml`

CodeQL Advanced (`.github/workflows/codeql.yml:12`). Triggers on push/PR to
`main` and a weekly cron `31 0 * * 0` (`:14-20`). Matrix analyzes `actions`
(build-mode none) and `go` (build-mode **autobuild**) (`:44-50`). `go` runs on
ubuntu; perms include `security-events: write` (`:30-33`). Autobuild relies on
`go.work` to compile the whole workspace.

---

## Change hazards (MOST IMPORTANT)

Ordered roughly by likelihood × blast radius.

1. **Release-ordering / `go.work` masking (highest).** Editing a shared root
   symbol (`netx.Driver` `driver.go:8`, `Wrapper`, `Tun`/`TunMaster`
   `tun.go:14-109`, the URI/Scheme types `uri.go`/`scheme.go`/`transport.go`)
   compiles and tests green locally because `go.work` (`go.work:3-17`) uses
   on-disk source for every sibling — **even though each `go.mod` still pins root
   `v1.4.0`** (e.g. `drivers/tls/go.mod:5`). External `go get` consumers break.
   Mitigation: after any cross-module API change, bump+tag bottom-up
   (root -> `proto/aesgcm`,`proto/dnst` -> all `drivers/*` -> `cli`) and verify
   each go.mod `require` points at the new tag before tagging the next level.
   Note `proto/ssh` has no root dep (`proto/ssh/go.mod:5-7`) so it is exempt
   unless that changes.

2. **Forgetting a blank import in ONE of the two mains.** A new driver must be
   blank-imported in BOTH `cli/cmd/netx/main.go:10-17` AND
   `cli/internal/lib/main.go:21-28`. Miss the lib file and the FFI build /
   `test:e2e:tun lib=true` lacks the driver; miss `main.go` and the binary lacks
   it. Failure is **runtime-only** (`uri: unknown driver` `driver.go:31-32`),
   never a compile error, and nothing tests that the two lists agree. Also update
   the driver list in `cli/go.mod:7-15` (require) and re-tidy.

3. **FFI callback memory lifetime.** `outWriter`/`errWriter` free the
   `C.CString` immediately after the synchronous callback returns
   (`cli/internal/lib/main.go:40-41,54-55`). A C consumer that stores the
   pointer instead of copying gets a use-after-free. Do not make the Go side
   async or hold the pointer past the call.

4. **FFI concurrency / id collisions.** `activeCalls` is keyed by the
   caller-supplied id string. A second `Netx` with the same id **cancels the
   first** (`registerActiveCall` `:74-76`). Distinct concurrent calls need
   distinct ids. `removeActiveCall` only deletes if the entry identity still
   matches (`:82`), so deferred cleanup of an old call won't evict a newer one —
   preserve this identity check if you refactor. The `RWMutex` is only ever
   `Lock()`ed (`:71,81,89`); don't assume read-parallelism exists.

5. **argv parsing assumes a correct `argc`.** `Netx` slices `argv` to `length =
   int(argc)` (`cli/internal/lib/main.go:106-107`). A caller passing an `argc`
   larger than the real array (with no nil terminator before the end) reads out
   of bounds. The nil-pointer early-break (`:109-110`) only helps if the caller
   nil-terminates. Document the contract for FFI consumers.

6. **e2e hard-coded ports.** All e2e ports are fixed in `Taskfile.yml:160-164`
   (`48080/48081`, `49xxx`, `50xxx`). Any local/CI process on those ports makes
   `test:e2e:tun` flaky or fail; the harness has no port-retry. Adding a new
   protocol pair means picking unused ports in these ranges by hand. Also the
   harness relies on a flat `sleep 2` for readiness (`Taskfile.yml:199`) — slow
   CI can race.

7. **Toolchain drift in cross builds.** `task build` hard-codes specific CCs
   (`Taskfile.yml:37-50`) and CI installs exact packages plus a pinned
   `llvm-mingw` version (`.github/workflows/lint_test_and_build.yml:19-32`).
   Android targets need `$ANDROID_NDK_HOME` to be set (`Taskfile.yml:49-50`) —
   it is **not** set anywhere in CI, so `task build` would fail the Android steps
   if that env var is absent (verify before relying on Android artifacts).
   `build:lib:darwin` needs a macOS host + Xcode SDKs (`Taskfile.yml:78-89`).
   Bumping `LLVM_MINGW_VERSION` or the apt package names must keep the
   `aarch64-w64-mingw32-clang` symlink working.

8. **Stale/misleading metadata.** `netx --version` always prints `dev`
   (`cli/internal/root.go:56`), never the `cli/vX.Y.Z` tag — don't use it to
   identify a release. `CHANGELOG.md` stops at v1.2.0 while root is v1.4.0 and
   references paths that no longer exist (`cmd/netx_lib`, `internal/cli`,
   `CHANGELOG.md:14,17,22`); keep it updated per release if you depend on it.
   `uriFormat` advertises `reality` (`cli/internal/help.go`) which no driver in
   this tree registers (its e2e probes are commented out,
   `Taskfile.yml:184,197,216`).

9. **Release trigger scope.** Only `cli/v*` tags publish artifacts
   (`.github/workflows/lint_test_and_build.yml:11-12,76`); the artifact set is
   whatever `task build` + `task build:lib:darwin` produce. If you add a new
   build target to the Taskfile, add it to that job's outputs or it won't ship.
   The `release` job `needs` both build jobs (`:78`), so a macOS build failure
   blocks the release even though the binaries are fine.

---

## Related docs

- `docs/internals/pipeline.md` — the wrapper/scheme chain pipeline that
  `ListenerURI.Listen` / `DialerURI.Dial` drive (the engine behind `--from`/
  `--to`).
- `docs/internals/server-tun.md` — `Server[ID]` / `TunMaster` / `Tun.Relay`
  internals used by `runTun` (referenced by the prompt; create if absent).
- `docs/internals/drivers-proto.md` — per-driver and per-proto wrapper details
  (referenced by the prompt; create if absent).

> AMBIGUITY: at time of writing only `docs/internals/pipeline.md` exists under
> `docs/internals/` (plus `docs/mux-tag-poll.md`). `server-tun.md` and
> `drivers-proto.md` are referenced here per the documentation plan but are not
> yet present in the tree.
