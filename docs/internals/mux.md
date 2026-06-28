# Mux & MuxClient

> Source of truth: `mux.go`, `mux_client.go` (package `netx`, module root).
> Code is cited by **symbol + file**, not line number, so refs survive refactors.

> [!IMPORTANT]
> **The two halves are NOT symmetric, contrary to the older guide.**
>
> - **`Mux` (server side) returns a `TaggedConn`, not a `net.Conn`** — it is a
>   concurrent *fan-in/fan-out tag router*, not an EOF-cycling single connection.
>   Every accepted underlying conn stays open and is read concurrently; the source
>   conn is carried as the read *tag* and is the routing key for `WriteTagged`
>   (`mux` struct + `WriteTagged` in `mux.go`; `TaggedConn` in `tagged_conn.go`).
> - **`MuxClient` (client side) returns a `net.Conn`** — it *is* the lazy,
>   EOF-cycling, single-most-recent-conn adapter (`NewMuxClient` in `mux_client.go`).
>
> The repo-level doc `docs/mux-tag-poll.md` is **stale for Mux**: it describes
> `Mux` as `NewMux(ln) net.Conn` that EOF-cycles to the next connection, and lists
> MuxClient options `WithMuxClientLocalAddr`/`WithMuxClientRemoteAddr` that **do not
> exist**. Treat *this* doc as authoritative over that one for Mux internals.

---

## Purpose & role

Both adapters bridge a connection-oriented transport up to layers (dnst / demux /
poll) that want a single logical endpoint. The package doc-comments at the top of
`mux.go` / `mux_client.go` cover the prose; the short version:

- **`Mux`** wraps a `net.Listener` and presents it as one `TaggedConn`. It services
  *all* accepted conns concurrently and routes each write back to the exact conn a
  given read came from — this is what lets a DNST server reply on the same TCP/UDP
  connection that delivered the query.
- **`MuxClient`** wraps a `Dialer` (`func() (net.Conn, error)`) as one
  auto-redialing `net.Conn`, giving the illusion of one persistent connection over
  a transport made of short-lived connections (per-query UDP/DNS round-trips).

Both register under the driver name `mux` (`init` in `mux.go`): the listener
wrapper produces a `TaggedConn` via `ListenerToTagged`; the dialer wrapper produces
a `net.Conn` via `DialerToConn`.

---

## Public API (exported symbols + options)

### Server: `Mux`

| Symbol | Contract |
|---|---|
| `NewMux(ln net.Listener, opts ...MuxOption) TaggedConn` | Wraps a listener as a `TaggedConn`. Spawns the accept-loop goroutine immediately. Defaults: logger = `slog.Default()`, read queue = **64**. |
| `MuxOption` | Functional option applied to the internal `*mux`. |
| `WithMuxReadQueue(size uint16) MuxOption` | Replaces the shared read queue with a buffered channel of the given size. ⚠️ Its doc-comment says "Default is 128" but `NewMux` actually uses **64** — the comment is **stale** and is deliberately not being fixed; trust the constructor. |
| `WithMuxLogger(logger Logger) MuxOption` | Sets the internal logger. |
| `TaggedConn` (returned interface, `tagged_conn.go`) | `ReadTagged([]byte, *any)`, `WriteTagged([]byte, any)`, plus `Close`/`LocalAddr`/`RemoteAddr`/`SetDeadline`/`SetReadDeadline`/`SetWriteDeadline`. |

`Mux` method contracts (`mux.go`):

- `ReadTagged(b, tag)`: returns the next chunk from any conn and sets `*tag` to the
  source `net.Conn`. Drains a pending partial chunk first; honors the mux-level read
  deadline; returns `net.ErrClosed` after close, `os.ErrDeadlineExceeded` on timeout.
- `WriteTagged(b, tag)`: `tag` must be a non-nil `net.Conn` or it errors; otherwise
  calls `conn.Write(b)` directly (no mux lock).
- `Close()`: idempotent; closes `doneCh` and the listener.
- `SetDeadline`/`SetReadDeadline`/`SetWriteDeadline`: store the deadline and fan it
  out to all currently-tracked conns.
- `LocalAddr()` = listener address; `RemoteAddr()` = a virtual `virtual://mux` addr
  (`muxVirtualAddr` in `mux.go`).

### Client: `MuxClient`

