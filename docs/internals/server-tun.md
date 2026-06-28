# Server, Tun & TunMaster

> Internals doc for `server.go`, `tun.go`, `logger.go`. Every claim is grounded in
> `file:line`. Source must NOT be changed based on this doc; it describes current
> behavior so future edits avoid unintended consequences.
>
> Package: `netx` (module root). Import path `github.com/pedramktb/go-netx`
> (server_test.go:13).

## Purpose & role

`Server[ID]` accepts connections from a `net.Listener` and routes each new
connection to a runtime-registered `Handler`, keyed by a comparable `ID`
(server.go:29-47, server.go:49-70). Handlers are tried in order; the first one
that "matches" owns the connection (server.go:125-161).

`Tun` is one endpoint of a byte-relay between two `net.Conn`s
(`Conn` <-> `Peer`), copying bytes bidirectionally until either side closes
(tun.go:12-45).

`TunMaster[ID]` embeds `Server[ID]` and adapts a `TunHandler` (which returns a
`Tun`) into a `Server` `Handler`: on match it starts `Relay` in a goroutine and
wires the relay's completion to the server's connection-tracking `closed()`
callback (tun.go:76-109).

The CLI `tun` command instantiates `TunMaster[struct{}]` with a single route
(`struct{}{}`) that dials the `--to` endpoint per accepted conn
(cli/internal/tun.go:73-91).

## Public API

| Symbol | Location | Notes |
|---|---|---|
| `var ErrServerClosed` | server.go:14-16 | `errors.New("server is shutting down")`. Returned by `Serve` after Close/Shutdown; also returned by `Serve` if started while already closing (server.go:54-56). |
| `type Handler func(...)` | server.go:27 | See exact signature below. |
| `type Server[ID comparable] struct` | server.go:32-47 | Zero value is usable; exported field `Logger Logger`. |
| `(*Server[ID]) Serve(ctx, listener) error` | server.go:49-70 | Blocking accept loop. |
| `(*Server[ID]) SetRoute(id ID, handler Handler)` | server.go:75-99 | Copy-on-write add/replace. |
| `(*Server[ID]) RemoveRoute(id ID)` | server.go:103-118 | Copy-on-write remove. |
| `(*Server[ID]) Close() error` | server.go:184-211 | Immediate: close listeners + all tracked conns. |
| `(*Server[ID]) Shutdown(ctx) error` | server.go:218-261 | Graceful: stop accepting, wait for tracked conns until `ctx` done, then force-close. |
| `type Tun struct` | tun.go:12-20 | Exported fields `Logger`, `Conn`, `Peer`, `BufferSize uint`. |
| `(*Tun) Relay(ctx)` | tun.go:23-45 | Blocking bidirectional copy. |
| `(*Tun) Close() error` | tun.go:61-74 | Idempotent; closes both ends. |
| `type TunHandler func(...)` | tun.go:76 | See exact signature below. |
| `type TunMaster[ID comparable] struct{ Server[ID] }` | tun.go:81 | Embeds `Server[ID]`. |
| `(*TunMaster[ID]) SetRoute(id ID, handler TunHandler)` | tun.go:86-109 | Wraps the `TunHandler` into a `Server.SetRoute`. **Shadows** `Server.SetRoute` for `TunMaster` (different signature; see hazards). |
| `type Logger interface` | logger.go:5-10 | `DebugContext/InfoContext/WarnContext/ErrorContext(ctx, msg, args...)`. |

Unexported supporting types: `route[ID]` (server.go:120-123), `frameConn`
(used in integration tests) is in frame_conn.go, not part of this subsystem.

### Exact `Handler` type (server.go:27)

```go
type Handler func(ctx context.Context, conn net.Conn, closed func()) (matched bool, wrappedConn io.Closer)
```

Doc comment contract (server.go:18-26): return `true` for matching connections;
if it does not match, **return `false` and a nil `wrappedConn`, and DO NOT call
`closed`**. Matching should be decided early. `wrappedConn` may be the same conn
or a wrapped version (TLS, obfuscation, etc.). (See "Handler contract" for the
implementation-level nuances, which differ slightly from this comment.)

### Exact `TunHandler` type (tun.go:76)

```go
type TunHandler func(ctx context.Context, conn net.Conn) (matched bool, connCtx context.Context, tunnel Tun)
```

