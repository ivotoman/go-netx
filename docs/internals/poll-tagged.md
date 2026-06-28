# PollConn & TaggedConn / TaggedPipe

Source of truth: `poll_conn.go`, `tagged_conn.go`, `tagged_pipe.go`, with tests in
`poll_conn_test.go`, `demux_tagged_test.go`, `mux_test.go`, and
`proto/dnst/dnst_conn_int_test.go`.

> Citations name a **symbol + file** (e.g. `loop in poll_conn.go`), not line numbers —
> line numbers rot on every refactor. Bare line numbers appear only for things with no
> stable symbol (an error-string literal, a numeric default in a struct literal).

---

## Purpose & role

### PollConn — request/response → persistent bidirectional stream

Many transports are strictly lock-step: the side that speaks first sends a request and
gets exactly one response back (DNS-over-TXT is the motivating case). PollConn turns such
a transport into an ordinary streaming `net.Conn` (see the package doc at the top of
`poll_conn.go`).

- `pollConnClient` (returned by `NewPollConn`) is the *initiator*. Its `loop` drives the
  cycle: write a request, read a response, repeat. When the user has queued data it is sent
  as the request payload; when idle it sends an **empty poll** every `interval` so
  server-initiated data is still picked up (`pollConnClient.loop`).
- `pollConnServer` (returned by `NewPollServerConn`) is the *responder*. Its `loop` reads
  each request, hands the payload to `Read`, then immediately writes back any queued `Write`
  data or an **empty response** if none is pending (`pollConnServer.loop`).

The two halves must be paired: client-side `PollConn` on one end, `PollServerConn` on the
other (see `NewPollConn` / `NewPollServerConn` doc comments). Used alone against a non-poll
peer they will not work because the peer will not perform the matching read-then-respond step.

### TaggedConn — carry an opaque tag from read to write

`TaggedConn` (`tagged_conn.go`) extends `net.Conn` with
`ReadTagged([]byte, *any) (int, error)` and `WriteTagged([]byte, any) (int, error)`.
`ReadTagged` populates `*tag` with context belonging to the data just read; the caller is
expected to feed that same tag back into the matching `WriteTagged` so the write can be
routed/correlated to the read (`WriteTagged` doc comment in `tagged_conn.go`). This is
"critical for DNS, where a response must correspond to a specific query" — the tag is
literally the parsed `*dns.Msg` query (`serverConn.ReadTagged` in `proto/dnst/dnst_conn.go`)
or a composite carrying both the DNS query and an inner routing tag (`serverConnTagged` /
`taggedServerConn.ReadTagged` in `proto/dnst/dnst_conn.go`).

### TaggedPipe — in-memory TaggedConn pair

`TaggedPipe()` (`tagged_pipe.go`) returns two connected `taggedPipeConn` endpoints,
analogous to `net.Pipe` but carrying tags and message-oriented (one `WriteTagged` ⇒ one
logical `taggedPipeReq`). Used in tests and as the in-memory transport for tagged demux.

---

## Public API (exported symbols + options)

### PollConn

| Symbol | Contract |
| --- | --- |
| `NewPollConn(conn net.Conn, opts ...PollConnOption) net.Conn` | Client side. Starts `loop` goroutine immediately. Defaults: sendq=32, recvq=32, interval=1ms (set in `NewPollConn`). |
| `NewPollServerConn(conn net.Conn, opts ...PollConnOption) net.Conn` | Server side. Starts `loop` immediately. Defaults: sendq=32, recvq=32, **no interval**, timeout=0 (set in `NewPollServerConn`). |
| `PollConnOption` | Functional option mutating the shared `pollConnCore`. |
| `WithPollSendQueue(size uint16)` | Capacity of the queue feeding the loop's request/response payloads. `Write` blocks when full ⇒ backpressure. |
| `WithPollRecvQueue(size uint16)` | Capacity of the queue the loop pushes received payloads into. When full the loop blocks until the user `Read`s ⇒ backpressure. |
| `WithPollInterval(d time.Duration)` | Idle poll cadence (client only — see Register). Default 1ms. |
| `WithPollTimeout(d time.Duration)` | Server-only idle read timeout; 0 = none. On expiry the server loop exits and closes the underlying conn so the demux session is reclaimed (see `pollConnServer.loop`). |

