# Server, Tun & TunMaster

> Internals doc for `server.go`, `tun.go`, `logger.go`. Package `netx` (module
> root, import path `github.com/pedramktb/go-netx`). Citations are by symbol +
> file; for the shared "source is the source of truth" policy see
> [README.md](README.md).

## Purpose & role

`Server[ID]` accepts connections from a `net.Listener` and routes each new
connection to a runtime-registered `Handler`, keyed by a comparable `ID`
(`Server` / `Handler` types in server.go). Handlers are tried in stored order;
the first one that "matches" owns the connection (`Server.route` in server.go).

`Tun` is one endpoint of a byte-relay between two `net.Conn`s
(`Conn` <-> `Peer`), copying bytes bidirectionally until either side closes
(`Tun` / `Tun.Relay` in tun.go).

`TunMaster[ID]` embeds `Server[ID]` and adapts a `TunHandler` (which returns a
`Tun`) into a `Server` `Handler`: on match it starts `Relay` in a goroutine and
wires the relay's completion to the server's connection-tracking `closed()`
callback (`TunMaster.SetRoute` in tun.go).

The CLI `tun` command instantiates `TunMaster[struct{}]` with a single route
(`struct{}{}`) whose handler supplies a `Peer` dialed from the `--to` endpoint
per accepted conn (`runTun` in cli/internal/tun.go). The dial path and its
recent reworks (eager pre-dial, darwin `IP_BOUND_IF`, readiness logging) are
covered in [modules-cli.md](modules-cli.md); see "CLI dial flow" below for the
high-level shape.

## Public API

| Symbol | File | Notes |
|---|---|---|
| `var ErrServerClosed` | server.go | `errors.New("server is shutting down")`. Returned by `Serve` after Close/Shutdown; also if `Serve` is started while already closing. |
| `type Handler func(...)` | server.go | See exact signature below. |
| `type Server[ID comparable] struct` | server.go | Zero value is usable; exported field `Logger Logger`. |
| `(*Server[ID]) Serve(ctx, listener) error` | server.go | Blocking accept loop. |
| `(*Server[ID]) SetRoute(id ID, handler Handler)` | server.go | Copy-on-write add/replace. |
| `(*Server[ID]) RemoveRoute(id ID)` | server.go | Copy-on-write remove. |
| `(*Server[ID]) Close() error` | server.go | Immediate: close listeners + all tracked conns. |
| `(*Server[ID]) Shutdown(ctx) error` | server.go | Graceful: stop accepting, wait for tracked conns until `ctx` done, then force-close. |
| `type Tun struct` | tun.go | Exported fields `Logger`, `Conn`, `Peer`, `BufferSize uint`. |
| `(*Tun) Relay(ctx)` | tun.go | Blocking bidirectional copy. |
| `(*Tun) Close() error` | tun.go | Idempotent; closes both ends. |
| `type TunHandler func(...)` | tun.go | See exact signature below. |
| `type TunMaster[ID comparable] struct{ Server[ID] }` | tun.go | Embeds `Server[ID]`. |
| `(*TunMaster[ID]) SetRoute(id ID, handler TunHandler)` | tun.go | Wraps the `TunHandler` into a `Server.SetRoute`. **Shadows** `Server.SetRoute` for `TunMaster` (different signature; see hazards). |
| `type Logger interface` | logger.go | `DebugContext/InfoContext/WarnContext/ErrorContext(ctx, msg, args...)`. |

Unexported supporting type: `route[ID]` (server.go).

### Exact `Handler` type (server.go)

```go
type Handler func(ctx context.Context, conn net.Conn, closed func()) (matched bool, wrappedConn io.Closer)
```

Doc-comment contract (above `Handler` in server.go): return `true` for matching
connections; if it does not match, **return `false` and a nil `wrappedConn`, and
DO NOT call `closed`**. Matching should be decided early. `wrappedConn` may be
the same conn or a wrapped version (TLS, obfuscation, etc.). The
implementation-level nuances differ slightly from this comment — see "Handler
contract".