| Symbol | Contract |
|---|---|
| `Dialer = func() (net.Conn, error)` | Returns a fresh `net.Conn` per call. Type **alias** (`=`), so callers can pass a plain func. |
| `NewMuxClient(dial Dialer, opts ...MuxClientOption) net.Conn` | Wraps the dialer as a `net.Conn`. **Does not dial eagerly** — first dial happens on the first Read/Write. Default logger = `slog.Default()`. |
| `MuxClientOption` | Functional option applied to `*muxClient`. |
| `WithMuxClientLogger(logger Logger) MuxClientOption` | Sets the internal logger. **The only client option that exists.** |

`MuxClient` method contracts (`mux_client.go`):

- `Read(b)`: ensures a conn (dialing if needed), reads, and on `io.EOF` closes+clears
  the current conn and **redials transparently** to satisfy the read (loops).
  Returns data-with-EOF as `(n, nil)` when `n>0`.
- `Write(b)`: ensures a conn and writes to it. **No redial on write error** (single
  attempt; see hazards).
- `Close()`: idempotent; closes the current conn (if any) and blocks further dialing.
- `SetDeadline`/`SetReadDeadline`/`SetWriteDeadline`: store the deadline and apply it
  to the current conn if one exists.
- `LocalAddr()` and `RemoteAddr()` **both** return the virtual `virtual://mux` addr —
  the client never exposes the real socket addrs.

> The `mux` driver never wires up `WithMuxClientLogger` — `DialerToConn` calls
> `NewMuxClient(d)` with no options. Likewise it never wires `WithMuxLogger`; only
> `WithMuxReadQueue` is reachable via the URI param `rq` (`init` in `mux.go`).

### Driver registration & params (`init` in `mux.go`)

The `mux` driver self-registers in the **root module** via `init()` (there is no
separate `drivers/mux` module), so it is always available without a blank import —
useful context if you add a sibling core transform.

- `rq=<uint16>` → `WithMuxReadQueue`. **Listener-only**; on a dialer it is a hard
  error (`mux: readq parameter is only valid for listeners`). This is the *only* way
  to retune the shared read queue from a URI.
- Any other key → error `uri: unknown mux parameter`. The client side takes no params.

---

## Internal design & invariants

### `mux` (server) — concurrent tag router

Struct (`mux` in `mux.go`):

- `listener net.Listener`, `closed atomic.Bool`.
- `doneCh chan struct{}` — broadcast-closed on `Close` to unblock readers/senders.
- `rQueue chan muxPacket` — shared fan-in queue; each per-conn goroutine pushes
  `{data, conn}` (`muxPacket` + `readConn`).
- `rMu` — serializes `ReadTagged` for the pending-partial-chunk buffer
  (`pendingData`, `pendingConn`).
- `connMu` + `conns map[net.Conn]struct{}` — the live-connection set.
- `deadlineMu` + `readDeadline`/`writeDeadline` — stored mux-level deadlines for
  replay onto newly accepted conns.

Goroutine tree:

1. `NewMux` spawns `acceptLoop`.
2. `acceptLoop` loops `listener.Accept()`. On a new conn it **replays the stored
   deadlines** onto it, adds it to `conns` under `connMu`, and spawns one `readConn`
   goroutine. On accept error it logs and **only stops if `closed` is set**;
   otherwise it `continue`s — transient accept errors do not kill the mux.
3. `readConn` reads into a `MaxPacketSize` (65535, `packet.go`) buffer, copies each
   chunk, and pushes `{data, conn}` to `rQueue` or aborts on `doneCh`. It exits on
   *any* read error including EOF, and on exit closes the conn and removes it from
   `conns`.

Invariants:

- One `readConn` goroutine per live conn; the conn is in `conns` for exactly that
  goroutine's lifetime (added in `acceptLoop`, deleted in `readConn`'s `defer`).
- `pendingData`/`pendingConn` is touched only under `rMu`; a short read leaves the
  remainder buffered for the next `ReadTagged` (drained at the top of `ReadTagged`).
- The write path does **not** consult `conns` — `WriteTagged` writes straight to the
  tagged `net.Conn`. A write can therefore target a conn whose `readConn` goroutine
  has already exited; the underlying `Write` then fails normally. No membership check.

### `muxClient` — EOF→redial state machine

Struct (`muxClient` in `mux_client.go`):

- `dial Dialer`, `closed atomic.Bool`.
- `rMu` / `wMu` — separate mutexes serializing the read and write paths independently.
- `connMu sync.RWMutex` + `current net.Conn` — the single current-connection pointer;
  `nil` means "not dialed / needs redial".
- `deadlineMu` + `readDeadline`/`writeDeadline` — stored for replay onto a fresh dial.