Both client and server expose `MaxWrite() uint16` that forwards the underlying conn's
`MaxWrite` if it implements that interface, else 0 (`pollConnServer.MaxWrite` /
`pollConnClient.MaxWrite`).

**Driver registration**: the `init` in `poll_conn.go` registers driver name `"poll"` via
`Register`. Recognized URI params:
- `interval` → `WithPollInterval`; rejected for listeners (client-only).
- `timeout` → `WithPollTimeout`; rejected for non-listeners (server-only).
- `sendq` → `WithPollSendQueue`, `recvq` → `WithPollRecvQueue` (uint16).
- Unknown param ⇒ error.

The wrapper maps client→`NewPollConn`, server→`NewPollServerConn`. **AMBIGUITY**: only the
URI parser enforces the side restriction. Calling `WithPollInterval` on
`NewPollServerConn` directly is a silent no-op (the server `loop` has no `time.After`
path), and `timeout` is ignored by the client `loop`. Direct option calls do not validate
side correctness.

### TaggedConn / TaggedPipe

| Symbol | Contract |
| --- | --- |
| `TaggedConn` interface (`tagged_conn.go`) | `ReadTagged([]byte, *any)`, `WriteTagged([]byte, any)`, plus the standard `net.Conn` methods except `Read`/`Write` (those are *not* in the interface). |
| `ReadTagged(b []byte, tag *any) (int, error)` | Reads into `b`; sets `*tag` to context for this datum. `tag` may be nil (callers must tolerate; implementations vary — see Hazards). |
| `WriteTagged(b []byte, tag any) (int, error)` | Writes `b` using `tag` for routing/correlation. Doc explicitly says: pass the tag from the matching `ReadTagged` (`WriteTagged` doc comment). |
| `TaggedPipe() (TaggedConn, TaggedConn)` | In-memory connected pair, message-oriented, tag-carrying. |

`taggedPipeConn` also satisfies plain `net.Conn`: `Read` delegates to `ReadTagged` with a
throwaway tag; `Write` delegates to `WriteTagged(b, nil)` (`taggedPipeConn.Read` /
`taggedPipeConn.Write`).

---

## Internal design & invariants

### PollConn client loop (`pollConnClient.loop`)

```
buf := make([]byte, MaxPacketSize)        // 65535 (MaxPacketSize in packet.go)
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
- **One write is always followed by one read.** The loop never reads twice or writes twice
  in a cycle. This is the request/response contract.
- **Idle ⇒ empty poll**: with nothing in `sendCh`, after `interval` elapses it writes `nil`.
  `conn.Write(nil)` must produce a real round-trip on the underlying transport — see the
  test's `msgPipeConn` / `FrameConn` which preserve zero-length message boundaries
  (`msgPipeConn.Write` in `poll_conn_test.go`).
- **A fresh `time.After` timer is created every iteration.** When data flows continuously
  the timer is created and discarded each cycle; it is only *fired* when idle. **HAZARD**:
  per-loop timer creation is allocation churn but not a leak (the unfired timer is GC'd once
  the select returns via another case).

### PollConn server loop (`pollConnServer.loop`)

```
defer conn.Close()                        // eager close (see comment in loop)
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
  empty) is sent for every request. This is what guarantees the client's `Read` is
  eventually satisfied.