A `TunHandler` returns whether it matches, a (possibly derived) `connCtx` used
for the tunnel's logging/relay context, and the `Tun` to relay
(tun.go:87-108).

## Handler contract (rules + `closed()` lifecycle)

Implemented in `Server.route` (server.go:125-161). For each accepted conn the
server iterates the route slice in stored order and calls each handler until one
matches:

1. **Decline → next route.** If the handler returns `matched == false`, the
   server `continue`s to the next route (server.go:143-145). The returned
   `wrappedConn` is ignored on decline. The doc comment says to return a nil
   closer on decline (server.go:24), but the implementation does not read it —
   `TunMaster`'s wrapper actually returns `conn` (non-nil) on decline
   (tun.go:90), which is harmless because decline ignores the closer.

2. **Match → track.** If `matched == true`, the conn is tracked: the server
   records `wConn` (a `*io.Closer` pointing at the closer to use) in
   `s.conns` (server.go:150-155).

3. **nil closer fallback.** If the handler matched but returned a nil
   `wrappedConn`, the server tracks the **original** `conn` instead
   (server.go:146-149). So a matched conn is always tracked by *something*
   closeable.

4. **`closed()` must be called exactly once, and only on match.** `closed` is a
   per-conn closure (server.go:137-142) that removes `wConn` from `s.conns`.
   It is the handler's signal that the conn is logically done. Calling it
   removes the conn from the tracking set so `Close`/`Shutdown` no longer
   force-close it.

5. **`closed()` close-cooldown ordering.** There is a 1-slot
   `closeCooldown` channel (server.go:136). `closed()` blocks on
   `<-closeCooldown` (server.go:138) until the server has finished inserting
   `wConn` into the map and sends to the channel (server.go:156). This prevents
   a race where a fast handler calls `closed()` before `route` has added the
   conn to `s.conns` (which would otherwise leak the entry). **Consequence: if a
   matching handler never calls `closed()`, nothing leaks the cooldown channel,
   but the conn stays tracked forever** (until Close/Shutdown force-close it).

6. **No-route / unhandled.** If `s.routes` is unset (no `SetRoute` ever ran),
   `route` closes the conn and logs `DEBUG "no routes configured, dropping
   connection"` (server.go:126-131). If routes exist but all decline, `route`
   closes the conn and logs `DEBUG "unhandled connection, dropping connection"`
   (server.go:159-160). In both cases the conn is dropped and never tracked.

### `closed()` lifecycle summary

- Created fresh per accepted conn (server.go:137).
- Captures `wConn` (server.go:134) and `closeCooldown` (server.go:136).
- Must be called **exactly once** by the matching handler when the conn is
  logically finished. Calling it twice will attempt a second
  `<-closeCooldown` which **blocks forever** (the channel only receives one
  value, server.go:156) — so a double-call leaks a goroutine. Calling it zero
  times leaves the conn tracked (blocks graceful `Shutdown` until ctx deadline).
- `TunMaster` calls `closed()` for you, exactly once, after `Relay` returns
  (tun.go:99-100).

## Internal design & invariants

### Copy-on-write route slice

Routes are stored as `[]route[ID]` inside an `atomic.Value`
(server.go:36, server.go:120-123). Reads in the hot path (`route`) load the
slice atomically with no lock (server.go:126). Writers (`SetRoute`/
`RemoveRoute`) take `routesMu` (server.go:37) and always build a **new** slice,
never mutating the shared backing array:

- `SetRoute` first-insert: stores a fresh 1-element slice (server.go:80-83).
- `SetRoute` replace-existing: copies into a new slice of equal length, replaces
  one element, stores (server.go:85-93).
- `SetRoute` append-new: copies into a new slice of `len+1`, appends, stores
  (server.go:94-98).
- `RemoveRoute`: builds a new filtered slice excluding `id`, stores
  (server.go:110-117).

**Invariant:** a slice handed to `s.routes.Store` is never modified afterward,
so concurrent readers always see a consistent, immutable snapshot. Order is
insertion order for appends; replace preserves position; remove preserves
relative order.

### Tracking set

