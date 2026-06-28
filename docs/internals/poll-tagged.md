# PollConn & TaggedConn / TaggedPipe

Source of truth: `poll_conn.go`, `tagged_conn.go`, `tagged_pipe.go`, with tests in
`poll_conn_test.go`, `demux_tagged_test.go`, `mux_test.go`, and
`proto/dnst/dnst_conn_int_test.go`. Every claim below is anchored with `file:line`.
Where behavior is implicit or surprising it is flagged as **AMBIGUITY** / **HAZARD**.

---

## Purpose & role

### PollConn — request/response → persistent bidirectional stream

Many transports are strictly lock-step: the side that speaks first sends a request and
gets exactly one response back (DNS-over-TXT is the motivating case). PollConn turns such
a transport into an ordinary streaming `net.Conn` (`poll_conn.go:1-21`).

- `pollConnClient` (returned by `NewPollConn`, `poll_conn.go:406-422`) is the *initiator*.
  Its loop drives the cycle: write a request, read a response, repeat. When the user has
  queued data it is sent as the request payload; when idle it sends an **empty poll**
  every `interval` so server-initiated data is still picked up (`poll_conn.go:424-460`).
- `pollConnServer` (returned by `NewPollServerConn`, `poll_conn.go:168-183`) is the
  *responder*. Its loop reads each request, hands the payload to `Read`, then immediately
  writes back any queued `Write` data or an **empty response** if none is pending
  (`poll_conn.go:185-239`).

The two halves must be paired: client-side `PollConn` on one end, `PollServerConn` on the
other (`poll_conn.go:18-20`, `164-167`). Used alone against a non-poll peer they will not
work because the peer will not perform the matching read-then-respond step.

### TaggedConn — carry an opaque tag from read to write

`TaggedConn` (`tagged_conn.go:11-38`) extends `net.Conn` with
`ReadTagged([]byte, *any) (int, error)` and `WriteTagged([]byte, any) (int, error)`.
`ReadTagged` populates `*tag` with context belonging to the data just read; the caller is
expected to feed that same tag back into the matching `WriteTagged` so the write can be
routed/correlated to the read (`tagged_conn.go:8-30`). This is "critical for DNS, where a
response must correspond to a specific query" — the tag is literally the parsed
`*dns.Msg` query (`proto/dnst/dnst_conn.go:115`, `141-145`) or a composite carrying both
the DNS query and an inner routing tag (`proto/dnst/dnst_conn.go:181-184`, `248`, `294`).

### TaggedPipe — in-memory TaggedConn pair

`TaggedPipe()` (`tagged_pipe.go:35-54`) returns two connected `taggedPipeConn` endpoints,
analogous to `net.Pipe` but carrying tags and message-oriented (one `WriteTagged` ⇒ one
logical `taggedPipeReq`). Used in tests and as the in-memory transport for tagged demux.

---

## Public API (exported symbols + options)

### PollConn

| Symbol | Location | Contract |
| --- | --- | --- |
| `NewPollConn(conn net.Conn, opts ...PollConnOption) net.Conn` | `poll_conn.go:406` | Client side. Starts loop goroutine immediately (`poll_conn.go:420`). Defaults: sendq=32, recvq=32, interval=1ms (`poll_conn.go:409-413`). |
| `NewPollServerConn(conn net.Conn, opts ...PollConnOption) net.Conn` | `poll_conn.go:168` | Server side. Starts loop immediately (`poll_conn.go:181`). Defaults: sendq=32, recvq=32, **no interval**, timeout=0 (`poll_conn.go:171-177`). |
| `PollConnOption` | `poll_conn.go:106` | Functional option mutating the shared `pollConnCore`. |
| `WithPollSendQueue(size uint16)` | `poll_conn.go:111` | Capacity of the queue feeding the loop's request/response payloads. `Write` blocks when full ⇒ backpressure (`poll_conn.go:108-115`). |
| `WithPollRecvQueue(size uint16)` | `poll_conn.go:120` | Capacity of the queue the loop pushes received payloads into. When full the loop blocks until the user `Read`s ⇒ backpressure (`poll_conn.go:117-124`). |
| `WithPollInterval(d time.Duration)` | `poll_conn.go:130` | Idle poll cadence (client only — see Register). Default 1ms (`poll_conn.go:126-134`). |
| `WithPollTimeout(d time.Duration)` | `poll_conn.go:141` | Server-only idle read timeout; 0 = none. On expiry the server loop exits and closes the underlying conn so the demux session is reclaimed (`poll_conn.go:136-145`, `198-202`). |