- **Non-blocking send drain**: the server pulls at most one queued `Write` payload per
  request via `select { case <-sendCh: default: }`. If multiple payloads are queued, they
  are delivered one per incoming poll — server push throughput is bounded by the client's
  poll rate (see Change hazard #2).
- **Read deadline is set then cleared around each cycle** so the idle timeout does not bleed
  into the write.

### Shared `pollConnCore`

`sendCh` (data queued for the next request/response), `recvCh` (received payloads awaiting
`Read`), `interval`, `timeout`. Both `pollConnClient` and `pollConnServer` embed it; options
mutate this embedded struct (see the `for _, o := range opts` loops in the constructors).

### Read-side buffering & partial reads (both sides)

`Read` first drains any leftover `unread` slice, then selects on `recvCh`. When a chunk is
larger than the caller's buffer, `copy` fills the buffer and the remainder is stashed in
`c.unread` for the next `Read` (`pollConnServer.Read` / `pollConnClient.Read`). Verified by
`TestPollConn_SmallReadBuffer`. **Invariant**: chunk boundaries from the underlying conn are
*not* preserved across `Read` calls once split — partial reads are byte-stream-like.

### Tag carry-through contract (the load-bearing invariant)

`ReadTagged` → user → `WriteTagged` must round-trip the tag, and each tag must be consumed
exactly once. The canonical consumers:

- **TaggedDemux** (`demux_tagged.go`): `taggedDemux.readLoop` reads `(payload, tag)` from
  the underlying TaggedConn and enqueues `taggedDemuxPacket{data, tag}` into the per-session
  `rQueue` (`taggedDemux.processPacket`). On the session's `Read`, the tag is popped from
  `rQueue` and pushed onto a *separate* `tagQueue`; on the session's `Write`, a tag is pulled
  from `tagQueue` and used for `bc.WriteTagged(payload, tag)` (`taggedDemuxSess.Read` /
  `taggedDemuxSess.Write`). This is the read→write hand-off, decoupled through two channels.
- **dnst `taggedServerConn`** (`proto/dnst/dnst_conn.go`): `ReadTagged` builds
  `serverConnTagged{dnsMsg, connTag}` where `connTag` is the *inner* TaggedConn's tag;
  `WriteTagged` type-asserts that struct, forms a DNS TXT reply for `dnsMsg`, and writes it
  with `connTag` to route it back. Nested tag composition: the DNST tag *wraps* the inner tag.
- **dnst (plain) `serverConn`** (`proto/dnst/dnst_conn.go`): tag is the bare `*dns.Msg`.
- **Mux** (`mux.go`): tag is the `net.Conn` the data arrived on; `mux.WriteTagged`
  type-asserts `tag.(net.Conn)` and writes straight to it.

---

## Concurrency model

### PollConn

- Exactly **one loop goroutine** per endpoint, spawned in the constructor. It owns all I/O
  on the underlying conn.
- `recvCh` / `sendCh` are buffered channels (default 32) bridging loop ↔ user.
- `Read` blocks in a `select` over `recvCh`, `closed`, a per-call deadline timer, and a
  `readDlNotify` channel. It loops on `notify` so a concurrent `SetReadDeadline` re-arms the
  timer.
- `Write` copies the buffer, then blocks on `sendCh <-` / `closed` / deadline. A pre-check on
  `closed` short-circuits a closed conn.
- Locks: `mu` guards `unread` + `readDeadline` + `readDlNotify`; `wMu` guards
  `writeDeadline`. Read and write paths use separate mutexes, so reads and writes do not
  contend. **AMBIGUITY**: `Read` is not internally serialized against itself beyond the
  `unread` critical section — two concurrent `Read` callers can both pull from `recvCh`; the
  tests only ever use a single reader.

### TaggedPipe

- **No background goroutine.** A `WriteTagged` rendezvous: it sends a `*taggedPipeReq` on the
  write channel and then blocks on `req.done` until the reader has consumed all bytes. The
  pair's channels are *unbuffered* (`TaggedPipe`).
- `ReadTagged` holds a partially-consumed `req` in `c.req` under `mu`; a second `ReadTagged`
  resumes it. When fully read it signals `req.done` and clears `c.req`.
- Single-slot `c.req` means **one reader per endpoint**; the `done`-send `select` (in
  `ReadTagged`, the `req.n == len(req.data)` branch) has a `default` that spawns a goroutine
  to deliver `req.done` if the direct send would block — a fallback that may briefly outlive
  the call. Normally the writer is already parked on `req.done`, so the direct send succeeds.

---

## Lifecycle & ownership

### PollConn Close (`pollConnServer.Close` / `pollConnClient.Close`)

- Guarded by `sync.Once` (`closeOnce`) — **double-close is safe**, returns the first
  underlying `Close` error; subsequent calls return nil (the `Do` body does not re-run).
- `Close` closes the `closed` channel and calls `c.conn.Close()`. Closing `closed` unblocks
  the loop's `select` and any blocked `Read`/`Write`.
- **Loop teardown**: the loop's `defer close(c.recvCh)` runs when the loop returns; a
  subsequent `Read` draining a closed-and-empty `recvCh` gets `io.EOF`. Note the *race
  window*: a `Read` blocked in `select` may resolve via the `closed` case (`net.ErrClosed`)
  **or** the closed `recvCh` case (`io.EOF`) depending on scheduling — see Errors section.
- **Server eager underlying close**: `pollConnServer.loop` also `defer c.conn.Close()` so
  that when the loop exits for *any* reason (read error, idle timeout, closed send) the
  underlying conn is closed immediately — the comment in `loop` explains this prevents
  reconnecting-client packets from landing in a dead demux session's `rQueue` and being
  silently lost.
- **Drain semantics**: queued but unsent `sendCh` data is **discarded** on close — the loop
  exits without flushing. There is no graceful drain (see Change hazard #3).

### TaggedPipe end closing (`taggedPipeConn.Close`)

- `Close` (under `closeOnce`) closes `c.closed` **and** closes `c.chWrite`. Closing
  `chWrite` is what makes the *peer's* `ReadTagged` observe `io.EOF` — the EOF signal flows
  from the closer to the other end's read path.
- The local end's blocked `ReadTagged`/`WriteTagged` see `<-c.closed` and return
  `io.ErrClosedPipe`.
- `WriteTagged` wraps the body in a `recover()` that maps a panic to `io.ErrClosedPipe`. Each
  end closes its *own* `chWrite`; if both ends race to close, a send on an already-closed
  channel is the panic this recover guards. Double-close of one end is safe via `closeOnce`.
- **Ordering guarantee**: because channels are unbuffered and `WriteTagged` blocks until
  `req.done`, a completed `WriteTagged` means the peer fully consumed those bytes — writes
  and reads are strictly synchronized 1:1 in order. Two writes from the same end cannot be
  reordered; the second blocks until the first's reader drains it.

---

## Error, EOF & deadline semantics

### PollConn

- `Read` after close: `net.ErrClosed` (via `closed`) **or** `io.EOF` (via closed `recvCh`) —
  non-deterministic which fires first. `TestPollConn_Close` only asserts `err != nil`, not
  the specific error. **AMBIGUITY**: callers must treat both as "closed".
- `Read` deadline: zero-time deadline ⇒ block forever; past deadline ⇒ immediate
  `os.ErrDeadlineExceeded`; timer fire ⇒ `os.ErrDeadlineExceeded`. A deadline change mid-wait
  re-arms via `readDlNotify` (`SetReadDeadline`).
- `Write` deadline: same shape — past/expired ⇒ `os.ErrDeadlineExceeded`. **Note**: the
  `Write` deadline only bounds the time spent waiting to enqueue into `sendCh`; it does
  **not** bound the time until the data is actually transmitted by the loop.
- Empty `Write` returns `(0, nil)` without queuing — so the user cannot force an empty poll
  via `Write`; empty polls come only from the interval timer.
- Underlying read/write errors terminate the loop; the loop's `defer close(recvCh)` then
  surfaces as `io.EOF` to the reader.
- Server idle timeout fires as a read error inside the loop, exiting the loop and closing the
  underlying conn; the local `Read` then sees EOF/closed. Verified by
  `TestPollServerConn_IdleTimeout`.

### TaggedPipe

- `ReadTagged` on closed local end: `io.ErrClosedPipe`; on peer-closed channel: `io.EOF`;
  deadline: `os.ErrDeadlineExceeded` (`taggedPipeConn.ReadTagged`).
- `WriteTagged` on closed: `io.ErrClosedPipe` (plus the `recover()` in `WriteTagged`).
- Deadline support is **read-only and simplified**: `SetReadDeadline` stores it; `SetDeadline`
  only applies the *read* deadline and ignores write; `SetWriteDeadline` is a no-op.
  **HAZARD**: `WriteTagged` can block indefinitely with no deadline escape — only `Close` or
  a reader unblocks it. **AMBIGUITY**: `ReadTagged` reads the deadline *without holding `mu`*
  while `SetReadDeadline` writes it under `mu` — a benign data race in practice but
  technically unsynchronized. The deadline timer is also computed once and not re-armed if
  changed mid-read.

---

## Dependencies

**PollConn depends on:**
- `MaxPacketSize` (= 65535, `packet.go`) for its read buffer.
- `Register` / `Wrapper` / `ConnWrapListener` / `ConnWrapDialer` / `Dialer` for the `"poll"`
  driver (`ConnWrapListener` / `ConnWrapDialer` in `wrap.go`; `Dialer` in `driver.go`).
- The underlying `net.Conn`'s optional `MaxWrite()` (forwarded).
- Standard library only otherwise (`net`, `time`, `sync`, `io`, `os`).

**PollConn depended on by:**
- The `"poll"` URI driver consumers (anything composing a transport string with `poll`).
- `proto/dnst` integration: full client stack TCP → DNST → DemuxClient → **PollConn**
  (`proto/dnst/dnst_conn_int_test.go`). PollConn is the *outermost* client wrapper that turns
  the DNS request/response tunnel into a stream.

**TaggedConn / TaggedPipe depended on by (the contract is widely relied upon):**
- `taggedDemux` / `taggedDemuxSess` (`demux_tagged.go`).
- `Mux` produces a `TaggedConn` (`NewMux` in `mux.go`).
- dnst `serverConn` / `taggedServerConn` *are* `TaggedConn`s and consume an inner
  `TaggedConn` (`proto/dnst/dnst_conn.go`).
- `wrap.go` pipeline typing has a first-class `PipeTypeTaggedConn`; `demux.go` has a
  `TaggedToListener` adapter (in the `NewTaggedDemux` wrapper).

**External:** dnst uses `github.com/miekg/dns` for tag = `*dns.Msg`
(`proto/dnst/dnst_conn.go`).

---

## Change hazards (MOST IMPORTANT)

1. **Poll interval too low ⇒ busy-poll / load amplification.** Default is 1ms (set in
   `NewPollConn`). When idle, the client emits a full request/response round-trip every
   `interval` on the underlying transport. On a real network (DNS over UDP/TCP) a
   sub-millisecond interval is a self-inflicted flood and burns CPU on `time.After`
   allocation every iteration. Tune up for production; the tests use 10ms. Changing the
   default affects latency-vs-load for every poll user.

2. **Server push throughput is gated by the client poll rate.** The server drains **one**
   `sendCh` payload per incoming request (see server-loop invariants). Raising `sendq` does
   not raise throughput — it only buffers more before `Write` blocks. Bursty server push
   requires changing the loop logic, not the queue size.

3. **Close drops queued data — no flush.** Both loops exit on `closed` without draining
   `sendCh` (see Lifecycle > drain semantics). A `Write` that returned success can still be
   lost if `Close` races it. Do not assume `Write`+`Close` delivers.

4. **Tag must round-trip read→write and be consumed once** (see the tag carry-through
   invariant). In `taggedDemuxSess` the tag moves `rQueue` → `tagQueue` → write; `tagQueue`
   is sized `sessReadQueueSize*2` (default 128, so 256; see `taggedDemux.processPacket`). If
   reads and writes get out of balance, tags accumulate or starve: `taggedDemuxSess.Write`
   *blocks* pulling from `tagQueue` if no tag is pending. **A write without a prior read can
   deadlock** — the escape hatch on the demux session is `SetWriteDeadline` (its `Write`
   honours a `writeDeadline`, unlike `TaggedPipe` where `SetWriteDeadline` is a no-op). Any
   change that breaks the 1-read ⇒ 1-write tag accounting will corrupt DNS query/response
   correlation.

5. **`WriteTagged` tag-type contract is unchecked at the interface but enforced per
   implementation.** `mux.WriteTagged` requires `tag.(net.Conn)` non-nil; dnst requires
   `*dns.Msg` (`serverConn.WriteTagged`) or `serverConnTagged` with non-nil `dnsMsg`
   (`taggedServerConn.WriteTagged`). Passing a `nil` tag (as `taggedPipeConn.Write` does) or
   the wrong type is a runtime error, not a compile error. **HAZARD**: plain
   `serverConn.ReadTagged` does `*tag = m` with **no nil check** (unlike
   `taggedServerConn.ReadTagged` which guards `if tag != nil`, and `mux`/`taggedPipe` which
   also guard), so `serverConn.ReadTagged(b, nil)` will **panic** (nil pointer deref). In
   practice no in-tree caller hits this — `taggedDemux.readLoop` and the poll stack always
   pass a non-nil `*tag` — so it bites only a caller who hand-writes `ReadTagged` with nil.
   Keep the nil-guard convention when adding implementations.

6. **Deadline interaction with the loop.** A `Read` deadline only times out the *waiting
   caller*; the loop keeps polling independently. A `Write` deadline only bounds enqueue time,
   not transmit time (hazard #3 corollary). The server idle `timeout` is a *different*
   mechanism (underlying read deadline) that tears down the whole connection, not a per-call
   deadline — don't conflate `WithPollTimeout` (server, kills conn) with `SetReadDeadline`
   (per-call).

7. **No configurable read buffer.** The loop's read buffer is fixed at `MaxPacketSize`
   (65535); there is **no** `WithPollBufSize` option. If the underlying conn delivers a single
   read larger than 65535 (a stream conn without framing) the excess is truncated/lost on that
   read. PollConn assumes a *message/framed* underlying conn — the tests wrap raw `net.Pipe`
   in `FrameConn` precisely for this (see `newPollPair`). Pairing PollConn directly over an
   unframed byte stream breaks the one-request/one-response and empty-poll assumptions.

8. **Empty writes must round-trip on the underlying transport.** Idle polling writes
   zero-length payloads. If the underlying conn collapses empty writes (e.g. framing that
   drops zero-length frames) the server never sees the poll and never replies, stalling
   server-initiated delivery. The test pipe deliberately round-trips empty messages
   (`msgPipeConn.Write`).

9. **TaggedPipe writes have no deadline escape.** `SetWriteDeadline` is a no-op; a
   `WriteTagged` with no reader blocks until `Close`. Any code expecting write timeouts on a
   TaggedPipe will hang. (Contrast hazard #4: the demux session *does* honour a write
   deadline.)

10. **Driver param side-restriction lives only in the URI parser.** `interval` is client-only
    and `timeout` is server-only *only when parsed from a URI*. Direct option calls have no
    such guard and silently no-op on the wrong side. Don't rely on options to validate side
    correctness.

---

## Tests covering this

Run from the **repo root** module (covers `poll_conn_test.go`, `demux_tagged_test.go`,
`mux_test.go`):

```bash
go test -run TestPollConn .          # client-only loop
go test -run TestPollServerConn .    # full client+server pairing
go test -run TestTaggedDemux .       # tag round-trip / nil tag / close
```

The dnst integration test is in a **separate module** — `go test ./...` from root misses it:

```bash
cd proto/dnst && go test -run TestDNST .   # PollConn over the full DNST+demux stack
```

**Canonical pairing recipe** for testing PollConn changes: `newPollPair` (`poll_conn_test.go`)
wraps both ends of a `net.Pipe` in `NewFrameConn` (for message boundaries) then in
`NewPollConn` / `NewPollServerConn`. Hand-wiring PollConn over a raw byte stream (no framing)
will not work — see hazard #7.

**Behaviors covered**: write/read echo, server-initiated delivery via idle poll, post-close
errors on `Read`+`Write`, read deadline firing, partial reads (`unread` reassembly), server
idle timeout (`WithPollTimeout` closes within `5*timeout`), bidirectional exchange, and the
key **tag round-trip** assertion in `TestTaggedDemux_Basic` (a tag set on the client
`WriteTagged` is echoed back and the client `ReadTagged` sees the *same* tag — the contract
dnst relies on). `TestTaggedDemux_MultipleSessions` exercises a nil tag.

**Notable gaps**: no test for **send-queue or recv-queue backpressure** (filling `sendCh` /
`recvCh` to block `Write` / the loop); no test pinning the **specific** post-close error (EOF
vs `net.ErrClosed`, hazard #6 non-determinism) or **data loss on Close with queued sendCh**
(hazard #3); `serverConn.ReadTagged(b, nil)` nil-panic (hazard #5) is untested; TaggedPipe's
`req.done` `default`-goroutine fallback is only exercised indirectly via demux tests.

---

## Related docs

- `pipeline.md` — how `Wrapper`/`PipeType*` compose layers (the `poll` driver and
  `TaggedConn` typing plug in here; `wrap.go`).
- `mux.md` — `Mux` as a `TaggedConn` producer (tag = `net.Conn`).
- `demux.md` — `taggedDemux`/`taggedDemuxSess`, the primary tag read→write consumer.
- `stream-transforms.md` — `FrameConn`, `SplitConn`, `aesgcm` and `MaxWrite`/`MaxPacketSize`
  interplay that PollConn forwards and relies on.
- `drivers-proto.md` — the `dnst` proto/driver, the motivating tag = `*dns.Msg` case and the
  full PollConn↔DNST↔Demux stack.