### Exact `TunHandler` type (tun.go)

```go
type TunHandler func(ctx context.Context, conn net.Conn) (matched bool, connCtx context.Context, tunnel Tun)
```

A `TunHandler` returns whether it matches, a (possibly derived) `connCtx` used
for the tunnel's logging/relay context, and the `Tun` to relay (consumed by
`TunMaster.SetRoute` in tun.go).

## Handler contract (rules + `closed()` lifecycle)

Implemented in `Server.route` (server.go). For each accepted conn the server
iterates the route slice in stored order and calls each handler until one
matches:

1. **Decline → next route.** If the handler returns `matched == false`, the
   server `continue`s to the next route. The returned `wrappedConn` is ignored
   on decline. The `Handler` doc comment says to return a nil closer on decline,
   but the implementation does not read it — `TunMaster`'s wrapper actually
   returns `conn` (non-nil) on decline (`TunMaster.SetRoute` in tun.go), which
   is harmless because decline ignores the closer.

2. **Match → track.** If `matched == true`, the conn is tracked: the server
   records `wConn` (a `*io.Closer` pointing at the closer to use) in `s.conns`.

3. **nil closer fallback.** If the handler matched but returned a nil
   `wrappedConn`, the server tracks the **original** `conn` instead. So a matched
   conn is always tracked by *something* closeable.

4. **`closed()` must be called exactly once, and only on match.** `closed` is a
   per-conn closure (built fresh in `Server.route`) that removes `wConn` from
   `s.conns`. It is the handler's signal that the conn is logically done.
   Calling it removes the conn from the tracking set so `Close`/`Shutdown` no
   longer force-close it.

5. **`closed()` close-cooldown ordering.** There is a 1-slot `closeCooldown`
   channel (built in `Server.route`). `closed()` blocks on `<-closeCooldown`
   until the server has finished inserting `wConn` into the map and sends to the
   channel. This prevents a race where a fast handler calls `closed()` before
   `route` has added the conn to `s.conns` (which would otherwise leak the
   entry). **Consequence: if a matching handler never calls `closed()`, the conn
   stays tracked forever** (until Close/Shutdown force-close it).

6. **No-route / unhandled.** If `s.routes` is unset (no `SetRoute` ever ran),
   `route` closes the conn and logs `DEBUG "no routes configured, dropping
   connection"`. If routes exist but all decline, `route` closes the conn and
   logs `DEBUG "unhandled connection, dropping connection"`. In both cases the
   conn is dropped and never tracked.

### `closed()` lifecycle summary

- Created fresh per accepted conn, capturing `wConn` and `closeCooldown`
  (`Server.route` in server.go).
- Must be called **exactly once** by the matching handler when the conn is
  logically finished. Calling it twice deadlocks a goroutine and calling it zero
  times leaves the conn tracked — see hazards #1 and #2.
- `TunMaster` calls `closed()` for you, exactly once, after `Relay` returns
  (`TunMaster.SetRoute` in tun.go).

## Internal design & invariants

### Copy-on-write route slice

Routes are stored as `[]route[ID]` inside an `atomic.Value` (`Server.routes`
field; `route` type in server.go). Reads in the hot path (`Server.route`) load
the slice atomically with no lock. Writers (`SetRoute`/`RemoveRoute`) take
`routesMu` and always build a **new** slice, never mutating the shared backing
array:

- `SetRoute` first-insert: stores a fresh 1-element slice.
- `SetRoute` replace-existing: copies into a new slice of equal length, replaces
  one element, stores.
- `SetRoute` append-new: copies into a new slice of `len+1`, appends, stores.
- `RemoveRoute`: builds a new filtered slice excluding `id`, stores.