Both client and server expose `MaxWrite() uint16` that forwards the underlying conn's
`MaxWrite` if it implements that interface, else 0 (`poll_conn.go:242-247`, `463-468`).

**Driver registration**: `init()` registers driver name `"poll"` via `Register("poll", ...)`
(`poll_conn.go:35-96`). Recognized URI params (`poll_conn.go:38-72`):
- `interval` → `WithPollInterval`; rejected for listeners (client-only) (`poll_conn.go:40-48`).
- `timeout` → `WithPollTimeout`; rejected for non-listeners (server-only) (`poll_conn.go:49-57`).
- `sendq` → `WithPollSendQueue`, `recvq` → `WithPollRecvQueue` (uint16) (`poll_conn.go:58-69`).
- Unknown param ⇒ error (`poll_conn.go:70-71`).
The wrapper maps client→`NewPollConn`, server→`NewPollServerConn`
(`poll_conn.go:74-95`). **AMBIGUITY**: the `init` parser accepts `sendq`/`recvq` on both
sides, but `interval` is silently *ignored* if you pass it via the option list to a
server (the server loop has no `time.After` path — `poll_conn.go:185-239`), and `timeout`
is ignored by the client loop. Only the URI parser enforces the side restriction; calling
`WithPollInterval` on `NewPollServerConn` directly is a no-op.

### TaggedConn / TaggedPipe

| Symbol | Location | Contract |
| --- | --- | --- |
| `TaggedConn` interface | `tagged_conn.go:11` | `ReadTagged([]byte, *any)`, `WriteTagged([]byte, any)`, plus the standard `net.Conn` methods except `Read`/`Write` (those are *not* in the interface). |
| `ReadTagged(b []byte, tag *any) (int, error)` | `tagged_conn.go:14` | Reads into `b`; sets `*tag` to context for this datum. `tag` may be nil (callers must tolerate; implementations vary — see Hazards). |
| `WriteTagged(b []byte, tag any) (int, error)` | `tagged_conn.go:30` | Writes `b` using `tag` for routing/correlation. Doc explicitly says: pass the tag from the matching `ReadTagged` (`tagged_conn.go:18-29`). |
| `TaggedPipe() (TaggedConn, TaggedConn)` | `tagged_pipe.go:35` | In-memory connected pair, message-oriented, tag-carrying. |

`taggedPipeConn` also satisfies plain `net.Conn`: `Read` delegates to `ReadTagged` with a
throwaway tag (`tagged_pipe.go:139-142`); `Write` delegates to `WriteTagged(b, nil)`
(`tagged_pipe.go:144-146`).

---

## Internal design & invariants

### PollConn client loop (`poll_conn.go:424-460`)

```
buf := make([]byte, MaxPacketSize)        // 65535 (packet.go:4)
for {
  select {
    <-closed:           return            // shutdown
    d := <-sendCh:      data = d          // user data → this request
    <-time.After(interval): data = nil    // idle: empty poll
  }
  conn.Write(data)                        // request (may be empty)
  n, err := conn.Read(buf)                // exactly one response
  if n>0 { copy chunk; recvCh <- chunk }  // or block on closed
  if err != nil { return }
}
```

Invariants:
- **One write is always followed by one read** (`poll_conn.go:441-446`). The loop never
  reads twice or writes twice in a cycle. This is the request/response contract.
- **Idle ⇒ empty poll**: with nothing in `sendCh`, after `interval` elapses it writes
  `nil` (`poll_conn.go:436-437`, `441`). `conn.Write(nil)` must produce a real round-trip
  on the underlying transport — see the test's `msgPipeConn`/`FrameConn` which preserve
  zero-length message boundaries (`poll_conn_test.go:14-16`, `61-70`, `326-328`).
