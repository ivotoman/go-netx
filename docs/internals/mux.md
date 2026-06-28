# Mux & MuxClient

> Source of truth: `mux.go`, `mux_client.go` (package `netx`, module root). All
> `file:line` references are against the tree at the time of writing. Verify line
> numbers if the files have since changed.

> [!IMPORTANT]
> **The two halves are NOT symmetric, contrary to the older guide.** The current
> code implements two structurally different things:
>
> - **`Mux` (server side) returns a `TaggedConn`, not a `net.Conn`** — it is a
>   concurrent *fan-in/fan-out tag router*, not an EOF-cycling single connection.
>   Every accepted underlying conn stays open and is read concurrently; the source
>   conn is carried as the read *tag* and is the routing key for `WriteTagged`
>   (`mux.go:122`, `mux.go:53`, `tagged_conn.go:11`).
> - **`MuxClient` (client side) returns a `net.Conn`** — it *is* the lazy,
>   EOF-cycling, single-most-recent-conn adapter (`mux_client.go:60`).
>
> The repo-level doc `docs/mux-tag-poll.md` (lines 31, 48–54) and several task
> framings describe `Mux` as `NewMux(ln) net.Conn` that "transitions to the next
> connection on EOF" with writes "to the most recently accepted connection". **That
> description is stale and does not match `mux.go`.** It also lists MuxClient
> options `WithMuxClientLocalAddr` / `WithMuxClientRemoteAddr` (lines 69–70) that
> **do not exist** in `mux_client.go`. Treat this doc as authoritative over that one
> for Mux internals; see [Change hazards](#change-hazards).

---

## Purpose & role

Both adapters bridge a connection-oriented transport (TCP, UDP/pion, etc.) up to
layers (dnst / demux / poll) that want a single logical endpoint.

- **`Mux`** wraps a `net.Listener` and presents it as one `TaggedConn`
  (`mux.go:1-13`, `mux.go:122`). It services *all* accepted connections
  concurrently, multiplexing their reads into a shared queue, and routes each
  write back to the exact connection a given read came from. This is what lets a
  DNST server reply to the same TCP/UDP connection that delivered the query
  (`mux.go:9-12`).
- **`MuxClient`** wraps a `Dialer` (`func() (net.Conn, error)`, `mux_client.go:28`)
  and presents it as one auto-redialing `net.Conn` (`mux_client.go:1-11`,
  `mux_client.go:60`). It gives the illusion of one persistent connection over a
  transport made of short-lived connections (e.g. per-query UDP/DNS round-trips).

Both register under the driver name `mux` (`mux.go:31`): as a listener wrapper it
produces a `TaggedConn` via `ListenerToTagged`; as a dialer wrapper it produces a
`net.Conn` via `DialerToConn` (`mux.go:48-65`).

---

## Public API (exported symbols + options)

### Server: `Mux`

| Symbol | file:line | Contract |
|---|---|---|
| `NewMux(ln net.Listener, opts ...MuxOption) TaggedConn` | `mux.go:122` | Wraps a listener as a `TaggedConn`. Starts the accept loop goroutine immediately (`mux.go:133`). Defaults: logger = `slog.Default()`, read queue = 64 (`mux.go:123-128`). |
| `MuxOption` | `mux.go:94` | Functional option applied to the internal `*mux` (`mux.go:130-132`). |
| `WithMuxReadQueue(size uint16) MuxOption` | `mux.go:98` | Replaces the shared read queue with a buffered channel of the given size. Doc-comment says "Default is 128" but constructor default is actually **64** (`mux.go:127`) — comment is wrong (`mux.go:96-97`). |
| `WithMuxLogger(logger Logger) MuxOption` | `mux.go:105` | Sets the internal logger. |
| `TaggedConn` (returned interface) | `tagged_conn.go:11` | `ReadTagged([]byte, *any)`, `WriteTagged([]byte, any)`, plus `Close`/`LocalAddr`/`RemoteAddr`/`SetDeadline`/`SetReadDeadline`/`SetWriteDeadline`. |

`Mux` method contracts:

- `ReadTagged(b, tag) (int, error)` (`mux.go:219`): returns the next chunk from any
  connection; sets `*tag` to the source `net.Conn`. Drains a pending partial chunk
  first; honors the mux-level read deadline; returns `net.ErrClosed` after close,
  `os.ErrDeadlineExceeded` on timeout.
- `WriteTagged(b, tag) (int, error)` (`mux.go:275`): `tag` must be a non-nil
  `net.Conn` or it returns an error (`mux.go:279-282`); otherwise calls
  `conn.Write(b)` directly (`mux.go:283`).
- `Close()` (`mux.go:286`): idempotent; closes `doneCh` and the listener.
- `SetDeadline`/`SetReadDeadline`/`SetWriteDeadline` (`mux.go:294-331`): store the
  deadline and fan it out to all currently-tracked conns.
- `LocalAddr()` = listener address; `RemoteAddr()` = a virtual `"virtual://mux"`
  addr (`mux.go:333-344`).

### Client: `MuxClient`

| Symbol | file:line | Contract |
|---|---|---|
| `Dialer = func() (net.Conn, error)` | `mux_client.go:28` | Returns a fresh `net.Conn` per call. Type alias (`=`), so callers can pass a plain func. |
| `NewMuxClient(dial Dialer, opts ...MuxClientOption) net.Conn` | `mux_client.go:60` | Wraps the dialer as a `net.Conn`. **Does not dial eagerly** — first dial happens on the first Read/Write (`mux_client.go:57`, `mux_client.go:131`). Default logger = `slog.Default()`. |
| `MuxClientOption` | `mux_client.go:46` | Functional option applied to `*muxClient`. |
| `WithMuxClientLogger(logger Logger) MuxClientOption` | `mux_client.go:49` | Sets the internal logger. **The only client option that exists.** |

`MuxClient` method contracts:

- `Read(b)` (`mux_client.go:122`): ensures a conn (dialing if needed), reads,
  and on `io.EOF` closes+clears the current conn and **redials transparently**
  to satisfy the read (loop). Returns data-with-EOF as `(n, nil)` when `n>0`.
- `Write(b)` (`mux_client.go:157`): ensures a conn and writes to it. **No redial
  on write error** (single attempt; see hazards).
- `Close()` (`mux_client.go:176`): idempotent; closes the current conn (if any)
  and blocks further dialing.
- `SetDeadline`/`SetReadDeadline`/`SetWriteDeadline` (`mux_client.go:191-229`):
  store the deadline and apply it to the current conn if one exists.
- `LocalAddr()` and `RemoteAddr()` **both** return the virtual `"virtual://mux"`
  addr — the client never exposes the real socket addrs (`mux_client.go:231-232`).

> The `mux` driver never wires up `WithMuxClientLogger` — `DialerToConn` calls
> `NewMuxClient(d)` with no options (`mux.go:62-64`). Likewise it never wires
> `WithMuxLogger`; only `WithMuxReadQueue` is reachable via the URI param `rq`
> (`mux.go:35-43`).

### Driver params (`mux.go:31-66`)

- `rq=<uint16>` → `WithMuxReadQueue`. **Listener-only**; using it on a dialer is a
  hard error (`mux.go:36-38`).
- Any other key → error `uri: unknown mux parameter` (`mux.go:45`). The client
  side has *no* params.

---

## Internal design & invariants

### `mux` (server) — concurrent tag router

Struct (`mux.go:74-92`):

- `listener net.Listener`, `closed atomic.Bool`.
- `doneCh chan struct{}` — broadcast-closed on `Close` to unblock readers/senders.
- `rQueue chan muxPacket` — shared fan-in queue; each per-conn goroutine pushes
  `{data, conn}` (`mux.go:69-72`, `mux.go:206`).
- `rMu` — serializes `ReadTagged` for the pending-partial-chunk buffer
  (`pendingData`, `pendingConn`) (`mux.go:82-84`).
- `connMu` + `conns map[net.Conn]struct{}` — the live-connection set
  (`mux.go:86-87`).
- `deadlineMu` + `readDeadline`/`writeDeadline` — stored mux-level deadlines for
  replay onto newly accepted conns (`mux.go:89-91`).

Lifecycle of the goroutine tree:

1. `NewMux` spawns `acceptLoop` (`mux.go:133`).
2. `acceptLoop` (`mux.go:139`): loops `listener.Accept()`. On a new conn it
   **replays the stored deadlines** onto it (`mux.go:163-171`), adds it to `conns`
   under `connMu` (`mux.go:173-180`), and spawns one `readConn` goroutine per conn
   (`mux.go:182`). On accept error it logs, and **only stops if `closed` is set**;
   otherwise it `continue`s (`mux.go:153-161`) — i.e. transient accept errors do
   not kill the mux.
3. `readConn` (`mux.go:188`): reads into a `MaxPacketSize` (65535) buffer
   (`mux.go:199`, `packet.go:4`), copies each chunk and pushes `{data, conn}` to
   `rQueue` or aborts on `doneCh` (`mux.go:202-211`). It exits on *any* read error
   including EOF (`mux.go:212-215`), and on exit closes the conn and removes it
   from `conns` (`mux.go:190-197`).

Invariants:

- One `readConn` goroutine per live conn; the conn is in `conns` for exactly the
  goroutine's lifetime (added at `mux.go:179`, deleted at `mux.go:194`).
- `pendingData`/`pendingConn` is only touched under `rMu`; a short read leaves the
  remainder buffered for the next `ReadTagged` with the same tag (`mux.go:263-266`,
  drained at `mux.go:224-233`).
- The write path does **not** consult `conns` — it writes straight to the tagged
  `net.Conn` (`mux.go:283`). So a write can target a conn whose `readConn`
  goroutine has already exited (the tag is a stale `net.Conn`); the underlying
  `Write` then fails normally. There is no membership check.

### `muxClient` — EOF→redial state machine

Struct (`mux_client.go:30-44`):

- `dial Dialer`, `closed atomic.Bool`.
- `rMu` / `wMu` — separate mutexes serializing the read and write paths
  (independently).
- `connMu sync.RWMutex` + `current net.Conn` — the single current connection
  pointer; `nil` means "not dialed / needs redial".
- `deadlineMu` + `readDeadline`/`writeDeadline` — stored for replay onto a freshly
  dialed conn.

State machine:

- **Lazy first dial**: `current` starts `nil`; `ensureConn` (`mux_client.go:73`)
  dials on first use, double-checking under the write lock to avoid duplicate dials
  (`mux_client.go:74-87`). On a fresh dial it **replays stored deadlines**
  (`mux_client.go:96-104`) then publishes `current` (`mux_client.go:106`).
- **EOF cycle (read only)**: in `Read`, `io.EOF` triggers `replaceCurrent(conn)`
  (`mux_client.go:142`, `mux_client.go:147`). `replaceCurrent` (`mux_client.go:112`)
  closes and clears `current` **only if it is still the same conn** (`mux_client.go:114-118`)
  — guarding against clobbering a conn another path already replaced. The read loop
  then `continue`s and `ensureConn` redials (`mux_client.go:148`).
- **Data-with-EOF**: if `n>0` and `err==io.EOF`, it returns `(n, nil)` *and*
  retires the conn so the next read redials (`mux_client.go:140-144`).

Invariant: at most one `current` conn at a time; the EOF→retire→redial transition
is the only way `current` changes other than first dial and `Close`.

### Deadline storage/replay (both)

The mux-level deadline is the source of truth, stored under `deadlineMu`, and
**replayed onto every new underlying conn** at creation:

- Server: replayed in `acceptLoop` per accepted conn (`mux.go:163-171`); also
  fanned out to all existing conns when `SetXDeadline` is called (`mux.go:301-331`).
  Additionally, `Mux` enforces the *read* deadline itself with a timer in
  `ReadTagged` (`mux.go:240-252`, `mux.go:268`) so the deadline works even with
  **zero** connections (pinned by `TestMux_Deadlines`).
- Client: replayed in `ensureConn` per dial (`mux_client.go:96-104`); applied to
  `current` when `SetXDeadline` is called and a conn exists (`mux_client.go:191-229`).
  The client has **no** internal deadline timer — the deadline is enforced solely
  by the underlying conn, so a deadline set with no conn yet only takes effect once
  a conn is dialed (then enforced by that conn; pinned by `TestMuxClient_Deadlines`,
  which dials via a write before reading).

---

## Concurrency model

### Server (`mux`)

| Lock / chan | Guards | file:line |
|---|---|---|
| `rMu` | `pendingData`/`pendingConn`; serializes `ReadTagged` callers | `mux.go:82`, `mux.go:220` |
| `connMu` | `conns` map mutations + iteration | `mux.go:86`, `mux.go:141`, `mux.go:173`, `mux.go:192`, `mux.go:306`, `mux.go:322` |
| `deadlineMu` | `readDeadline`/`writeDeadline` fields | `mux.go:89`, `mux.go:163`, `mux.go:241`, `mux.go:302`, `mux.go:318` |
| `rQueue` (chan) | fan-in of reads; backpressure when full | `mux.go:80`, `mux.go:206`, `mux.go:255` |
| `doneCh` (chan) | close broadcast to unblock senders/readers | `mux.go:79`, `mux.go:207`, `mux.go:270` |
| `closed` (atomic) | fast close check on read/write | `mux.go:77`, `mux.go:236`, `mux.go:276`, `mux.go:287` |

- **Read path**: per-conn `readConn` goroutines (producers) → `rQueue` →
  `ReadTagged` (single logical consumer, serialized by `rMu`). Multiple concurrent
  `ReadTagged` callers are allowed but serialized.
- **Write path**: `WriteTagged` takes **no mux lock** — it calls `conn.Write`
  directly (`mux.go:283`). Concurrency safety of the write is delegated entirely to
  the underlying conn. Multiple writes to *different* tags run fully in parallel.
- **Backpressure**: if `rQueue` fills, `readConn` blocks in the channel send until
  a consumer drains it or `doneCh` fires (`mux.go:205-210`). Reads are not dropped.
  (Contrast with Demux, which drops on full per-session queues — `docs/mux-tag-poll.md:129`.)

### Client (`muxClient`)

| Lock | Guards | file:line |
|---|---|---|
| `rMu` | serializes Read + its redial | `mux_client.go:35`, `mux_client.go:123` |
| `wMu` | serializes Write + its dial | `mux_client.go:36`, `mux_client.go:158` |
| `connMu` (RW) | `current` pointer | `mux_client.go:38`, `mux_client.go:74-119`, `mux_client.go:181` |
| `closed` (atomic) | fast close check | `mux_client.go:33`, `mux_client.go:127`, `mux_client.go:161`, `mux_client.go:177` |

- Read and write paths use **separate** mutexes, so a concurrent Read and Write
  can run at once (each on `current`, read under `connMu.RLock`). `ensureConn`
  upgrades to `connMu.Lock` only to dial.
- **Cycle race**: if Read hits EOF and redials while Write is mid-flight, both go
  through `ensureConn`/`replaceCurrent` under `connMu`. `replaceCurrent`'s
  `if c.current == old` check (`mux_client.go:114`) prevents a late EOF on an
  already-replaced conn from closing the *new* current conn. But Write captures
  `conn` from `ensureConn` and then writes outside `connMu` (`mux_client.go:165-173`):
  it may write to a conn that Read has just retired/closed → the write fails
  normally and is **not** retried (single-attempt write).

---

## Lifecycle & ownership

### Who owns what

- **`Mux` owns the listener and every accepted conn.** `Close` closes the listener
  (`mux.go:291`); `acceptLoop`'s deferred cleanup closes all live conns and nils
  `conns` (`mux.go:140-150`); `readConn` closes its own conn on exit
  (`mux.go:191`). The caller passes ownership of `ln` to `NewMux`.
- **`MuxClient` owns only the current conn.** `Close` closes `current`
  (`mux_client.go:183-186`). Retired conns are closed at the moment of cycling by
  `replaceCurrent` (`mux_client.go:116`). The `Dialer` itself is owned by the
  caller; MuxClient never "closes" the dialer (there is nothing to close).

### Close ordering (server)

1. `Close` CAS `closed` false→true (idempotent guard) (`mux.go:287-289`).
2. `close(doneCh)` — unblocks any `readConn` blocked on the `rQueue` send and any
   `ReadTagged` blocked on the select (`mux.go:290`, consumed at `mux.go:207`,
   `mux.go:270`).
3. `listener.Close()` — makes `Accept` return an error; `acceptLoop` sees
   `closed==true` and returns (`mux.go:153-159`), then its `defer` closes all conns
   and sets `conns=nil` (`mux.go:140-150`).

> **`rQueue` is intentionally never closed** (`mux.go:147-149`): `readConn`
> goroutines may still be sending to it, and sending on a closed channel panics.
> Termination of blocked readers/senders is via `doneCh`, not channel close. The
> `case pkt, ok := <-c.rQueue; if !ok` branch in `ReadTagged` (`mux.go:255-258`) is
> therefore effectively dead under normal operation but is kept as a safety net.

### Close ordering (client)

1. `Close` CAS `closed` false→true (`mux_client.go:177-179`).
2. Under `connMu.Lock`, close `current` and nil it (`mux_client.go:181-188`).
   After this, `ensureConn` could still dial (it does not check `closed`), but the
   Read/Write entry points re-check `closed` after `ensureConn` and return
   `net.ErrClosed`, so a stray post-close dial is dropped on the floor (see hazards)
   (`mux_client.go:127-137`, `mux_client.go:161-170`).

### Double close

Both `Close` methods CAS on `closed` and return `nil` on the second call — no
panic, no double-close of underlying resources (`mux.go:287-288`,
`mux_client.go:177-178`). Pinned by `TestMux_Close` (`mux_test.go:288-290`) and
`TestMuxClient_Close` (`mux_client_test.go:176-178`).

---

## Error, EOF & deadline semantics

### Server (`Mux`)

- **Per-conn read errors (incl. EOF) end only that conn**, not the mux:
  `readConn` returns on any error (`mux.go:212-215`) and closes just that conn.
  Other conns keep flowing. There is no "EOF cycles to next conn" concept on the
  server side — connections coexist.
- **Accept errors** are logged and ignored unless `closed` is set, in which case
  the loop stops (`mux.go:153-161`). A persistently failing `Accept` (e.g. fatal
  listener error that is *not* a close) would spin the loop logging warnings — see
  hazards.
- **`ReadTagged` errors**: `net.ErrClosed` if closed or `doneCh` fired
  (`mux.go:236-237`, `mux.go:270-271`); `os.ErrDeadlineExceeded` on deadline
  (`mux.go:247`, `mux.go:269`). Both close and timeout satisfy `net.Error`'s
  `Timeout()` only for the deadline case.
- **`WriteTagged` errors**: `net.ErrClosed` if closed (`mux.go:276`); invalid-tag
  error if `tag` is nil or not a `net.Conn` (`mux.go:279-282`); otherwise whatever
  `conn.Write` returns.

### Client (`MuxClient`)

- **EOF → cycle (Read only)**: `io.EOF` is the *only* error that triggers a redial.
  Detected with `errors.Is(err, io.EOF)` (`mux_client.go:141`, `mux_client.go:146`).
- **Non-EOF read error → propagated** as-is (`mux_client.go:153`) after a close
  re-check.
- **Dial error → propagated** (wrapped only by `errors.Is` semantics; the raw error
  is returned), unless `closed` (then `net.ErrClosed`) (`mux_client.go:90-93`,
  `mux_client.go:132-137`, `mux_client.go:166-170`). Pinned by
  `TestMuxClient_DialError` (`mux_client_test.go:239-255`).
- **Write error → propagated, NO redial.** `Write` makes a single attempt; a broken
  current conn surfaces the write error to the caller (`mux_client.go:172-173`).
  Asymmetric with Read.
- **Deadlines**: enforced only by the underlying conn (no internal timer). Replayed
  on every dial. A deadline set before the first dial applies to the conn once
  dialed (`mux_client.go:96-104`).

---

## Dependencies

**Depends on (within package `netx`):**
- `TaggedConn` interface (`tagged_conn.go:11`) — `Mux`'s return type/contract.
- `Logger` interface (`logger.go:5`) — both use `slog.Default()` by default.
- `MaxPacketSize` = 65535 (`packet.go:4`) — server per-conn read buffer size
  (`mux.go:199`).
- `Wrapper` struct + `Register`/`Driver` (`wrap.go:119`, `driver.go:8-25`) — the
  `mux` driver registration in `init()` (`mux.go:30-67`).

**Depended on by:**
- **Driver registry / pipeline**: `mux` driver feeds `ListenerToTagged` (server) /
  `DialerToConn` (client) (`mux.go:53-65`, consumed by `wrap.go:170/219` and the
  pipeline machinery).
- **DNST server**: `proto/dnst.NewTaggedServerConn` consumes a `TaggedConn` (a Mux)
  and **propagates the Mux tag end-to-end** — it stores Mux's source-conn tag as
  `serverConnTagged.connTag` on read and passes it back on `WriteTagged`
  (`proto/dnst/dnst_conn.go:199`, `:247-248`, `:294`). This is the routing chain
  that makes per-connection request/response work over DNST.
- **TaggedDemux**: `NewTaggedDemux(c TaggedConn, ...)` (`demux_tagged.go:33`) sits
  above a `Mux`/DNST `TaggedConn`, preserving tags through sessions.
- **DNST client**: `MuxClient`'s `net.Conn` is wrapped by `dnst.NewClientConn`
  (a `ConnToConn` wrapper, `drivers/dnst/dnst.go:52`).
- **Integration tests**: `proto/dnst/dnst_conn_int_test.go:531,613,699` build the
  full server stack `Mux → NewTaggedServerConn → NewTaggedDemux`.

**External:** stdlib only — `net`, `sync`, `sync/atomic`, `time`, `context`,
`errors`, `io`, `os`, `log/slog`, `fmt`, `strconv` (`mux.go:17-28`,
`mux_client.go:15-24`).

---

## Change hazards

1. **`Mux` is a `TaggedConn`, not an EOF-cycling `net.Conn`. Do not "fix" it to
   match the older prose.** Downstream (DNST tagged server, TaggedDemux) depends on
   the tag being the *source `net.Conn`* so responses route back to the originating
   transport (`mux.go:206/261/283`, `proto/dnst/dnst_conn.go:247-248/294`). Collapsing
   Mux to a single "current conn" would break request/response correctness for
   concurrent connections (pinned by `TestMux_ConcurrentConnections`,
   `TestMux_RequestResponseAcrossConnections`).

2. **EOF-vs-real-error distinction is client-only and uses `errors.Is(err, io.EOF)`.**
   Only `io.EOF` cycles; any other error propagates (`mux_client.go:141-153`). If an
   underlying transport signals end-of-association with a non-EOF sentinel (e.g. a
   custom "connection reset" or `net.ErrClosed`), MuxClient will surface it as a
   hard error instead of redialing. Adding transports requires their EOF to be a
   real `io.EOF`. The server has no EOF-cycle logic at all — EOF just ends one conn.

3. **Write path has no redial and no membership/liveness check.**
   - Client `Write` makes one attempt; a stale `current` conn yields a write error
     to the caller, not a transparent redial (`mux_client.go:157-173`). If callers
     assume writes self-heal like reads, they will see spurious failures right after
     a server-side close. Asymmetry is intentional but easy to trip over.
   - Server `WriteTagged` writes to the tagged conn with no `conns`-membership check
     (`mux.go:275-284`); a write with a tag whose `readConn` already exited targets a
     closed/closing conn and fails. Holding tags across long gaps is unsafe.

4. **Deadline replay correctness differs between sides.**
   - Server enforces the read deadline with its *own* timer in `ReadTagged`
     (`mux.go:240-252`), so deadlines work with zero conns — but the per-conn
     `SetReadDeadline` fan-out (`mux.go:301-315`) and the timer are two separate
     mechanisms; a change to one must keep the other consistent. The stored deadline
     is also replayed on each *new* accepted conn (`mux.go:163-171`), so changing the
     storage field semantics affects future conns silently.
   - Client has **no** internal timer; a deadline set before the first dial does
     nothing until a conn exists (`mux_client.go:191-229`). Across an EOF redial,
     the *new* conn gets the stored deadline replayed (`mux_client.go:96-104`) — but
     note the deadline is an **absolute time**, so after a redial the remaining
     budget is whatever is left until that absolute instant, which may already be in
     the past (immediate timeout). Anyone reasoning about "fresh timeout per dial"
     is wrong.

5. **Thread-safety of the current-conn swap (client).** The `replaceCurrent`
   guard `if c.current == old` (`mux_client.go:114`) is load-bearing: it prevents a
   late EOF from one path closing a conn another path just installed. Any refactor
   that drops this identity check, or that closes `current` outside `connMu`, will
   introduce a use-after-close / double-close race. Note also `ensureConn` and
   `Close` both mutate `current` under `connMu`, but `ensureConn` does **not** check
   `closed` — correctness relies on the Read/Write callers re-checking `closed`
   after `ensureConn` (`mux_client.go:127-137`, `mux_client.go:161-170`). Removing
   those re-checks would let a post-`Close` dial leak an open conn.

6. **`rQueue` must never be closed; close coordination is via `doneCh` only**
   (`mux.go:147-149`). A well-meaning "close the channel on shutdown" change will
   panic any in-flight `readConn` send. Also, `rQueue` backpressure (default cap 64,
   `mux.go:127`) means a slow `ReadTagged` consumer stalls *all* conns' reads, not
   just one — a head-of-line blocking property downstream layers implicitly rely on
   for ordering but which can deadlock if a consumer stops reading while conns keep
   producing. The "Default is 128" doc-comment is wrong; the real default is 64.

7. **Accept-error loop can spin.** `acceptLoop` `continue`s on any non-close accept
   error (`mux.go:153-161`). A listener that returns a permanent error without being
   "closed" would busy-loop logging warnings. There is no backoff.

8. **Stale repo doc.** `docs/mux-tag-poll.md` describes a `NewMux(ln) net.Conn` and
   nonexistent `WithMuxClientLocalAddr`/`WithMuxClientRemoteAddr` options. Anyone
   editing Mux from that doc will build on a wrong model. Consider updating it.

---

## Tests covering this

### `mux_test.go`

| Test | file:line | Asserts |
|---|---|---|
| `TestMux_SingleConnection` | `mux_test.go:26` | One conn → `ReadTagged` returns data and a non-nil `net.Conn` tag. |
| `TestMux_WriteBack` | `mux_test.go:62` | `WriteTagged` with the read tag routes the response back on the *same* conn (request/response pairing). |
| `TestMux_MultipleConnections` | `mux_test.go:113` | Sequential short-lived conns are all read (fan-in). |
| `TestMux_ConcurrentConnections` | `mux_test.go:153` | 4 simultaneous conns; each echo lands on its source conn via tag. Core concurrency/routing guarantee. |
| `TestMux_RequestResponseAcrossConnections` | `mux_test.go:207` | Multi-round request/response across distinct conns. |
| `TestMux_Close` | `mux_test.go:265` | Read/Write after close error; double close no panic. |
| `TestMux_WriteInvalidTag` | `mux_test.go:293` | `WriteTagged` with nil / non-`net.Conn` tag errors. |
| `TestMux_LocalAddr` | `mux_test.go:309` | `LocalAddr()` == listener addr. |
| `TestMux_Deadlines` | `mux_test.go:319` | Read deadline times out **with zero conns** (mux-level timer), error satisfies `net.Error.Timeout()`. |

### `mux_client_test.go`

| Test | file:line | Asserts |
|---|---|---|
| `TestMuxClient_SingleConnection` | `mux_client_test.go:23` | Write triggers dial; echo read back. |
| `TestMuxClient_MultipleConnections` | `mux_client_test.go:67` | Transparent redial across EOF-closed conns (one msg per conn). The EOF-cycle guarantee. |
| `TestMuxClient_RequestResponse` | `mux_client_test.go:105` | Multiple rounds on one kept-open conn (no premature cycle). |
| `TestMuxClient_Close` | `mux_client_test.go:154` | Read/Write after close error; double close no panic. |
| `TestMuxClient_WriteTriggersDialOnNoConnection` | `mux_client_test.go:181` | First Write dials (lazy first dial). |
| `TestMuxClient_Deadlines` | `mux_client_test.go:202` | Deadline set pre-dial; after dialing via Write, Read times out (enforced by underlying conn). |
| `TestMuxClient_DialError` | `mux_client_test.go:239` | Dial error propagates through both Write and Read via `errors.Is`. |

### Coverage gaps (no direct test)

- **Server backpressure / full `rQueue`** behavior (slow consumer stalling all
  conns) — untested.
- **Server accept-error continue loop** (`mux.go:160`) — untested.
- **Server deadline fan-out to *existing* conns** (`mux.go:301-331`) — only the
  zero-conn timer path is tested; the per-conn fan-out is not.
- **Server partial-read pending buffer** (`mux.go:224-233`, `:263-266`) — not
  exercised; tests read into 256-byte buffers larger than payloads.
- **Client write-error path (no redial)** — untested; only Read's EOF cycle is
  pinned. A regression that accidentally added write-redial would not be caught.
- **Client cycle race** (concurrent Read EOF + in-flight Write) — untested.
- **Client absolute-deadline across redial** (deadline already past after cycle) —
  untested.
- **Driver registration / `rq` param parsing** (`mux.go:31-66`) — no unit test in
  these files (exercised only indirectly via the pipeline elsewhere).

---

## Related docs

- `docs/internals/pipeline.md` — how the `mux` `Wrapper`/`Register` plugs into the
  layer pipeline (driver resolution, type transitions).
- `docs/internals/demux.md` — TaggedDemux/Demux that sit above Mux and consume tags.
- `docs/internals/poll-tagged.md` — PollConn and the request/response→stream bridge
  on the client (above MuxClient).
- `docs/internals/stream-transforms.md` — conn transforms (frame/split/buffered)
  that may be composed around these adapters.
- `docs/internals/server-tun.md` — server wiring that consumes the listener side.
- `docs/internals/drivers-proto.md` — DNST and other proto drivers that wrap the
  Mux/MuxClient endpoints.
- `docs/mux-tag-poll.md` — older architecture guide; **partially stale for Mux**
  (see the note at the top of this file).