**Invariant:** a slice handed to `s.routes.Store` is never modified afterward,
so concurrent readers always see a consistent, immutable snapshot. Order is
insertion order for appends; replace preserves position; remove preserves
relative order.

### Tracking set

`s.conns map[*io.Closer]struct{}` (`Server` struct) holds one entry per matched,
not-yet-closed conn. Keyed by a **pointer** to an `io.Closer` (`wConn`) so each
accepted conn is a distinct map key even if two conns wrap the same underlying
closer. Lazily initialized under `s.mu` in `Server.route`. Mutated only under
`s.mu` (`closed()`, `Server.route`, `Close`, `Shutdown`). `closed()` deletes;
`Close`/`Shutdown` drain.

### Listener set & WaitGroup

`s.listeners map[net.Listener]struct{}` plus `s.listenerGroup sync.WaitGroup`
(`Server` struct). `addListener` lazily inits the map, refuses if already
`closing`, registers the listener and `Add(1)`. `removeListener` deletes and
`Done()`, called via `defer` when `Serve` returns. `Close`/`Shutdown` close
listeners then `listenerGroup.Wait()` to ensure every `Serve` loop has exited
before draining conns.

### `closing` flag

`s.closing atomic.Bool` (`Server` struct). Set via `CompareAndSwap(false, true)`
in both `Close` and `Shutdown` — so **only the first** of Close/Shutdown does
any work; subsequent calls return `nil` immediately. `Serve` checks it on accept
error to decide `ErrServerClosed` vs log-and-continue, and `addListener` checks
it to reject new listeners after close.

## Concurrency model

- **One `Serve` goroutine per listener.** `Serve` blocks in `listener.Accept()`.
  The same `Server` can serve multiple listeners concurrently; the listener set
  + WaitGroup track them (`addListener`/`removeListener` in server.go).
- **One goroutine per accepted conn.** Each accept spawns `go s.route(...)`.
  `route` runs the handler synchronously inside that goroutine.
- **`Logger` lazy init is racy if `Serve` runs concurrently with other writers.**
  `Serve` writes `s.Logger = slog.Default()` if nil without holding a lock. Set
  `Logger` before calling `Serve` to avoid a data race; the tests always set it
  first (e.g. `TestRouteReplacement` in server_test.go).
- **Locks:**
  - `routesMu` (`Server` struct): serializes route writers only.
  - `mu` (`Server` struct): guards `listeners`, `conns`. Note `Serve`'s lazy
    Logger write is NOT under `mu`.
  - `routes` (atomic.Value) and `closing` (atomic.Bool) are lock-free reads.
- **SetRoute/RemoveRoute during Serve are safe.** Readers (`route`) load the
  atomic snapshot; writers swap in new slices under `routesMu`. A conn accepted
  mid-swap sees either the old or the new snapshot, never a torn one. Existing
  conns created by a prior handler are NOT affected by route changes (see the
  `SetRoute`/`RemoveRoute` doc comments). `TestRouteReplacement` and
  `TestConcurrentSetRouteNoLoss` (server_test.go) exercise this.
- **No WaitGroup for per-conn goroutines.** Conn goroutines are tracked only via
  the `s.conns` map (and indirectly by `closed()`), not a WaitGroup. `Shutdown`
  polls `len(s.conns)` with a 10ms ticker rather than waiting on a group.

## Lifecycle & ownership

### `Close()` — immediate (`Server.Close` in server.go)

1. `CompareAndSwap` `closing` false→true; if already closing, return `nil`.
2. Close all listeners under `mu`, joining errors.
3. `listenerGroup.Wait()` — block until all `Serve` loops have returned. Each
   `Serve` returns `ErrServerClosed` because `closing` is set when `Accept`
   errors.
4. Under `mu`, close every tracked conn via `(*c).Close()` and delete it.
   Returns joined listener-close errors only.

`TestCloseClosesActiveConnections` (server_test.go) asserts that a
matched-but-open conn is closed by `Close`.