`s.conns map[*io.Closer]struct{}` (server.go:46) holds one entry per matched,
not-yet-closed conn. Keyed by a **pointer** to an `io.Closer` (`wConn`,
server.go:134) so each accepted conn is a distinct map key even if two conns
wrap the same underlying closer. Lazily initialized under `s.mu`
(server.go:151-153). Mutated only under `s.mu` (server.go:139-141,
150-155, 204-207, 250-255). `closed()` deletes; `Close`/`Shutdown` drain.

### Listener set & WaitGroup

`s.listeners map[net.Listener]struct{}` plus `s.listenerGroup sync.WaitGroup`
(server.go:43-44). `addListener` lazily inits the map, refuses if already
`closing`, registers the listener and `Add(1)` (server.go:163-175).
`removeListener` deletes and `Done()` (server.go:177-182), called via `defer`
when `Serve` returns (server.go:57). `Close`/`Shutdown` close listeners then
`listenerGroup.Wait()` to ensure every `Serve` loop has exited before draining
conns (server.go:200, 234).

### `closing` flag

`s.closing atomic.Bool` (server.go:39). Set via `CompareAndSwap(false, true)` in
both `Close` (server.go:185) and `Shutdown` (server.go:219) — so **only the
first** of Close/Shutdown does any work; subsequent calls return `nil`
immediately. `Serve` checks it on accept error to decide `ErrServerClosed` vs
log-and-continue (server.go:62-66), and `addListener` checks it to reject new
listeners after close (server.go:169-171).

## Concurrency model

- **One `Serve` goroutine per listener.** `Serve` blocks in `listener.Accept()`
  (server.go:60). The same `Server` can serve multiple listeners concurrently;
  the listener set + WaitGroup track them (server.go:163-182).
- **One goroutine per accepted conn.** Each accept spawns `go s.route(...)`
  (server.go:68). `route` runs the handler synchronously inside that goroutine.
- **`Logger` lazy init is racy if `Serve` runs concurrently with other writers.**
  `Serve` writes `s.Logger = slog.Default()` if nil (server.go:50-52) without
  holding a lock. Set `Logger` before calling `Serve` to avoid a data race; the
  tests always set it first (e.g. server_test.go:20-21).
- **Locks:**
  - `routesMu` (server.go:37): serializes route writers only.
  - `mu` (server.go:41): guards `listeners`, `conns`. Note `Serve`'s lazy
    Logger write is NOT under `mu`.
  - `routes` (atomic.Value) and `closing` (atomic.Bool) are lock-free reads.
- **SetRoute/RemoveRoute during Serve are safe.** Readers (`route`) load the
  atomic snapshot; writers swap in new slices under `routesMu`. A conn accepted
  mid-swap sees either the old or the new snapshot, never a torn one. Existing
  conns created by a prior handler are NOT affected by route changes
  (server.go:73-74, 102). `TestRouteReplacement` (server_test.go:16-89) and
  `TestConcurrentSetRouteNoLoss` (server_test.go:209-262) exercise this.
- **No WaitGroup for per-conn goroutines.** Conn goroutines are tracked only via
  the `s.conns` map (and indirectly by `closed()`), not a WaitGroup. `Shutdown`
  polls `len(s.conns)` with a 10ms ticker (server.go:238-260) rather than
  waiting on a group.

## Lifecycle & ownership

### `Close()` — immediate (server.go:184-211)

1. `CompareAndSwap` `closing` false→true; if already closing, return `nil`
   (server.go:185-187).
2. Close all listeners under `mu`, joining errors (server.go:190-197).
3. `listenerGroup.Wait()` — block until all `Serve` loops have returned
   (server.go:200). Each `Serve` returns `ErrServerClosed` because `closing` is
   set when `Accept` errors (server.go:62-63).
4. Under `mu`, close every tracked conn via `(*c).Close()` and delete it
   (server.go:203-208). Returns joined listener-close errors only.

`TestCloseClosesActiveConnections` (server_test.go:154-207) asserts that a
matched-but-open conn is closed by `Close`.

### `Shutdown(ctx)` — graceful (server.go:218-261)

1. `CompareAndSwap` `closing` false→true; if already closing, return `nil`
   (server.go:219-221).
2. Close listeners under `mu`, join errors (server.go:224-231).
3. `listenerGroup.Wait()` (server.go:234).
4. Poll loop with a 10ms ticker (server.go:238-260):
   - If `len(s.conns) == 0`, return `err` (listener errors, usually nil)
     (server.go:241-246).
   - On `ctx.Done()`: force-close all remaining tracked conns and return
     `errors.Join(err, ctx.Err())` (server.go:247-256). With a deadline ctx this
     is `context.DeadlineExceeded`.
   - On ticker tick: re-check (server.go:257-258).