State machine:

- **Lazy first dial**: `current` starts `nil`; `ensureConn` dials on first use,
  double-checking under the write lock to avoid duplicate dials. On a fresh dial it
  **replays stored deadlines** then publishes `current`.
- **EOF cycle (read only)**: in `Read`, `io.EOF` triggers `replaceCurrent(conn)`,
  which closes and clears `current` **only if it is still the same conn** (the
  `if c.current == old` identity guard) — preventing it from clobbering a conn another
  path already replaced. The read loop then `continue`s and `ensureConn` redials.
- **Data-with-EOF**: if `n>0` and `err==io.EOF`, `Read` returns `(n, nil)` *and*
  retires the conn so the next read redials.

Invariant: at most one `current` conn at a time; the EOF→retire→redial transition is
the only way `current` changes other than first dial and `Close`.

### Deadline storage/replay (both)

The mux-level deadline is the source of truth, stored under `deadlineMu`, and
**replayed onto every new underlying conn** at creation:

- **Server**: replayed in `acceptLoop` per accepted conn; also fanned out to all
  existing conns when `SetXDeadline` is called. Additionally, `Mux` enforces the
  *read* deadline itself with a timer in `ReadTagged`, so the deadline works even with
  **zero** connections (pinned by `TestMux_Deadlines`).
- **Client**: replayed in `ensureConn` per dial; applied to `current` in
  `SetXDeadline` only if a conn exists. The client has **no** internal timer — the
  deadline is enforced solely by the underlying conn. A deadline is an **absolute
  time**, so after a redial the remaining budget is whatever is left until that
  instant, which may already be in the past (immediate timeout). It is *not* a fresh
  per-dial timeout.

---

## Concurrency model

### Server (`mux`)

- **Read path**: per-conn `readConn` goroutines (producers) → `rQueue` → `ReadTagged`
  (consumers serialized by `rMu`). Concurrent `ReadTagged` callers are allowed but
  serialized.
- **Write path**: `WriteTagged` takes **no mux lock** — it calls `conn.Write`
  directly. Concurrency safety is delegated entirely to the underlying conn. Writes to
  *different* tags run fully in parallel; ⚠️ two goroutines holding the *same* tag have
  no serialization and can interleave on one fd (the tag is an exported opaque value
  callers may stash — do not share one tag across concurrent `WriteTagged`).
- **Backpressure**: if `rQueue` (default cap **64**, shared across *all* conns) fills,
  `readConn` blocks in the channel send until a consumer drains it or `doneCh` fires.
  Reads are never dropped. A slow `ReadTagged` consumer therefore stalls *all* conns'
  reads (head-of-line). Contrast Demux, which drops on full per-session queues.
- Lock map: `rMu` (pending buffer), `connMu` (`conns` map), `deadlineMu` (deadline
  fields), `rQueue`/`doneCh` channels, `closed` atomic — see the `mux` struct.

### Client (`muxClient`)

- Read and write paths use **separate** mutexes (`rMu`, `wMu`), so a concurrent Read
  and Write can run at once, each reading `current` under `connMu.RLock`. `ensureConn`
  upgrades to `connMu.Lock` only to dial.
- **Cycle race**: if Read hits EOF and redials while Write is mid-flight, both go
  through `ensureConn`/`replaceCurrent` under `connMu`. The `if c.current == old`
  guard prevents a late EOF on an already-replaced conn from closing the *new* conn.
  But `Write` captures `conn` from `ensureConn` and writes outside `connMu`, so it may
  write to a conn Read just retired/closed → the write fails normally and is **not**
  retried (single-attempt write).

---

## Lifecycle & ownership

### Who owns what

- **`Mux` owns the listener and every accepted conn.** `Close` closes the listener;
  `acceptLoop`'s `defer` closes all live conns and nils `conns`; `readConn` closes its
  own conn on exit. The caller passes ownership of `ln` to `NewMux`.
- **`MuxClient` owns only the current conn.** `Close` closes `current`; retired conns
  are closed at the moment of cycling by `replaceCurrent`. The `Dialer` is owned by the
  caller; MuxClient never closes it (nothing to close).

### Close ordering (server)

1. `Close` CAS `closed` false→true (idempotent guard).
2. `close(doneCh)` — unblocks any `readConn` blocked on the `rQueue` send and any
   `ReadTagged` blocked on the select.
3. `listener.Close()` — makes `Accept` return an error; `acceptLoop` sees
   `closed==true` and returns, then its `defer` closes all conns and sets `conns=nil`.