### `Shutdown(ctx)` — graceful (`Server.Shutdown` in server.go)

1. `CompareAndSwap` `closing` false→true; if already closing, return `nil`.
2. Close listeners under `mu`, join errors.
3. `listenerGroup.Wait()`.
4. Poll loop with a 10ms ticker:
   - If `len(s.conns) == 0`, return `err` (listener errors, usually nil).
   - On `ctx.Done()`: force-close all remaining tracked conns and return
     `errors.Join(err, ctx.Err())`. With a deadline ctx this is
     `context.DeadlineExceeded`.
   - On ticker tick: re-check.

`TestShutdownGraceful` (server_test.go) shows conns draining before the deadline
(returns nil). `TestShutdownTimeoutForcesClose` (server_test.go) shows the
deadline path returning `context.DeadlineExceeded` and force-closing the conn.

### Ownership of accepted conns

- A **declined / unhandled** conn is closed by `route` itself — caller/handler
  need do nothing.
- A **matched** conn is owned jointly: the handler controls logical lifetime via
  `closed()`, and `Close`/`Shutdown` will force-close whatever closer is tracked
  (`wrappedConn` or original conn) if `closed()` hasn't fired yet.
- The closer that gets force-closed is the handler's returned `wrappedConn`
  (`Close`/`Shutdown` dereference `*c`, which is `wConn`'s target set in
  `Server.route`). So if a handler wraps the conn (e.g. TLS), closing the wrapper
  is what runs — usually the desired behavior.

## Error semantics

- **Accept errors while not closing:** logged at WARN
  (`"error accepting connection"`) and the loop `continue`s — `Serve` does NOT
  return (`Serve` in server.go). A permanently-broken listener that keeps
  returning errors will spin/log forever (no backoff). This differs from
  `net/http.Server`, which returns on accept errors.
- **Accept errors while closing:** `Serve` returns `ErrServerClosed`. This is
  the normal shutdown exit.
- **`Serve` started after close:** `addListener` returns false (closing set), so
  `Serve` returns `ErrServerClosed` without entering the loop.
- **Declined conns:** closed and dropped, logged at DEBUG (`Server.route`).
- **No-routes conns:** closed and dropped, logged at DEBUG (`Server.route`).
- **Listener close errors:** joined into the `Close`/`Shutdown` return value.
- Callers are expected to treat `ErrServerClosed` as success: the CLI does
  `if err != nil && !errors.Is(err, netx.ErrServerClosed)` (`runTun` in
  cli/internal/tun.go).

## Tun specifics

### Relay = two half-duplex copies (`Tun.Relay` in tun.go)

`Relay` guards against nil `Conn`/`Peer` (returns early), lazy-inits `Logger` to
`slog.Default()`, then launches two goroutines:

- `halfCopy(Peer, Conn, sendErrCh)` — Conn→Peer ("send").
- `halfCopy(Conn, Peer, recvErrCh)` — Peer→Conn ("recv").

`Relay` blocks reading **both** error channels, so it returns only after **both**
copy directions have finished. Each `halfCopy` `defer t.Close()`s, so when the
first direction ends it closes both endpoints, which unblocks the second
`io.CopyBuffer` (read/write on a closed conn errors), and that second goroutine
reports `nil` (because `closing` is now set — see the `t.closing.Load()` branch
in `halfCopy`). This is why `Relay` reliably returns once *either* side closes.

### BufferSize / default (`Tun.halfCopy` in tun.go)

`BufferSize uint` (`Tun` struct). In `halfCopy`, if `BufferSize != 0` a buffer
of that size is allocated and passed to `io.CopyBuffer`. **If `BufferSize == 0`,
a nil buffer is passed**, and `io.CopyBuffer` allocates its own internal buffer
— the stdlib default of **32 KiB** (matches the `BufferSize` field comment).
Each direction gets its **own** buffer (two allocations per relay).