`TestShutdownGraceful` (server_test.go:264-321) shows conns draining before the
deadline (returns nil). `TestShutdownTimeoutForcesClose`
(server_test.go:323-395) shows the deadline path returning
`context.DeadlineExceeded` and force-closing the conn.

### Ownership of accepted conns

- A **declined / unhandled** conn is closed by `route` itself (server.go:128,
  159) — caller/handler need do nothing.
- A **matched** conn is owned jointly: the handler controls logical lifetime via
  `closed()`, and `Close`/`Shutdown` will force-close whatever closer is tracked
  (`wrappedConn` or original conn) if `closed()` hasn't fired yet.
- The closer that gets force-closed is the handler's returned `wrappedConn`
  (server.go:204-205 dereference `*c`, which is `wConn`'s target updated at
  server.go:137/147). So if a handler wraps the conn (e.g. TLS), closing the
  wrapper is what runs — usually the desired behavior.

### Double-call of `closed()`

Not guarded. Second call blocks permanently on `<-closeCooldown`
(server.go:138, channel has one buffered slot consumed by the first call). This
leaks the goroutine that called it (potentially the handler goroutine or a
relay goroutine). See hazards.

## Error semantics

- **Accept errors while not closing:** logged at WARN
  (`"error accepting connection"`) and the loop `continue`s — `Serve` does NOT
  return (server.go:61-66). A permanently-broken listener that keeps returning
  errors will spin/log forever (no backoff). Note this differs from
  `net/http.Server`, which returns on accept errors.
- **Accept errors while closing:** `Serve` returns `ErrServerClosed`
  (server.go:62-63). This is the normal shutdown exit.
- **`Serve` started after close:** `addListener` returns false (closing set), so
  `Serve` returns `ErrServerClosed` without entering the loop (server.go:54-56).
- **Declined conns:** closed and dropped, logged at DEBUG (server.go:159-160).
- **No-routes conns:** closed and dropped, logged at DEBUG (server.go:128-129).
- **Listener close errors:** joined into the `Close`/`Shutdown` return value
  (server.go:191-196, 225-230, 256).
- Callers are expected to treat `ErrServerClosed` as success: CLI does
  `if err != nil && !errors.Is(err, netx.ErrServerClosed)` (cli/internal/tun.go:87).

## Tun specifics

### Relay = two half-duplex copies (tun.go:23-45)

`Relay` guards against nil `Conn`/`Peer` (returns early, tun.go:24-26), lazy-inits
`Logger` to `slog.Default()` (tun.go:27-29), then launches two goroutines:

- `halfCopy(Peer, Conn, sendErrCh)` — Conn→Peer ("send", tun.go:34).
- `halfCopy(Conn, Peer, recvErrCh)` — Peer→Conn ("recv", tun.go:35).

`Relay` blocks reading **both** error channels (tun.go:37-38), so it returns only
after **both** copy directions have finished. Each `halfCopy` `defer t.Close()`s
(tun.go:52), so when the first direction ends it closes both endpoints, which
unblocks the second `io.CopyBuffer` (read/write on a closed conn errors), and
that second goroutine reports `nil` (because `closing` is now set, tun.go:54-57).
This is why `Relay` reliably returns once *either* side closes.

### BufferSize / default (tun.go:47-53)

`BufferSize uint` (tun.go:18). In `halfCopy`, if `BufferSize != 0` a buffer of
that size is allocated and passed to `io.CopyBuffer` (tun.go:49-51, 53). **If
`BufferSize == 0`, a nil buffer is passed**, and `io.CopyBuffer` allocates its
own internal buffer — the stdlib default of **32 KiB** (matches the field
comment at tun.go:18). Each direction gets its **own** buffer (two allocations
per relay).

For packet/framing protocols the buffer size matters: integration tests set
`BufferSize: 64 << 10` (64 KiB) so a single UDP datagram fits in one read
(tun_udp_tcp_int_test.go:109-110, 121-122, 208, 218). `MaxPacketSize` is 65535
(packet.go:4); `FrameConn` uses a 2-byte length header so a frame can be up to
65535 bytes (frame_conn.go:71-87, 94-95). A `BufferSize` smaller than a datagram
can split it across reads/writes — fine for streams, **wrong for
datagram-preserving relays**. See hazards.