> **`rQueue` is intentionally never closed** (see the comment in `acceptLoop`'s
> `defer`): `readConn` goroutines may still be sending to it, and sending on a closed
> channel panics. Termination of blocked readers/senders is via `doneCh`, not channel
> close. The `case pkt, ok := <-c.rQueue; if !ok` branch in `ReadTagged` is therefore
> effectively dead under normal operation but kept as a safety net.

### Close ordering (client)

1. `Close` CAS `closed` false→true.
2. Under `connMu.Lock`, close `current` and nil it. After this, `ensureConn` could
   still dial (it does not check `closed`), but Read/Write re-check `closed` after
   `ensureConn` and return `net.ErrClosed`, so a stray post-close dial is dropped.

### Double close

Both `Close` methods CAS on `closed` and return `nil` on the second call — no panic,
no double-close of underlying resources. Pinned by `TestMux_Close` /
`TestMuxClient_Close`.

---

## Error, EOF & deadline semantics

### Server (`Mux`)

- **Per-conn read errors (incl. EOF) end only that conn**, not the mux: `readConn`
  returns on any error and closes just that conn; other conns keep flowing. There is
  no "EOF cycles to next conn" concept on the server side — connections coexist.
- **Accept errors** are logged and ignored unless `closed` is set, in which case the
  loop stops. A persistently failing `Accept` that is *not* a close would spin the loop
  logging warnings — see hazards.
- **`ReadTagged` errors**: `net.ErrClosed` if closed or `doneCh` fired;
  `os.ErrDeadlineExceeded` on deadline (only the deadline case satisfies
  `net.Error.Timeout()`).
- **`WriteTagged` errors**: `net.ErrClosed` if closed; invalid-tag error if `tag` is
  nil or not a `net.Conn`; otherwise whatever `conn.Write` returns.

### Client (`MuxClient`)

- **EOF → cycle (Read only)**: `io.EOF` is the *only* error that triggers a redial,
  detected with `errors.Is(err, io.EOF)`.
- **Non-EOF read error → propagated** as-is (after a close re-check).
- **Dial error → propagated** raw, unless `closed` (then `net.ErrClosed`). Pinned by
  `TestMuxClient_DialError`.
- **Write error → propagated, NO redial.** Single attempt; asymmetric with Read.
- **Deadlines**: enforced only by the underlying conn (no internal timer); replayed
  on every dial; absolute time (see Deadline storage/replay above).

---

## Dependencies

**Depends on (within package `netx`):**
- `TaggedConn` interface (`tagged_conn.go`) — `Mux`'s return type/contract.
- `Logger` interface (`logger.go`) — both default to `slog.Default()`.
- `MaxPacketSize` = 65535 (`packet.go`) — server per-conn read buffer size (`readConn`).
- `Wrapper` struct + `Register`/`Driver` (`Wrapper` in `wrap.go`, `Register`/`Driver`
  in `driver.go`) — the `mux` driver registration in `init`.

**Depended on by:**
- **Driver registry / pipeline**: the `mux` driver feeds `ListenerToTagged` (server) /
  `DialerToConn` (client), dispatched by `Wrapper.Apply` (`wrap.go`).
- **DNST server**: `proto/dnst.NewTaggedServerConn` consumes a `TaggedConn` (a Mux) and
  **propagates the Mux tag end-to-end** — `taggedServerConn.ReadTagged` stores Mux's
  source-conn tag as `serverConnTagged.connTag` and `taggedServerConn.WriteTagged`
  passes it back (`proto/dnst/dnst_conn.go`). This is the routing chain that makes
  per-connection request/response work over DNST.
- **TaggedDemux**: `NewTaggedDemux(c TaggedConn, ...)` (`demux_tagged.go`) sits above a
  `Mux`/DNST `TaggedConn`, preserving tags through sessions.
- **DNST client**: `MuxClient`'s `net.Conn` is wrapped by `dnstproto.NewClientConn` (a
  `ConnToConn` wrapper in `drivers/dnst/dnst.go`).
- **Integration tests**: `proto/dnst/dnst_conn_int_test.go` builds the full server
  stack `Mux → NewTaggedServerConn → NewTaggedDemux`.

**External:** stdlib only (`net`, `sync`, `sync/atomic`, `time`, `context`, `errors`,
`io`, `os`, `log/slog`, `fmt`, `strconv`).

---

## Change hazards

Terse "what breaks if you touch X" list; the mechanism is in the sections above.