For packet/framing protocols the buffer size matters: integration tests set
`BufferSize: 64 << 10` (64 KiB) so a single UDP datagram fits in one read
(tun_udp_tcp_int_test.go). `MaxPacketSize` is 65535 (packet.go); `frameConn`
(constructor `NewFrameConn`, frame_conn.go) uses a 2-byte length header so a
frame can be up to 65535 bytes. ⚠️ The frame_conn.go package doc comment says
"4-byte length header" — that is **stale**; the header is `uint16` (2 bytes).
A `BufferSize` smaller than a datagram can split it across reads/writes — fine
for streams, **wrong for datagram-preserving relays**. See hazards.

### Close ordering & idempotency (`Tun.Close` in tun.go)

`Close` uses `CompareAndSwap(false, true)` on `closing` so it runs once; repeat
calls return `nil`. It closes `Conn` then `Peer`, treating `net.ErrClosed` as a
non-error for each, and joins remaining errors. **No nil-guard on `Conn`/`Peer`
here** — `Close` dereferences both. If `Conn`/`Peer` are nil, `Close` panics;
`Relay` avoids this by returning early for nil endpoints, but a direct
`Tun{}.Close()` would panic. See hazards.

### TunMaster integration & logging (`TunMaster.SetRoute` in tun.go)

`TunMaster.SetRoute` wraps a `TunHandler` into a `Server.Handler`:

1. Calls the `TunHandler`. On no-match returns `(false, conn)` — declines,
   server tries next route.
2. On match, logs INFO `"starting new tunnel"` with `tun`/`peer` addresses
   formatted as `network://host:port`.
3. Starts `go func(){ tunnel.Relay(connCtx); closed(); log INFO "tunnel closed" }()`.
   So `Relay` runs off the route goroutine, `closed()` fires exactly once when
   the relay finishes, and the close is logged.
4. Returns `(true, &tunnel)` — the server tracks `&tunnel` (a `*Tun`, an
   `io.Closer`). On `Close`/`Shutdown` force-close, the server calls
   `(*Tun).Close()`, closing both endpoints.

Logging reads `tunnel.Conn.RemoteAddr()` / `tunnel.Peer.RemoteAddr()` — these
are evaluated even at "starting" time, so a `Tun` whose `Conn`/`Peer` is nil
would panic in the log call before Relay starts. The CLI route guarantees both
are set on match (`runTun` in cli/internal/tun.go).

> ⚠️ Stale source comment: the `TunMaster` doc comment in tun.go says "add
> tunnel handlers via SetHandler" — the actual method is `SetRoute`. No
> `SetHandler` exists.

### CLI dial flow (high level)

The CLI handler builds the `Tun.Peer` by dialing `--to`. Details and citations
live in [modules-cli.md](modules-cli.md); the shape relevant to this subsystem:

- **Per-conn lazy dial** is the default: the route handler dials `--to` once per
  inbound conn (via the `dialPeer` closure in `runTun`) and logs
  `INFO "netx tun upstream dialed"`.
- **`--eager` / warm-peer**: when set, `runTun` pre-dials `--to` the moment the
  listener is bound (warming the obfuscation handshake). The first inbound conn
  consumes the warm peer via an `atomic.Pointer` swap; later conns fall back to
  the lazy dial. An unconsumed warm peer is closed on shutdown.
- **darwin `IP_BOUND_IF`**: the dial uses a `dialControl` hook that is a no-op
  off darwin and binds the outbound socket to the primary physical interface on
  macOS (`dialControl` in cli/internal/tun_darwin.go vs tun_other.go), so the
  obfuscation dial does not loop back into the NEPacketTunnelProvider.
- **Readiness signal**: there is **no `NETX_READY` literal** in the code despite
  the commit history; the listener-bound signal embedders latch on is the
  `slog.Info("netx tun started", ...)` line in `runTun`.