- **A fresh `time.After` timer is created every iteration** (`poll_conn.go:436`). When
  data flows continuously the timer is created and discarded each cycle; it is only
  *fired* when idle. **HAZARD**: a brand-new timer each loop is allocation churn but not a
  leak (the unfired timer is GC'd once the select returns via another case).

### PollConn server loop (`poll_conn.go:185-239`)

```
defer conn.Close()                        // eager close (see comment 186-191)
defer close(recvCh)
for {
  if timeout>0 { conn.SetReadDeadline(now+timeout) }   // idle detection
  n, err := conn.Read(buf)                              // a request
  if n>0 { recvCh <- chunk (or <-closed return) }
  if err != nil { return }                              // incl. deadline
  if timeout>0 { conn.SetReadDeadline(zero) }           // clear before write
  select { data := <-sendCh: response=data; default: response=nil }
  conn.Write(response)                                  // reply (may be empty)
}
```

Invariants:
- **Server never initiates**; it only replies to a received request. A response (possibly
  empty) is sent for every request (`poll_conn.go:226-237`). This is what guarantees the
  client's `Read` is eventually satisfied.
- **Non-blocking send drain**: the server pulls at most one queued `Write` payload per
  request via `select { case <-sendCh: default: }` (`poll_conn.go:228-233`). If multiple
  payloads are queued, they are delivered one per incoming poll — server push throughput
  is bounded by the client's poll rate. **HAZARD**: a server with a backlog can only
  flush it as fast as the client polls; with the client idle-polling at `interval`, that
  is at most one queued chunk per `interval`.
- **Read deadline is set then cleared around each cycle** so the idle timeout does not
  bleed into the write (`poll_conn.go:198-202`, `219-224`).

### Shared `pollConnCore` (`poll_conn.go:99-104`)

`sendCh` (server `Write` / client `Write` data queued for the next request/response),
`recvCh` (received payloads awaiting `Read`), `interval`, `timeout`. Both client and
server structs embed it (`poll_conn.go:150`, `389`). Options mutate this embedded struct
(`poll_conn.go:178-180`, `417-419`).

### Read-side buffering & partial reads (both sides)

`Read` first drains any leftover `unread` slice (`poll_conn.go:251-261`, `470-481`), then
selects on `recvCh`. When a chunk is larger than the caller's buffer, `copy` fills the
buffer and the remainder is stashed in `c.unread` for the next `Read`
(`poll_conn.go:288-293`, `510-515`). Verified by `TestPollConn_SmallReadBuffer`
(`poll_conn_test.go:285-315`). **Invariant**: chunk boundaries from the underlying conn
are *not* preserved across `Read` calls once split — partial reads are byte-stream-like.

### Tag carry-through contract (the load-bearing invariant)

`ReadTagged` → user → `WriteTagged` must round-trip the tag, and each tag must be
consumed exactly once. The canonical consumers:

- **TaggedDemux** (`demux_tagged.go`): `readLoop` reads `(payload, tag)` from the
  underlying TaggedConn (`demux_tagged.go:85`), enqueues `taggedDemuxPacket{data, tag}`
  into the per-session `rQueue` (`demux_tagged.go:133`). On the session's `Read`, the tag
  is popped from `rQueue` and pushed onto a *separate* `tagQueue`
  (`demux_tagged.go:200-204`). On the session's `Write`, a tag is pulled from `tagQueue`
  and used for `bc.WriteTagged(payload, tag)` (`demux_tagged.go:246-266`). This is the
  read→write hand-off, decoupled through two channels.
- **dnst TaggedServerConn** (`proto/dnst/dnst_conn.go:227-298`): `ReadTagged` builds
  `serverConnTagged{dnsMsg, connTag}` where `connTag` is the *inner* TaggedConn's tag
  (`dnst_conn.go:233`, `248`); `WriteTagged` type-asserts that struct, forms a DNS TXT
  reply for `dnsMsg`, and writes it with `connTag` to route it back
  (`dnst_conn.go:272-294`). Nested tag composition: the DNST tag *wraps* the inner tag.
- **dnst (plain) serverConn** (`proto/dnst/dnst_conn.go:96-168`): tag is the bare
  `*dns.Msg` (`dnst_conn.go:115`, `142`).
- **Mux** (`mux.go:219-284`): tag is the `net.Conn` the data arrived on; `WriteTagged`
  type-asserts `tag.(net.Conn)` and writes straight to it (`mux.go:279-283`).

---

## Concurrency model

### PollConn

- Exactly **one loop goroutine** per endpoint, spawned in the constructor
  (`poll_conn.go:181`, `420`). It owns all I/O on the underlying conn.
- `recvCh` / `sendCh` are buffered channels (default 32) bridging loop ↔ user.
- `Read` blocks in a `select` over `recvCh`, `closed`, a per-call deadline timer, and a
  `readDlNotify` channel (`poll_conn.go:280-307`, `502-529`). It loops on `notify` so a
  concurrent `SetReadDeadline` re-arms the timer (`poll_conn.go:302-307`).
- `Write` copies the buffer (`poll_conn.go:316-317`, `538-539`), then blocks on
  `sendCh <-` / `closed` / deadline (`poll_conn.go:341-348`, `563-570`). A pre-check on
  `closed` short-circuits a closed conn (`poll_conn.go:335-339`, `557-561`).
- Locks: `mu` guards `unread` + `readDeadline` + `readDlNotify`; `wMu` guards
  `writeDeadline` (`poll_conn.go:152-159`, `391-397`). Read and write paths use separate
  mutexes, so reads and writes do not contend. **AMBIGUITY**: `Read` is not internally
  serialized against itself beyond the `unread` critical section — two concurrent `Read`
  callers can both pull from `recvCh`; the tests only ever use a single reader
  (`poll_conn_test.go:247-259`).

### TaggedPipe

- **No background goroutine.** A `WriteTagged` rendezvous: it sends a `*taggedPipeReq`
  on the write channel and then blocks on `req.done` until the reader has consumed all
  bytes (`tagged_pipe.go:119-137`). Channels are *unbuffered* (`tagged_pipe.go:36-37`).
- `ReadTagged` holds a partially-consumed `req` in `c.req` under `mu`
  (`tagged_pipe.go:57-59`, `100-108`); a second `ReadTagged` resumes it. When fully read
  it signals `req.done` and clears `c.req` (`tagged_pipe.go:91-102`).
- `req.done` send uses a `select` with a `default` that spawns a goroutine if it would
  block (`tagged_pipe.go:93-99`). **AMBIGUITY/HAZARD**: comment says "Should not block
  ideally"; because the writer is parked on `req.done` (`tagged_pipe.go:131-136`) the
  direct send normally succeeds, but the `default`-spawned goroutine is a fallback that
  can briefly outlive the call. Concurrent multi-reader use of one endpoint is not a
  supported pattern (the partial-`req` state is single-slot).

---

## Lifecycle & ownership

### PollConn Close (`poll_conn.go:351-358`, `573-580`)

- Guarded by `sync.Once` (`closeOnce`) — **double-close is safe**, returns the first
  underlying `Close` error; subsequent calls return nil (the `Do` body does not re-run).
- `Close` closes the `closed` channel and calls `c.conn.Close()`. Closing `closed`
  unblocks the loop's `select` (`poll_conn.go:432`, `336-344`, `558-566`) and any blocked
  `Read`/`Write` (`poll_conn.go:295-299`, `517-521`).
- **Loop teardown**: the loop's `defer close(c.recvCh)` (`poll_conn.go:426`, `194`) runs
  when the loop returns; a subsequent `Read` draining a closed-and-empty `recvCh` gets
  `io.EOF` (`poll_conn.go:285-287`, `507-509`). Note the *race window*: a `Read` blocked
  in `select` may resolve via the `closed` case (`net.ErrClosed`) **or** the closed
  `recvCh` case (`io.EOF`) depending on scheduling — see Errors section.
- **Server eager underlying close**: the server loop also `defer c.conn.Close()`
  (`poll_conn.go:192`) so that when the loop exits for *any* reason (read error, idle
  timeout, closed send) the underlying conn is closed immediately — the comment
  (`poll_conn.go:186-191`) explains this prevents reconnecting-client packets from
  landing in a dead demux session's `rQueue` and being silently lost.
- **Drain semantics**: queued but unsent `sendCh` data is **discarded** on close — the
  loop exits without flushing. There is no graceful drain. **HAZARD**: data accepted by
  `Write` (returned success) but still sitting in `sendCh` when `Close` happens is lost.

### TaggedPipe end closing (`tagged_pipe.go:148-154`)

- `Close` (under `closeOnce`) closes `c.closed` **and** closes `c.chWrite`
  (`tagged_pipe.go:149-152`). Closing `chWrite` is what makes the *peer's* `ReadTagged`
  observe `io.EOF` (`tagged_pipe.go:70-74`) — the EOF signal flows from the closer to the
  other end's read path.
- The local end's blocked `ReadTagged`/`WriteTagged` see `<-c.closed` and return
  `io.ErrClosedPipe` (`tagged_pipe.go:68-69`, `126-127`, `132-133`).
- `WriteTagged` wraps the body in a `recover()` that maps a panic to `io.ErrClosedPipe`
  (`tagged_pipe.go:113-117`). **HAZARD**: this catches the "send on closed channel" panic
  that occurs if the *peer* closed `chWrite` (which is this end's read channel)… but note
  each end closes its *own* `chWrite`. If both ends race to close, a send on an
  already-closed channel is the panic this recover guards. Double-close of one end is
  safe via `closeOnce`.
- **Ordering guarantee**: because channels are unbuffered and `WriteTagged` blocks until
  `req.done`, a completed `WriteTagged` means the peer fully consumed those bytes — writes
  and reads are strictly synchronized 1:1 in order (`tagged_pipe.go:128-136`,
  `91-102`). Two writes from the same end cannot be reordered; the second blocks until the
  first's reader drains it.

---

## Error, EOF & deadline semantics

### PollConn

- `Read` after close: `net.ErrClosed` (via `closed`) **or** `io.EOF` (via closed
  `recvCh`) — non-deterministic which fires first (`poll_conn.go:285-299`, `507-521`).
  `TestPollConn_Close` only asserts `err != nil`, not the specific error
  (`poll_conn_test.go:202-205`). **AMBIGUITY**: callers must treat both as "closed".
- `Read` deadline: zero-time deadline ⇒ block forever; past deadline ⇒ immediate
  `os.ErrDeadlineExceeded` (`poll_conn.go:271-278`, `493-500`); timer fire ⇒
  `os.ErrDeadlineExceeded` (`poll_conn.go:300-301`, `522-523`). A deadline change mid-wait
  re-arms via `readDlNotify` (`poll_conn.go:302-307`, `370-377`).
- `Write` deadline: same shape — past/expired ⇒ `os.ErrDeadlineExceeded`
  (`poll_conn.go:325-332`, `346-347`). **Note**: the `Write` deadline only bounds the time
  spent waiting to enqueue into `sendCh`; it does **not** bound the time until the data is
  actually transmitted by the loop (`poll_conn.go:341-348`).
- Empty `Write` returns `(0, nil)` without queuing (`poll_conn.go:312-314`, `534-536`) —
  so the user cannot force an empty poll via `Write`; empty polls come only from the
  interval timer.
- Underlying read/write errors terminate the loop (`poll_conn.go:441-443`, `456-458`,
  `205-217`, `235-237`); the loop's `defer close(recvCh)` then surfaces as `io.EOF` to the
  reader.
- Server idle timeout fires as a read error inside the loop (`poll_conn.go:198-205`),
  exiting the loop and closing the underlying conn; the local `Read` then sees EOF/closed.
  Verified by `TestPollServerConn_IdleTimeout` (`poll_conn_test.go:449-473`).

### TaggedPipe

- `ReadTagged` on closed local end: `io.ErrClosedPipe`; on peer-closed channel: `io.EOF`;
  deadline: `os.ErrDeadlineExceeded` (`tagged_pipe.go:67-77`).
- `WriteTagged` on closed: `io.ErrClosedPipe` (`tagged_pipe.go:125-133`, plus recover at
  `113-117`).
- Deadline support is **read-only and simplified**: `SetReadDeadline` stores it
  (`tagged_pipe.go:160-165`); `SetDeadline` only applies the *read* deadline and ignores
  write (`tagged_pipe.go:158`); `SetWriteDeadline` is a no-op (`tagged_pipe.go:159`).
  **HAZARD**: `WriteTagged` can block indefinitely with no deadline escape — only `Close`
  or a reader unblocks it. **AMBIGUITY**: the read deadline is read *without holding `mu`*
  at `tagged_pipe.go:62-64` while `SetReadDeadline` writes it under `mu`
  (`tagged_pipe.go:160-165`) — a benign data race in practice but technically unsynchronized.
  Also the deadline timer is computed once and not re-armed if changed mid-read.

---

## Dependencies

**PollConn depends on:**
- `MaxPacketSize` = 65535 for its read buffer (`poll_conn.go:186`, `425`; `packet.go:4`).
- `Register` / `Wrapper` / `ConnWrapListener` / `ConnWrapDialer` / `Dialer` for the
  `"poll"` driver (`poll_conn.go:36-95`; `driver.go:15`; `wrap.go:119`, `327-332`;
  `mux_client.go:28`).
- The underlying `net.Conn`'s optional `MaxWrite()` (forwarded, `poll_conn.go:242-247`,
  `463-468`).
- Standard library only otherwise (`net`, `time`, `sync`, `io`, `os`).

**PollConn depended on by:**
- The `"poll"` URI driver consumers (anything composing a transport string with `poll`).
- `proto/dnst` integration: full client stack TCP → DNST → DemuxClient → **PollConn**
  (`proto/dnst/dnst_conn_int_test.go:725-739`, also `416`, `489`). PollConn is the
  *outermost* client wrapper that turns the DNS request/response tunnel into a stream.

**TaggedConn / TaggedPipe depended on by (the contract is widely relied upon):**
- `TaggedDemux` / `taggedDemuxSess` (`demux_tagged.go:33`, `85`, `266`).
- `Mux` produces a `TaggedConn` (`mux.go:122`, `219-284`).
- dnst `serverConn` / `taggedServerConn` *are* `TaggedConn`s and consume an inner
  `TaggedConn` (`proto/dnst/dnst_conn.go:67`, `96-168`, `199`, `227-298`).
- `wrap.go` pipeline typing has a first-class `PipeTypeTaggedConn`
  (`wrap.go:28`, `126-140`, `242`); `demux.go:69` has a `TaggedToListener` adapter.
- `TaggedPipe` is the in-memory transport in `demux_tagged_test.go:14`, `85`, `186`.

**External:** dnst uses `github.com/miekg/dns` for tag = `*dns.Msg`
(`proto/dnst/dnst_conn.go:142`, `181-184`).

---

## Change hazards (MOST IMPORTANT)

1. **Poll interval too low ⇒ busy-poll / load amplification.** Default is 1ms
   (`poll_conn.go:412`). When idle, the client emits a full request/response round-trip
   every `interval` on the underlying transport (`poll_conn.go:436-446`). On a real
   network (DNS over UDP/TCP) a sub-millisecond interval is a self-inflicted flood and
   burns CPU on `time.After` allocation every iteration. Tune up for production; the
   tests use 10ms (`poll_conn_test.go:107` and throughout). Changing the default affects
   latency-vs-load for every poll user.

2. **Server push throughput is gated by the client poll rate.** The server drains **one**
   `sendCh` payload per incoming request via `select{...; default}`
   (`poll_conn.go:228-233`). If the server queues N chunks, they leave at most one per
   client poll. Raising `sendq` does not raise throughput — it only buffers more before
   `Write` blocks. If you need bursty server push, the loop logic (not the queue size) is
   what must change.

3. **Close drops queued data — no flush.** Both loops exit on `closed` without draining
   `sendCh` (`poll_conn.go:432`, `194` server / `426` client). A `Write` that returned
   success can still be lost if `Close` races it. Do not assume `Write`+`Close` delivers.

4. **Tag must round-trip read→write and be consumed once.** The whole TaggedConn design
   assumes the caller threads the `*tag` from `ReadTagged` into the matching
   `WriteTagged` (`tagged_conn.go:18-29`). In `TaggedDemux` the tag is moved
   read-queue → `tagQueue` → write (`demux_tagged.go:200-204`, `246-266`), and
   `tagQueue` is sized `sessReadQueueSize*2` (`demux_tagged.go:118`). If reads and writes
   get out of balance (more reads than writes, or vice-versa) tags accumulate or starve:
   a `Write` with no pending tag *blocks* until one is available (`demux_tagged.go:247-257`).
   **A write without a prior read can deadlock.** Any change that breaks the 1-read ⇒
   1-write tag accounting (e.g. adding internal reads that drop the tag) will corrupt DNS
   query/response correlation.

5. **`WriteTagged` tag-type contract is unchecked at the interface but enforced per
   implementation.** Mux requires `tag.(net.Conn)` non-nil (`mux.go:279-283`); dnst
   requires `*dns.Msg` (`dnst_conn.go:142-145`) or `serverConnTagged` with non-nil
   `dnsMsg` (`dnst_conn.go:273-276`). Passing a `nil` tag (as `taggedPipeConn.Write` does,
   `tagged_pipe.go:145`) or the wrong type to those implementations is a runtime error,
   not a compile error. **HAZARD**: `serverConn.ReadTagged` does `*tag = m` with **no nil
   check** (`dnst_conn.go:115`), unlike `taggedServerConn.ReadTagged` which guards
   `if tag != nil` (`dnst_conn.go:247`) and `mux`/`taggedPipe` which also guard. Calling
   the plain dnst `serverConn.ReadTagged(b, nil)` will **panic** (nil pointer deref).
   Keep the nil guard convention when adding implementations.

6. **Deadline interaction with the loop.** A `Read` deadline only times out the *waiting
   caller*; the loop keeps polling independently (`poll_conn.go:280-307`). A `Write`
   deadline only bounds enqueue time, not transmit time (hazard #3 corollary). The server
   idle `timeout` is a *different* mechanism (underlying read deadline,
   `poll_conn.go:198-202`) that tears down the whole connection, not a per-call deadline —
   don't conflate `WithPollTimeout` (server, kills conn) with `SetReadDeadline` (per-call).

7. **`PollBufSize` vs upstream frame sizes.** The loop's read buffer is fixed at
   `MaxPacketSize` (65535) (`poll_conn.go:186`, `425`); there is **no** `WithPollBufSize`
   option despite the task brief mentioning one — buffer size is not configurable.
   If the underlying conn delivers a single read larger than 65535 (a stream conn without
   framing) the excess is truncated/lost on that read. PollConn assumes a *message/framed*
   underlying conn (the tests wrap raw `net.Pipe` in `FrameConn` precisely for this,
   `poll_conn_test.go:326-328`). Pairing PollConn directly over an unframed byte stream
   breaks the one-request/one-response and empty-poll assumptions.

8. **Empty writes must round-trip on the underlying transport.** Idle polling writes
   zero-length payloads (`poll_conn.go:441` with `data == nil`). If the underlying conn
   collapses empty writes (e.g. some framing that drops zero-length frames) the server
   never sees the poll and never replies, stalling server-initiated delivery. The test
   pipe deliberately round-trips empty messages (`poll_conn_test.go:14-16`).

9. **TaggedPipe writes have no deadline escape.** `SetWriteDeadline` is a no-op
   (`tagged_pipe.go:159`); a `WriteTagged` with no reader blocks until `Close`
   (`tagged_pipe.go:128-136`). Any code expecting write timeouts on a TaggedPipe will hang.

10. **Driver param side-restriction lives only in the URI parser.** `interval` is
    client-only and `timeout` is server-only *only when parsed from a URI*
    (`poll_conn.go:40-57`). Direct option calls have no such guard and silently no-op on
    the wrong side. Don't rely on options to validate side correctness.

---

## Tests covering this

`poll_conn_test.go`:
- `TestPollConn_Echo` (`:100-124`): write→read echo round-trips through the loop.
- `TestPollConn_ServerInitiated` (`:126-153`): idle empty-poll picks up server data
  (asserts polling delivers unrequested data).
- `TestPollConn_MultipleMessages` (`:155-181`): sequential write/read ordering across 5
  messages.
- `TestPollConn_Close` (`:183-212`): post-close `Read` **and** `Write` both error
  (asserts only `err != nil`, not which error).
- `TestPollConn_ReadDeadline` (`:214-231`): server always-empty ⇒ `Read` deadline fires.
- `TestPollConn_ConcurrentReadWrite` (`:233-283`): one reader goroutine + sequential
  writer, 10 messages with 15ms spacing — exercises concurrent Read/Write paths.
- `TestPollConn_SmallReadBuffer` (`:285-315`): 5-byte buffer reassembles a long message —
  asserts partial-read `unread` handling.
- `TestPollServerConn_Echo` / `_ServerInitiated` / `_BidirectionalExchange` /
  `_ClosePropagatesToClient` / `_IdleTimeout` (`:333-473`): full client+server pairing
  over `net.Pipe`+`FrameConn`; `_IdleTimeout` asserts `WithPollTimeout` closes within
  `5*timeout`.

`demux_tagged_test.go`:
- `TestTaggedDemux_Basic` (`:13-82`): **the key tag round-trip assertion** — a tag set on
  the client `WriteTagged` (`:35`) is echoed back via the demux session's `Write` and the
  client `ReadTagged` sees the *same* tag (`:69`, `:79-81`). This is the contract dnst
  relies on.
- `TestTaggedDemux_MultipleSessions` (`:84+`): tag may be nil (`:104`, `:113`).

`mux_test.go`: `ReadTagged`/`WriteTagged` with `net.Conn` tags; nil/wrong-type tag
rejection (`:298-305`).

`proto/dnst/dnst_conn_int_test.go`: PollConn over the full DNST+demux stack
(`:416`, `:489`, `:725-739`).

**Gaps / under-tested:**
- No test for **send-queue overflow / `Write` backpressure** (filling `sendCh` to make
  `Write` block) — the documented backpressure (`poll_conn.go:108-115`) is unverified.
- No test for **recv-queue overflow** (loop blocking on full `recvCh`,
  `poll_conn.go:450-454`) — backpressure unverified.
- No test asserts the **specific** post-close error (EOF vs `net.ErrClosed`) — the
  non-determinism (hazard) is untested.
- No test for **data loss on Close with queued sendCh** (hazard #3).
- No direct unit test for `TaggedPipe` partial reads / `req.done` `default` goroutine
  fallback (`tagged_pipe.go:91-99`); only exercised indirectly via demux tests.
- No test for `serverConn.ReadTagged(b, nil)` nil-panic (hazard #5).
- No test that `WithPollInterval` on a server is a no-op, nor that `time.After` allocation
  churn is bounded.

---

## Related docs

- `pipeline.md` — how `Wrapper`/`PipeType*` compose layers (the `poll` driver and
  `TaggedConn` typing plug in here; `wrap.go`).
- `mux.md` — `Mux` as a `TaggedConn` producer (tag = `net.Conn`).
- `demux.md` — `TaggedDemux`/`taggedDemuxSess`, the primary tag read→write consumer.
- `stream-transforms.md` — `FrameConn`, `SplitConn`, `aesgcm` and `MaxWrite`/`MaxPacketSize`
  interplay that PollConn forwards and relies on.
- `drivers-proto.md` — the `dnst` proto/driver, the motivating tag = `*dns.Msg` case and
  the full PollConn↔DNST↔Demux stack.