1. **Do not collapse `Mux` to a single "current conn".** It is a `TaggedConn` whose
   tag is the *source `net.Conn`*; DNST's tagged server and TaggedDemux route responses
   by that tag. Breaks concurrent request/response correctness (pinned by
   `TestMux_ConcurrentConnections`, `TestMux_RequestResponseAcrossConnections`).
2. **MuxClient cycles only on real `io.EOF`** (`errors.Is`). A transport that signals
   end-of-association with any other sentinel will surface as a hard error, not a
   redial. New transports must emit a true `io.EOF`. The server has no EOF-cycle logic.
3. **Neither write path redials or checks liveness.** Client `Write` is single-attempt;
   server `WriteTagged` writes to the tagged conn with no `conns` membership check.
   Callers expecting writes to self-heal like reads will see spurious failures right
   after a server-side close, and holding a tag across long gaps is unsafe.
4. **Same-tag concurrent writes are unserialized.** `WriteTagged` holds no mux lock, so
   two goroutines using the same tag can interleave on one fd. Keep one tag to one
   writer.
5. **Deadlines are absolute, not per-dial.** Server enforces the read deadline with its
   own timer *and* a per-conn fan-out — changing one must keep the other consistent.
   Client has no timer, and after a redial the replayed absolute deadline may already
   be in the past (immediate timeout). "Fresh timeout per dial" reasoning is wrong.
6. **`replaceCurrent`'s `if c.current == old` guard is load-bearing.** Dropping it, or
   closing `current` outside `connMu`, introduces a use-after-close / double-close race.
   Correctness also relies on Read/Write re-checking `closed` after `ensureConn` (which
   does not check `closed`); removing those re-checks leaks a post-`Close` dialed conn.
7. **Never close `rQueue`; coordinate shutdown via `doneCh` only.** Closing it panics
   any in-flight `readConn` send. The shared queue (default cap 64) means a stalled
   consumer head-of-line-blocks *all* conns.
8. **`acceptLoop` has no backoff.** A listener returning a permanent non-close error
   busy-loops logging warnings.

(The `WithMuxReadQueue` "Default is 128" doc-comment and `docs/mux-tag-poll.md`'s stale
Mux model are flagged in the top callout / Public API table — do not build on either.)

---

## Commands

Both files live in the root module, so run from the repo root:

```bash
go test -run TestMux ./...          # server-side mux (includes MuxClient by prefix)
go test -run 'TestMux_' ./...       # server-only
go test -run TestMuxClient ./...    # client-only
```

`task test` runs the whole suite across every workspace module (it iterates
`go list -m`); these `go test -run` invocations cover only the root module.

## Tests covering this

Load-bearing assertions to preserve:

- `TestMux_ConcurrentConnections` / `TestMux_RequestResponseAcrossConnections` — the
  core concurrency + tag-routing guarantee (each echo lands on its source conn).
- `TestMux_Deadlines` — read deadline times out with **zero conns** (mux-level timer);
  error satisfies `net.Error.Timeout()`.
- `TestMux_WriteInvalidTag` — nil / non-`net.Conn` tag errors.
- `TestMux_Close` / `TestMuxClient_Close` — post-close errors and no-panic double close.
- `TestMuxClient_MultipleConnections` — transparent redial across EOF-closed conns.
- `TestMuxClient_DialError` — dial error propagates through both Write and Read.

Untested paths (regressions here would not be caught): server `rQueue` backpressure /
slow-consumer stall, server accept-error continue loop, server deadline fan-out to
*existing* conns (only the zero-conn timer is tested), server partial-read pending
buffer, client write-error (no-redial) path, client cycle race, client
absolute-deadline-already-past after redial, and `rq` driver-param parsing (exercised
only indirectly via the pipeline).

---

## Related docs

- `docs/internals/pipeline.md` — how the `mux` `Wrapper`/`Register` plugs into the
  pipeline (driver resolution, type transitions).
- `docs/internals/demux.md` — TaggedDemux/Demux that sit above Mux and consume tags.
- `docs/internals/poll-tagged.md` — PollConn and the request/response→stream bridge on
  the client (above MuxClient).
- `docs/internals/stream-transforms.md` — frame/split/buffered transforms composed
  around these adapters.
- `docs/internals/server-tun.md` — server wiring that consumes the listener side.
- `docs/internals/drivers-proto.md` — DNST and other proto drivers that wrap the
  Mux/MuxClient endpoints.
- `docs/mux-tag-poll.md` — older architecture guide; **partially stale for Mux** (see
  the top callout).