## Dependencies

**Depends on (stdlib only):** `context`, `errors`, `io`, `log/slog`, `net`,
`sync`, `sync/atomic`, `time` (server.go); `context`, `errors`, `io`,
`log/slog`, `net`, `sync/atomic` (tun.go); `context` (logger.go).

**Depended on by (in-repo):**
- `TunMaster` embeds `Server` (tun.go).
- CLI `tun` command uses `TunMaster[struct{}]` (`runTun` in cli/internal/tun.go),
  feeding it `ListenerURI.Listen` (`fromURI.Listen`) / `DialerURI.Dial`
  (`toURI.Dial`, inside the `dialPeer` closure) results.
- Integration tests combine it with `frameConn` (frame_conn.go) and TLS
  (tun_udp_tcp_int_test.go).

**External transforms** like `frameConn` (frame_conn.go), `BufConn`,
`aesgcm`/`dnst` proto conns, mux/demux, etc. are independent layers that a
`Handler`/`TunHandler` may wrap around `conn` before relaying; they are not
imported by server.go/tun.go.

### Logger interface & fallback (logger.go)

`Logger` requires the four `*Context` methods, satisfied by `*slog.Logger`. Both
`Server.Serve` and `Tun.Relay` lazily set a nil `Logger` to `slog.Default()`.
**Caveat:** the fallback is set inside `Serve`/`Relay`, not in
`route`/`halfCopy`/`Close`. Code paths that touch `s.Logger` before `Serve`
runs, or `t.Logger` outside `Relay`, could hit a nil Logger. `TunMaster.SetRoute`
uses `m.Logger`, only guaranteed non-nil after `Serve` ran (or if the caller set
it) — in practice `Serve` runs first. Tests set `Logger` explicitly
(server_test.go, tun_test.go).

## Change hazards (MOST IMPORTANT)

1. **Forgetting `closed()` leaks tracking and blocks graceful Shutdown.** A
   matching handler that never calls `closed()` keeps its conn in `s.conns`
   forever. `Shutdown` then waits the full `ctx` deadline and force-closes.
   `Close` will still close it immediately. If you add a new handler, ensure
   `closed()` is called on every logical-completion path exactly once.
   `TunMaster` does this for tunnels; custom handlers must too.

2. **Double-calling `closed()` deadlocks a goroutine.** The `closeCooldown`
   channel (built in `Server.route`) holds exactly one value; a second
   `closed()` blocks on `<-closeCooldown` forever, leaking whatever goroutine
   called it (handler or relay). Never call `closed()` twice. Be careful if
   refactoring `Relay`/`TunMaster` to not invoke `closed()` from multiple paths.

3. **Handlers must decide match quickly and not block the route goroutine on
   decline.** Each conn is handled in its own goroutine, but handlers are tried
   **sequentially within that goroutine** (`Server.route`). A slow/blocking
   handler delays trying subsequent routes for that conn and can pile up
   goroutines under load. Matching should be early. For long-lived work, a
   matched handler should return promptly and do the work in its own goroutine
   (as `TunMaster` does) — do NOT block inside the handler return path.

4. **A matched handler that wraps but returns nil closer loses the wrapper on
   force-close.** If a handler wraps `conn` (TLS/obfuscation) but returns
   `(true, nil)`, the server tracks the **original** conn (`Server.route`) and
   force-close will close the raw conn, not the wrapper. Return the wrapper as
   `wrappedConn` so `Close`/`Shutdown` close the right thing.

5. **Copy-on-write assumption: never mutate a stored route slice in place.** All
   readers rely on stored slices being immutable (`Server.route` reads without
   lock). If you add a writer, it MUST build a new slice and `Store` it under
   `routesMu` (`SetRoute`/`RemoveRoute`). Appending to a loaded slice, or
   mutating an element, would corrupt concurrent reads. Also: `SetRoute` does NOT
   close existing conns from a replaced handler — replacing a route is not a way
   to tear down its live conns.