### Close ordering & idempotency (tun.go:61-74)

`Close` uses `CompareAndSwap(false, true)` on `closing` so it runs once; repeat
calls return `nil` (tun.go:62-64). It closes `Conn` then `Peer`, treating
`net.ErrClosed` as a non-error for each (tun.go:65-73), and joins remaining
errors. **No nil-guard on `Conn`/`Peer` here** — `Close` dereferences both. If
`Conn`/`Peer` are nil, `Close` panics; `Relay` avoids this by returning early
for nil endpoints (tun.go:24-26), but a direct `Tun{}.Close()` would panic. See
hazards.

### TunMaster integration & logging (tun.go:86-109)

`TunMaster.SetRoute` wraps a `TunHandler` into a `Server.Handler`:

1. Calls the `TunHandler` (tun.go:88). On no-match returns `(false, conn)`
   (tun.go:89-91) — declines, server tries next route.
2. On match, logs INFO `"starting new tunnel"` with `tun`/`peer` addresses
   formatted as `network://host:port` (tun.go:93-96).
3. Starts `go func(){ tunnel.Relay(connCtx); closed(); log INFO "tunnel closed" }()`
   (tun.go:98-105). So `Relay` runs off the route goroutine, `closed()` fires
   exactly once when the relay finishes, and the close is logged.
4. Returns `(true, &tunnel)` (tun.go:107) — the server tracks `&tunnel` (a
   `*Tun`, which is an `io.Closer`). On `Close`/`Shutdown` force-close, the
   server calls `(*Tun).Close()`, closing both endpoints.

Logging uses `tunnel.Conn.RemoteAddr()` / `tunnel.Peer.RemoteAddr()`
(tun.go:94-95, 102-103) — these are evaluated even at "starting" time, so a
`Tun` whose `Conn`/`Peer` is nil would panic in the log call before Relay starts.
The CLI route guarantees both are set on match (cli/internal/tun.go:83).

## Dependencies

**Depends on (stdlib only):** `context`, `errors`, `io`, `log/slog`, `net`,
`sync`, `sync/atomic`, `time` (server.go:3-12); `context`, `errors`, `io`,
`log/slog`, `net`, `sync/atomic` (tun.go:3-10); `context` (logger.go:3).

**Depended on by (in-repo):**
- `TunMaster` embeds `Server` (tun.go:81).
- CLI `tun` command uses `TunMaster[struct{}]` (cli/internal/tun.go:73-91),
  feeding it `ListenerURI.Listen` / `DialerURI.Dial` results (cli/internal/tun.go:67, 76).
- Integration tests combine it with `FrameConn` (frame_conn.go) and TLS
  (tun_udp_tcp_int_test.go).

**External transforms** like `FrameConn` (frame_conn.go), `BufferedConn`,
`aesgcm`/`dnst` proto conns, mux/demux, etc. are independent layers that a
`Handler`/`TunHandler` may wrap around `conn` before relaying; they are not
imported by server.go/tun.go.

### Logger interface & fallback (logger.go:5-10)

`Logger` requires the four `*Context` methods, satisfied by `*slog.Logger`. Both
`Server.Serve` (server.go:50-52) and `Tun.Relay` (tun.go:27-29) lazily set a nil
`Logger` to `slog.Default()`. **Caveat:** the fallback is set inside `Serve`/
`Relay`, not in `route`/`halfCopy`/`Close`. Code paths that touch `s.Logger`
before `Serve` runs, or `t.Logger` outside `Relay`, could hit a nil Logger.
`TunMaster.SetRoute` uses `m.Logger` (tun.go:93, 101) which is only guaranteed
non-nil after `Serve` ran (or if the caller set it) — in practice `Serve` runs
first. Tests set `Logger` explicitly (server_test.go:21, tun_test.go:19).

## Change hazards (MOST IMPORTANT)

1. **Forgetting `closed()` leaks tracking and blocks graceful Shutdown.** A
   matching handler that never calls `closed()` keeps its conn in `s.conns`
   forever. `Shutdown` then waits the full `ctx` deadline and force-closes
   (server.go:240-256). `Close` will still close it immediately. If you add a new
   handler, ensure `closed()` is called on every logical-completion path exactly
   once. `TunMaster` does this for tunnels (tun.go:99-100); custom handlers must
   too.