6. **`Relay` buffer size vs frame/packet size.** With `BufferSize == 0` the
   relay uses 32 KiB (`Tun.halfCopy`). For datagram-preserving paths
   (UDP-over-stream via `frameConn`), the buffer must be >= the largest datagram
   (up to `MaxPacketSize` 65535, packet.go) or a single read may not capture a
   whole datagram. Integration tests use 64 KiB. Changing the default or the
   field semantics can silently corrupt datagram tunnels. Note the CLI does NOT
   set `BufferSize` (`runTun` constructs `netx.Tun{Conn, Peer}` only), so it
   relies on the 32 KiB default — adequate for byte streams, but under-sized for
   max-size framed datagrams (a latent edge case).

7. **`Tun.Close()` has no nil-guard on `Conn`/`Peer`.** It dereferences both
   (`Tun.Close`). `Relay` guards nil endpoints and the `TunMaster` logging
   dereferences `RemoteAddr()` — so an improperly constructed `Tun` (nil
   endpoint) returned as matched will panic in the route goroutine / on
   force-close. Keep both endpoints non-nil on match.

8. **Goroutine leaks on partial close are mostly prevented but order-sensitive.**
   `Relay` returns only after both directions finish; the `defer t.Close()` in
   the first-finishing `halfCopy` unblocks the other. If you change `Relay` to
   not close both ends when one side ends, the surviving copy goroutine (and thus
   `Relay`, `closed()`, and the tracked conn) can leak. The `closing`-aware error
   suppression (`halfCopy`) depends on `Close` having flipped `closing` before
   the second copy errors.

9. **Accept loop never returns on non-closing errors (no backoff).** Unlike
   `net/http`, `Serve` logs and continues on accept errors. A listener stuck
   returning errors busy-loops. If you swap in a listener with different error
   semantics, account for this.

10. **`closing` is one-shot for both Close and Shutdown.** The first of the two
    wins; the other returns `nil` immediately. Calling `Shutdown` after `Close`
    (or vice versa) is a silent no-op — don't rely on Shutdown's graceful drain
    if Close already ran.

11. **`TunMaster.SetRoute` shadows `Server.SetRoute`.** Because `TunMaster`
    embeds `Server[ID]` and defines its own `SetRoute` with a `TunHandler`
    signature, calling `m.SetRoute(...)` uses the tunnel version. The embedded
    raw `Server.SetRoute` is still reachable as `m.Server.SetRoute(...)`. Mixing
    the two on one `TunMaster` is possible and would register raw handlers
    alongside tunnel handlers — likely unintended.

12. **`Logger` lazy-init races / nil deref outside Serve/Relay.** Setting
    `Logger` after `Serve` has started races with the lazy assignment (not under
    lock). Always set `Logger` before `Serve`. Paths that use the logger before
    `Serve`/`Relay` ran can nil-panic (see Dependencies > Logger fallback).

13. **Route racing `Close`/`Shutdown` is closed, not tracked.** `route` reads
    `s.closing` under `s.mu` at its insert point; if the server is closing it
    closes the conn (and any relay the handler started) instead of inserting it
    into `s.conns`. `Close`/`Shutdown` CAS `closing` *before* taking `s.mu`, so
    their force-close pass and a late insert can't both miss the conn (the bug
    this fixed: a leaked conn + relay goroutine). The `connCloser.Close()` +
    `closeCooldown` send run **outside** `s.mu` to avoid a self-reentrant deadlock
    if a handler's `closed()` runs synchronously. Consequence: a conn accepted
    just as `Shutdown` begins may be closed rather than served gracefully (it was
    never committed to tracking). Covered by `TestServer_RouteAfterCloseClosesConn`.

## Tests covering this

Run only this subsystem's tests from the **repo root** (root-module tests):

```bash
go test -run 'TestRoute|TestRemoveRoute|TestClose|TestConcurrentSetRoute|TestShutdown|TestTun|TestInt_' .
```

- `TestRouteReplacement` (server_test.go): hot-swap a route under the same ID
  mid-serve; verifies new conns use the new handler (`h1`→`h2`) and `Close`
  makes `Serve` return `ErrServerClosed`.
- `TestRemoveRouteThenUnhandled` (server_test.go): add then remove a route;
  verifies an unhandled conn is dropped (read errors) and a DEBUG drop log is
  emitted (accepts either "unhandled..." or "no routes..." wording).
- `TestCloseClosesActiveConnections` (server_test.go): a matched conn left open
  is force-closed by `Close`.
- `TestConcurrentSetRouteNoLoss` (server_test.go): 100 concurrent `SetRoute`
  then 100 `RemoveRoute` — exercises copy-on-write under race; only checks no
  panic + graceful drop (does NOT assert final route contents — see gaps).
- `TestShutdownGraceful` (server_test.go): conn drains before deadline;
  `Shutdown` returns nil.
- `TestShutdownTimeoutForcesClose` (server_test.go): handler never calls
  `closed()`; `Shutdown` waits ~timeout, returns `context.DeadlineExceeded`,
  force-closes the conn.
- `TestTunRelayBidirectional` (tun_test.go): both directions copy; graceful
  `Shutdown` after closing both ends.
- `TestTunShutdownTimeoutForcesClose` (tun_test.go): active relay, forced
  `Shutdown` (`DeadlineExceeded`), both ends error afterward.
- `TestInt_UDP_over_TCP_TunMasters` (tun_udp_tcp_int_test.go): two `TunMaster`s
  bridging UDP<->framed stream end to end, 64 KiB buffers.
- `TestInt_TunMasterRouting_PlainAndTLS` (tun_udp_tcp_int_test.go): single
  listener, two routes (TLS-first then plain fallback) selected by
  `conn.(interface{ ConnectionState() tls.ConnectionState })`, exercising the
  decline→next-route path and ordering.

**Gaps / not directly tested:**
- Double-call of `closed()` (hazard #2) — no test; relies on contract.
- `Tun.Close()` / `TunMaster` logging on nil `Conn`/`Peer` (hazard #7) — no
  test.
- Accept-error continue/no-backoff path (`Serve`) — no direct test (the test
  `chanListener` returns `net.ErrClosed` only on Close, which is the closing
  path).
- Multiple concurrent listeners on one `Server` (`listenerGroup` > 1) — not
  exercised; tests use one listener each.
- `TestConcurrentSetRouteNoLoss` does not assert the resulting route set, only
  absence of panic.
- Logger lazy-init race (hazard #12) — not exercised (tests always preset
  Logger).

## Related docs

- [pipeline.md](pipeline.md) — driver registry, `Wrapper` typed pipeline, URI/
  scheme parsing & chain validation.
- [mux.md](mux.md) / [demux.md](demux.md) — multiplexing layers.
- [poll-tagged.md](poll-tagged.md) — `PollConn` and the `TaggedConn` tag
  contract.
- [stream-transforms.md](stream-transforms.md) — `frameConn` (`NewFrameConn`),
  `BufConn`, `SplitConn` and other byte transforms a handler may wrap.
- [drivers-proto.md](drivers-proto.md) — proto drivers (aesgcm, dnst, etc.).
- [icmp.md](icmp.md) — ICMP transport.
- [modules-cli.md](modules-cli.md) — CLI `tun` command wiring
  (cli/internal/tun.go), including the eager/warm-peer dial flow, darwin
  `IP_BOUND_IF`, and the c-shared FFI library.
- [../mux-tag-poll.md](../mux-tag-poll.md) — DNS-tunnel data-flow diagrams (root
  `docs/`, not `docs/internals/`; partially stale — trust the source).