2. **Double-calling `closed()` deadlocks a goroutine.** The `closeCooldown`
   channel holds exactly one value (server.go:156); a second `closed()` blocks
   on `<-closeCooldown` forever (server.go:138). Never call `closed()` twice. Be
   careful if refactoring `Relay`/`TunMaster` to not invoke `closed()` from
   multiple paths.

3. **Handlers must decide match quickly and not block the route goroutine on
   decline.** Each conn is handled in its own goroutine (server.go:68), but
   handlers are tried **sequentially within that goroutine** (server.go:132-145).
   A slow/blocking handler delays trying subsequent routes for that conn and can
   pile up goroutines under load. Matching should be early (server.go:25). For
   long-lived work, a matched handler should return promptly and do the work in
   its own goroutine (as `TunMaster` does, tun.go:98) — do NOT block inside the
   handler return path.

4. **A matched handler that wraps but returns nil closer loses the wrapper on
   force-close.** If a handler wraps `conn` (TLS/obfuscation) but returns
   `(true, nil)`, the server tracks the **original** conn (server.go:146-149) and
   force-close will close the raw conn, not the wrapper. Return the wrapper as
   `wrappedConn` so `Close`/`Shutdown` close the right thing.

5. **Copy-on-write assumption: never mutate a stored route slice in place.** All
   readers rely on stored slices being immutable (server.go:126 reads without
   lock). If you add a writer, it MUST build a new slice and `Store` it under
   `routesMu` (server.go:80-98, 110-117). Appending to a loaded slice, or
   mutating an element, would corrupt concurrent reads. Also: `SetRoute` does NOT
   close existing conns from a replaced handler (server.go:73-74) — replacing a
   route is not a way to tear down its live conns.

6. **`Relay` buffer size vs frame/packet size.** With `BufferSize == 0` the
   relay uses 32 KiB (tun.go:18, 49-53). For datagram-preserving paths
   (UDP-over-stream via `FrameConn`), the buffer must be >= the largest datagram
   (up to `MaxPacketSize` 65535, packet.go:4) or a single read may not capture a
   whole datagram. Integration tests use 64 KiB (tun_udp_tcp_int_test.go:109).
   Changing the default or the field semantics can silently corrupt datagram
   tunnels. Note the CLI does NOT set `BufferSize` (cli/internal/tun.go:83), so
   it relies on the 32 KiB default — adequate for byte streams, but
   under-sized for max-size framed datagrams (a latent edge case).

7. **`Tun.Close()` has no nil-guard on `Conn`/`Peer`.** It dereferences both
   (tun.go:65, 69). `Relay` guards nil endpoints (tun.go:24-26) and the
   `TunMaster` logging dereferences `RemoteAddr()` (tun.go:94-95) — so an
   improperly constructed `Tun` (nil endpoint) returned as matched will panic in
   the route goroutine / on force-close. Keep both endpoints non-nil on match.

8. **Goroutine leaks on partial close are mostly prevented but order-sensitive.**
   `Relay` returns only after both directions finish (tun.go:37-38); the
   `defer t.Close()` in the first-finishing `halfCopy` unblocks the other
   (tun.go:52, 61-74). If you change `Relay` to not close both ends when one
   side ends, the surviving copy goroutine (and thus `Relay`, `closed()`, and the
   tracked conn) can leak. The `closing`-aware error suppression (tun.go:54-57)
   depends on `Close` having flipped `closing` before the second copy errors.

9. **Accept loop never returns on non-closing errors (no backoff).** Unlike
   `net/http`, `Serve` logs and continues on accept errors (server.go:64-66). A
   listener stuck returning errors busy-loops. If you swap in a listener with
   different error semantics, account for this.

10. **`closing` is one-shot for both Close and Shutdown.** The first of the two
    wins; the other returns `nil` immediately (server.go:185, 219). Calling
    `Shutdown` after `Close` (or vice versa) is a silent no-op — don't rely on
    Shutdown's graceful drain if Close already ran.

11. **`TunMaster.SetRoute` shadows `Server.SetRoute`.** Because `TunMaster`
    embeds `Server[ID]` (tun.go:81) and defines its own `SetRoute` with a
    `TunHandler` signature (tun.go:86), calling `m.SetRoute(...)` uses the
    tunnel version. The embedded raw `Server.SetRoute` is still reachable as
    `m.Server.SetRoute(...)`. Mixing the two on one `TunMaster` is possible and
    would register raw handlers alongside tunnel handlers — likely unintended.

12. **`Logger` lazy-init races / nil deref outside Serve/Relay.** Setting
    `Logger` after `Serve` has started races with the lazy assignment
    (server.go:50-52, not under lock). Always set `Logger` before `Serve`.
    Paths that use the logger before `Serve`/`Relay` ran can nil-panic
    (see Dependencies > Logger fallback).

## Tests covering this

- `TestRouteReplacement` (server_test.go:16-89): hot-swap a route under the same
  ID mid-serve; verifies new conns use the new handler (`h1`→`h2`) and `Close`
  makes `Serve` return `ErrServerClosed`.
- `TestRemoveRouteThenUnhandled` (server_test.go:91-152): add then remove a
  route; verifies an unhandled conn is dropped (read errors) and a DEBUG drop
  log is emitted (accepts either "unhandled..." or "no routes..." wording,
  server_test.go:132-134).
- `TestCloseClosesActiveConnections` (server_test.go:154-207): a matched conn
  left open is force-closed by `Close`.
- `TestConcurrentSetRouteNoLoss` (server_test.go:209-262): 100 concurrent
  `SetRoute` then 100 `RemoveRoute` — exercises copy-on-write under race; only
  checks no panic + graceful drop (does NOT assert final route contents — see
  gaps).
- `TestShutdownGraceful` (server_test.go:264-321): conn drains before deadline;
  `Shutdown` returns nil.
- `TestShutdownTimeoutForcesClose` (server_test.go:323-395): handler never calls
  `closed()`; `Shutdown` waits ~timeout, returns `context.DeadlineExceeded`,
  force-closes the conn.
- `TestTunRelayBidirectional` (tun_test.go:14-94): both directions copy; graceful
  `Shutdown` after closing both ends.
- `TestTunShutdownTimeoutForcesClose` (tun_test.go:96-174): active relay,
  forced `Shutdown` (`DeadlineExceeded`), both ends error afterward.
- `TestInt_UDP_over_TCP_TunMasters` (tun_udp_tcp_int_test.go:80-179): two
  `TunMaster`s bridging UDP<->framed stream end to end, 64 KiB buffers.
- `TestInt_TunMasterRouting_PlainAndTLS` (tun_udp_tcp_int_test.go:185-334):
  single listener, two routes (TLS-first then plain fallback) selected by
  `conn.(interface{ ConnectionState() tls.ConnectionState })`, exercising the
  decline→next-route path and ordering.

**Gaps / not directly tested:**
- Double-call of `closed()` (hazard #2) — no test; relies on contract.
- `Tun.Close()` / `TunMaster` logging on nil `Conn`/`Peer` (hazard #7) — no
  test.
- Accept-error continue/no-backoff path (server.go:64-66) — no direct test
  (chanListener returns `net.ErrClosed` only on Close, which is the closing
  path, tun_udp_tcp_int_test.go:35-42).
- Multiple concurrent listeners on one `Server` (`listenerGroup` > 1) — not
  exercised; tests use one listener each.
- `TestConcurrentSetRouteNoLoss` does not assert the resulting route set, only
  absence of panic (server_test.go:231-236 comments acknowledge routes are
  inspected only indirectly).
- Logger lazy-init race (hazard #12) — not exercised (tests always preset
  Logger).

## Related docs

- `docs/internals/pipeline.md` — (planned; not yet present) overall conn
  pipeline / wrappers.
- `docs/internals/mux.md` — (planned; not yet present) multiplexing layer.
- `docs/internals/stream-transforms.md` — (planned; not yet present) FrameConn,
  BufferedConn, and other byte transforms a handler may wrap.
- `docs/internals/drivers-proto.md` — (planned; not yet present) proto drivers
  (aesgcm, dnst, etc.).
- `docs/internals/modules-cli.md` — (planned; not yet present) CLI `tun` command
  wiring (cli/internal/tun.go).

> Currently the only existing sibling internals doc is
> `docs/internals/mux-tag-poll.md`; the five files above were not present at the
> time of writing.
