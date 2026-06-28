# Demux / TaggedDemux / DemuxClient

> Scope: the demux family of session multiplexers in package `netx`.
> Files: `demux.go`, `demux_client.go`, `demux_dialer.go`, `demux_listener.go`, `demux_tagged.go` (+ tests `demux_test.go`, `demux_tagged_test.go`).
> Code is cited by symbol + file (line numbers rot). Bare line numbers appear only for things with no stable symbol, and name the enclosing function/struct.

## Purpose & role

`Demux` turns a *single* `net.Conn` into many virtual connections ("sessions"), exposed as a `net.Listener`. Each packet on the wire is `[fixed-length session ID prefix][payload]` (see the package doc comment at the top of `demux.go` and `demux.readLoop`). The read loop strips the ID prefix and routes the payload to the per-session read queue keyed by the raw ID bytes (`demux.readLoop` / `demux.processPacket`). `Accept` hands out a new session the first time an unseen ID arrives (`demux.Accept`, `demux.processPacket`).

`DemuxClient` is the peer side: it produces a `Dialer` whose returned `net.Conn` prepends a fixed ID on `Write` and verifies/strips it on `Read` (`NewDemuxClient`, `demuxClient.Read`, `demuxClient.Write`).

`TaggedDemux` is the tag-aware variant over a `TaggedConn` (`NewTaggedDemux`). It carries an opaque `tag any` captured on the read path through to the matching `Write` on the session, so a response reuses the request's context. This is the mechanism the DNS-tunnel path relies on to associate a reply with the originating query (`taggedDemux.processPacket`, `taggedDemuxSess.Read`/`taggedDemuxSess.Write`). See `poll-tagged.md` for the `TaggedConn` contract itself (`TaggedConn` interface in `tagged_conn.go`).

It registers the `demux` URI driver in `init()` (`init` in `demux.go`).

## Commands

Tests for both variants live in the root module (package `netx_test`). From the repo root:

```bash
go test -run TestDemux -v .          # plain demux
go test -run TestTaggedDemux -v .    # tagged demux
```

Representative URI (server and client share the same `id`, which is **hex**; drivers `hex.DecodeString` it):

```
tcp+frame+demux://addr?id=deadbeef&accq=4&rq=128
```

`accq`/`rq` are listener-only params (see Driver params).

## Public API (exported symbols + options + driver params)

### Constructors
- `NewDemux(c net.Conn, idMask uint8, opts ...DemuxOption) (net.Listener, error)`.
  - `idMask` is the **ID prefix length in bytes**, stored as `idMask int` in `demuxCore`. There is no protocol-level ID; routing is purely positional on the first `idMask` bytes.
  - Starts the read-loop goroutine before returning (`go m.readLoop()` in `NewDemux`).
  - If the underlying conn exposes `MaxWrite() uint16` (and it is non-zero), demux computes its own advertised `maxWrite = underlying.MaxWrite() - idMask`, erroring if the underlying `MaxWrite() <= idMask` (in `NewDemux`). This accounts for the ID bytes the session must prepend.
- `NewTaggedDemux(c TaggedConn, idMask uint8, opts ...DemuxOption) (net.Listener, error)`. Same shape, over `TaggedConn`; same `MaxWrite` adjustment.
- `NewDemuxClient(c net.Conn, id []byte) Dialer`. Returns a `Dialer` (`type Dialer = func() (net.Conn, error)`, in `mux_client.go`). The **`id` value itself is the ID prefix**; its length (`len(id)`) is the per-direction framing length. Same `MaxWrite` reduction logic.
- `NewDemuxDialer(d Dialer, id []byte) Dialer`. Thin adapter: dials the inner `Dialer`, then wraps with `NewDemuxClient(c, id)()`.
- `NewDemuxListener(l net.Listener, idMask uint8, opts ...DemuxOption) net.Listener`. **Different semantics** from `NewDemux`: instead of demuxing one already-muxed conn, it runs a *separate* `NewDemux` per accepted underlying connection, flattening all their sessions into one accept queue (`demuxListener.Accept`). This lets per-remote-address routing coexist with in-packet ID routing (see the doc comment on `NewDemuxListener`).

### Options (`DemuxOption = func(*demuxCore)`)
- `WithDemuxAccQueue(size uint16)`. Sets accept-queue capacity. **Default 1** (set in `NewDemux`/`NewTaggedDemux` via `make(chan net.Conn, 1)`).
- `WithDemuxReadQueue(size uint16)`. Sets per-session read-queue depth. **Default 128** (the `sessReadQueueSize: 128` literal in `NewDemux`/`NewTaggedDemux`).
- `WithDemuxLogger(logger Logger)`. Default `slog.Default()`. `Logger` is the 4-method context logger (`Logger` interface in `logger.go`).

Options mutate the shared `demuxCore` and are applied **after** the defaults and after the `MaxWrite` computation (the `for _, o := range opts` loop in `NewDemux`/`NewTaggedDemux`).

### Driver params (`Register("demux", ...)` in `demux.go`)
- `id` — **required**, hex-decoded via `hex.DecodeString`. Its decoded byte length is the ID length passed as `idMask` to listeners and the literal ID bytes for dialers. Empty/absent `id` is rejected.
- `accq` — accept queue size, `uint16`, **listener-only** (errors for dialers) → `WithDemuxAccQueue`.
- `rq` — session read queue size, `uint16`, **listener-only** → `WithDemuxReadQueue`.
- Any other key → error.
- The driver wires up listener wrappers `ConnToListener` / `TaggedToListener` / `ListenerToListener` and dialer wrappers `ConnToDialer` / `DialerToDialer` (`Wrapper` struct in `wrap.go`). **Note:** there is no `ListenerToConn`/`TaggedToConn` etc. — demux is always a listener-producing wrapper on the server side and a dialer-producing wrapper on the client side.

### Exported behavioral surface on sessions
Sessions implement `net.Conn`. Notable extras:
- `MaxWrite() uint16` on `demuxSess` / `taggedDemuxSess` / `demuxClient`.
- `demuxClient.ID() []byte`.
- **Address augmentation is asymmetric:**
  - Server sessions (`demuxSess`/`taggedDemuxSess`) override **`RemoteAddr`** to return a `demuxVirtualAddr` whose `String()` is `underlyingRemoteAddr + ":" + hex(id)` (`demuxSess.RemoteAddr`, `taggedDemuxSess.RemoteAddr`, `demuxVirtualAddr.String`). Their `LocalAddr` is the plain underlying addr.
  - The client (`demuxClient`) overrides **`LocalAddr`** (not `RemoteAddr`) with the id-augmented `demuxVirtualAddr` (`demuxClient.LocalAddr`). It has no `RemoteAddr` override, so `RemoteAddr` delegates to the embedded `net.Conn` and is *not* id-augmented.

## Internal design & invariants

### ID-prefix framing
- **Server read**: `id = data[:idMask]`, `payload = data[idMask:]` (`demux.readLoop`, `taggedDemux.readLoop`). A packet shorter than `idMask` is **silently ignored** (logged at Debug) and the loop continues.
- **Server write** (session→wire): `payload = append(s.id, b...)` then a single `bc.Write` (`demuxSess.Write`, `taggedDemuxSess.Write`). Returns `n - len(s.id)` so the caller sees only payload bytes written. A short write `< len(s.id)` is reported as `io.ErrShortWrite`. The `append(s.id, b...)` form can alias `s.id`'s backing array — see Change hazards #5.
- **Client write**: fresh buffer, `copy(id)` + `copy(payload)`, single `Conn.Write` (`demuxClient.Write`; the comment there explains the deliberate fresh allocation).
- **Client read**: reads into a pooled buffer; if `n == 0` returns `(0, nil)` as a "no-data cycle" (e.g. empty DNS TXT); if `n < len(id)` → `io.ErrUnexpectedEOF`; if `buf[:len(id)] != id` → error `"demuxClient: received packet with mismatched ID"`; else copies `buf[len(id):n]` into the caller buffer and returns `n - len(id)` (`demuxClient.Read`).

### Session map
- `sessions map[string]*demuxSess` keyed by `string(id)` (raw bytes, not hex) (`demux` struct, `demux.processPacket`). Tagged: `map[string]*taggedDemuxSess`.
- Guarded entirely by `m.mu sync.Mutex`. `m.sessions == nil` is the "closed" sentinel (checked in `demux.processPacket`, set in `demux.Close`).

### Accept queue
- `accQueue chan net.Conn` in `demuxCore`. On a brand-new ID a session is created and a **non-blocking** send is attempted; if full, the session is **dropped** (deleted from the map, warning logged) so the read loop never blocks (`demux.processPacket`). The packet that created it is therefore lost along with the session. See Change hazards #2 for the drop-on-full rationale.

### Per-session read queue & backpressure
- `rQueue chan []byte` sized `sessReadQueueSize` (default 128) (created in `demux.processPacket`). Tagged: `rQueue chan taggedDemuxPacket`.
- Payload delivery is **non-blocking**: if the session's `rQueue` is full, the packet is **dropped** (warning logged), never blocking the read loop (`demux.processPacket`, `taggedDemux.processPacket`). This is lossy by design (see Change hazards #2; covered by `TestDemux_DroppedPackets`).
- `demuxSess.Read` drains `rQueue`; partial reads stash the remainder in `s.unread` and return it first next call.

### Tagged variant tag carry-through
- Read loop captures `tag` per packet via `ReadTagged(buf, &tag)` and enqueues `taggedDemuxPacket{data, tag}` (`taggedDemux.readLoop`, `taggedDemux.processPacket`).
- On `Read`, after dequeuing a packet, the tag is pushed onto a **separate** `tagQueue chan any` (sized `sessReadQueueSize*2`) *before* the data is returned to the caller. This push is `select`-guarded against `s.closed` (`taggedDemuxSess.Read`).
- On `Write`, the session **consumes one tag** from `tagQueue` and passes it to `WriteTagged(payload, tag)` (`taggedDemuxSess.Write`). Write blocks until a tag is available (or close/deadline).
- **Invariant: tag is produced exactly once per successful `Read` and consumed exactly once per `Write`.** This couples Read and Write call counts: the response model assumes one write per read. See Change hazards #3.

## Concurrency model

### Goroutines
- One **read-loop goroutine per demux**, started in the constructor (`go m.readLoop()`). It is the *sole* reader of the underlying conn and the *sole* writer of the session map's queues.
- `demuxListener` adds: one dispatcher goroutine, started lazily on the **first `Accept`** via `sync.Once` (`demuxListener.Accept`), plus one goroutine per accepted underlying conn, each forwarding that conn's demux `Accept`s into the shared `accQueue chan demuxAccept`. When the underlying `Listener.Accept` errors, the dispatcher returns and `defer close(dl.accQueue)` closes the shared queue, so all `demuxListener.Accept` callers then get `net.ErrClosed`.
- Session `Read`/`Write` run on **caller** goroutines.

### Locks & channels
- `m.mu` protects `sessions` and the create-or-drop logic; held across both the accept-queue send attempt and the rQueue send attempt (`demux.processPacket`). The read loop therefore holds `m.mu` while doing non-blocking channel sends only — it never blocks under the lock.
- Session-level `s.mu` protects `unread`, deadlines, and `readDlNotify` (`demuxSess.Read`, `demuxSess.SetReadDeadline`).
- `closing atomic.Bool` makes `Close` idempotent for both demux and sessions (`demux.Close`, `demuxSess.Close`, `taggedDemux.Close`, `taggedDemuxSess.Close`).
- Read deadline changes are signaled by **closing and replacing** `readDlNotify`, waking a blocked `Read` so it re-reads the new deadline (`demuxSess.Read`, `demuxSess.SetReadDeadline`; same pattern in tagged).

### Backpressure direction
- **There is no backpressure to the underlying conn.** Both the accept queue and per-session queues use non-blocking sends with drop-on-full. The read loop always drains the wire as fast as possible; slow consumers lose packets rather than stalling the loop. See Change hazards #2.
- **Exception — TaggedDemux write path.** `taggedDemuxSess.Write` **blocks** waiting for a tag from `tagQueue`. Tags are only produced by `Read`. So a Write with no preceding Read blocks until a Read happens, the session closes, or the (optional) write deadline fires. This is a deliberate request/response coupling, not wire backpressure. See Change hazards #3.

## Lifecycle & ownership

### Session creation
- Lazily, in `processPacket`, the first time a given ID is seen. Created session is registered, pushed to `accQueue`, and (on success) handed to the next `Accept`.
- **Unknown/new ID = new session.** There is no handshake; any first packet with a novel prefix spawns a session. See Change hazards #7 (resource-exhaustion vector on untrusted transports).

### Session teardown
- `demuxSess.Close()`: idempotent; under `m.mu` it **closes `rQueue`** and deletes itself from the map (`demuxSess.Close`). Closing `rQueue` makes a blocked/future `Read` return `io.EOF` (`demuxSess.Read`). Note: it closes `rQueue` *unconditionally* (only the map delete is guarded by `sessions != nil`) — see the latent double-close in Change hazards #4.
- `taggedDemuxSess.Close()`: gates *both* `close(rQueue)` and the delete behind `sessions != nil`, and **always** closes `s.closed` (`taggedDemuxSess.Close`). `closed` unblocks any `Write` waiting on a tag and any in-flight tag push.

### Demux Close vs sessions vs underlying conn
- `demux.Close()`: CAS `closing`; under lock `close(accQueue)`, close every session `rQueue`, set `sessions = nil`; then `bc.Close()` on the **underlying conn**. So closing the listener closes the underlying transport and fans EOF to all sessions.
- The read loop **`defer m.Close()`** (`demux.readLoop`, `taggedDemux.readLoop`): any underlying read error tears down the whole demux (see Error semantics and Change hazards #8).
- **Ownership**: demux owns the underlying conn — it calls `bc.Close()`. `demuxClient` does **not** own its conn in `Close` (it embeds `net.Conn`, so `Close` delegates to the underlying) — every `demuxClient` produced by the `Dialer` shares the *same* underlying `c` (captured in the closure in `NewDemuxClient`); closing one client closes the shared conn for all. See Change hazards #9.

### Orphan sessions
- A session dropped because the accept queue was full is deleted from the map but was **never returned by `Accept`** — its creating packet is lost, and a *later* packet with the same ID will re-create it (`demux.processPacket`).
- After demux `Close`, the read loop has returned; sessions still held by callers will see `io.EOF` on `Read` (queue closed) but their `Write` still targets the now-closed `bc` and returns the underlying conn's error. The plain variant does not currently guarantee EOF/ErrClosed on session reads after Close — see Tests.

## Error & EOF semantics

- **Underlying read error/EOF → whole-demux teardown.** The read loop logs and `return`s on any `bc.Read` error (`demux.readLoop`, `taggedDemux.readLoop`); the deferred `m.Close()` then closes all session `rQueue`s, so every session's pending/next `Read` returns `io.EOF`. EOF is fanned out to all sessions simultaneously — there is no per-session EOF (Change hazards #8). A normal `io.EOF` from the underlying conn is logged at **Error** level (the `ErrorContext` call in `demux.readLoop`) even though it is the expected end-of-stream — noise, not a correctness bug.
- **Queue-full = drop, not error.** Neither accept-queue-full nor read-queue-full surfaces an error to anyone; only a Warn log. Senders get no signal (the underlying transport already "succeeded").
- **Session Read after Close**: `io.EOF` once `rQueue` is closed.
- **Session Write after Close** (plain): `s.id` prepend + `bc.Write`; returns whatever the closed underlying conn returns (typically `net.ErrClosed`/`io.ErrClosedPipe`). No early `closing` check on the write path. Tagged Write returns `net.ErrClosed` via the `s.closed` select.
- **Client Read**: distinguishes empty read (`0, nil`), truncated (`io.ErrUnexpectedEOF`), and ID mismatch (custom error) (`demuxClient.Read`).
- **Packet too large**: `demuxSess.Write` rejects `len(b)+len(s.id) > MaxPacketSize` with `"demux: packet too large"` (`demuxSess.Write`, `taggedDemuxSess.Write`). **`MaxPacketSize = 65535`** (`packet.go`). `demuxClient.Write` has **no such cap** — see Change hazards #6.
- **Deadlines**: past read deadline → `os.ErrDeadlineExceeded` (`demuxSess.Read`); plain Write enforces only a write deadline via `time.Now().After` (non-blocking, `demuxSess.Write`); tagged Write enforces the write deadline against the tag-wait with a real `time.Timer` (`taggedDemuxSess.Write`). **Only the tagged write can actually block on a deadline**; the plain write's deadline is just a pre-check.

## Dependencies

### Depends on
- `net.Conn` (plain) / `TaggedConn` (tagged) as the single underlying transport. Optional `interface{ MaxWrite() uint16 }` capability for size budgeting (`NewDemux`, `NewTaggedDemux`, `NewDemuxClient`).
- `MaxPacketSize` constant (`packet.go`); used as read buffer size and as the write size cap.
- `Logger` interface, default `slog.Default()` (`Logger` in `logger.go`).
- `Register`/`Wrapper`/`Dialer` plumbing for the URI driver (`Register` in `driver.go`, `Wrapper` in `wrap.go`, `Dialer` in `mux_client.go`).
- stdlib: `encoding/hex`, `sync`, `sync/atomic`, `time`, `context`, `io`, `os`, `errors`.

### Depended on by
- The URI/driver pipeline via the registered `"demux"` driver. Other layers (encryption, transport mux, poll, dnst) can sit above/below it in the pipeline.
- The DNS-tunnel use case for the tagged variant (request/response correlation via tags).
- `NewDemuxListener` composes `NewDemux` per accepted conn.

### External
- None beyond the Go standard library.

## Change hazards (MOST IMPORTANT)

1. **ID length must match on both ends, exactly, per direction.** Server uses `idMask` (decoded `len(id)` from the URI) as the prefix length; client uses the literal `id` bytes and `len(id)` as its prefix length (`demuxClient.Read`). If the configured `id` differs in **length** between client and server, the server slices the wrong number of prefix bytes and routes every packet to a wrong/garbage session; if it differs in **value**, the client's `Read` rejects responses with `"mismatched ID"`. The driver derives both from the *same* `id` param, so consistency is only guaranteed when both sides share identical URI `id`. Any change to how `id` is parsed (hex vs raw), or to `idMask` sizing, breaks framing silently.

2. **Queue sizing controls loss and memory, not throttling.** Both `accq` and `rq` use drop-on-full with non-blocking sends (`demux.processPacket`). Increasing `rq` raises memory (each slot holds a full payload copy, up to `MaxPacketSize`) and reduces drops but never adds backpressure; decreasing it increases silent packet loss under burst. Tagged `tagQueue` is sized `sessReadQueueSize*2` — if you change `rq` you implicitly change tag buffering. There is **no head-of-line blocking across sessions** precisely because the loop never blocks; do not "fix" the drops by making the rQueue send blocking — that would let one slow session stall the read loop and starve **all** sessions (and, for net.Pipe-style transports, deadlock).

3. **Tagged: tag must be produced once per Read and consumed once per Write.** `Read` pushes a tag; `Write` pops one (`taggedDemuxSess.Read`/`taggedDemuxSess.Write`). A consumer that writes without reading **blocks**; one that reads twice then writes once leaves a stale tag that the *next* write will (incorrectly) reuse — responses could be correlated to the wrong request. `tagQueue` capacity `sessReadQueueSize*2` only bounds the imbalance before drops/blocking; it does not enforce 1:1. Any refactor of the Read/Write pairing, or adding extra internal reads, can desynchronize tags. Cross-layer: the dnst/poll-tagged layer below relies on the same tag surviving round-trip (see `poll-tagged.md`).

4. **Plain `demux` has a latent double-close of `rQueue`.** `demux.Close()` closes every session's `rQueue` while `demuxSess.Close()` *unconditionally* closes its own `rQueue` (the `sessions != nil` guard only protects the map delete). A session `Close()` racing after listener `Close()` can `close` an already-closed channel → **panic**. The `s.closing` CAS prevents a session closing its own queue twice, but does **not** coordinate with demux `Close()` closing the same queue. The tagged variant already guards both `close(rQueue)` and the delete behind `sessions != nil` (`taggedDemuxSess.Close`), so after demux `Close` niled the map, session `Close` skips closing `rQueue`. The plain and tagged Close paths are inconsistent; recommend aligning the plain path with the tagged path. Do not add session-Close logic without auditing this race.

5. **Write-path buffer aliasing inconsistency.** `demuxSess.Write`/`taggedDemuxSess.Write` use `append(s.id, b...)`, which can mutate `s.id` if it has spare capacity; `demuxClient.Write` deliberately allocates a fresh buffer to avoid this (see its comment). The session path is currently safe only because `s.id` is a tightly-sliced sub-array of the per-packet read buffer (`data[:m.idMask]` from a freshly `make`d `data` in the read loop), but any change to how `s.id` is created (e.g. reusing a larger buffer with spare cap) would corrupt the routing prefix on concurrent/subsequent writes. Prefer the explicit-copy pattern from `demuxClient.Write` everywhere.

6. **No write-size cap on the client; cap only on sessions.** `demuxSess.Write` rejects `len(b)+len(id) > MaxPacketSize`; `demuxClient.Write` does not. Oversized client writes can produce packets the transport rejects or that exceed peer framing assumptions. Keep these symmetric if you tighten one.

7. **Sessions are created by any novel ID with no handshake.** First packet with an unseen prefix spawns a session (`demux.processPacket`). Unauthenticated/noisy transports can create churn; the only bound is distinct IDs and the drop-on-full accept queue. Putting demux directly on an untrusted raw transport is a resource-exhaustion vector — usually it sits above an encryption/auth layer in the pipeline.

8. **EOF is whole-demux, not per-session.** Any underlying read error closes the entire listener and all sessions (`demux.readLoop` + deferred `Close`). A normal `io.EOF` is logged at Error level. Code that expects to keep some sessions alive after a transient underlying error will be surprised — the model is "single physical conn ⇒ shared fate."

9. **`demuxClient` Close closes the shared underlying conn.** All clients from one `NewDemuxClient` `Dialer` share one `c` (captured in the closure); `Close` delegates to the embedded `net.Conn`. Closing any "virtual" client kills the real transport for all. Adding multi-session client semantics would require decoupling Close from the shared conn.

## Tests

Tests are in `demux_test.go` and `demux_tagged_test.go` (package `netx_test`). They cover: round-trip with ID prepend, multiple concurrent sessions, drop-on-full read queue (`TestDemux_DroppedPackets`), undersized-packet skip (`TestDemux_InvalidPacket`), read deadlines, and — for tagged — the response-tag-equals-request-tag invariant (`TestTaggedDemux_Basic`) plus post-Close Read/Write/Accept errors (`TestTaggedDemux_Close`).

**Not covered (mind these when changing the code):**
- Plain `demux` post-Close session Read/Write semantics (`TestDemux_Close` itself notes "demux does not currently guarantee EOF/ErrClosed on session reads after Close" — that test comment is intentionally acknowledging the gap, not asserting behavior).
- Accept-queue-full drop path (Change hazards #2/#4); the `accq=4` in tests avoids it.
- The plain double-close-of-`rQueue` race (Change hazards #4).
- Tag desynchronization (write-without-read blocking, double-read single-write) (Change hazards #3).
- `demuxClient` ID-mismatch and truncated-read error paths.
- `MaxWrite` budgeting and the `MaxWrite <= idMask` error.
- `NewDemuxListener` per-conn fan-in and `NewDemuxDialer` — no test in these files.

## Source nits (NOT being fixed in this pass — flagged so docs don't restate the wrong names)

These are stale comments/names in the *source* that a future editor may correct; the docs above use the real symbols:
- `NewTaggedDemux`'s godoc comment reads `// NewDemuxTagged creates...` — the actual function is `NewTaggedDemux`.
- The three option godocs use old names: `// WithAccQueueSize`, `// WithReadQueueSize`, `// WithLogger` — the actual symbols are `WithDemuxAccQueue`, `WithDemuxReadQueue`, `WithDemuxLogger`.

## Related docs
- `pipeline.md` — how the `demux` driver/`Wrapper` slots into the URI layer pipeline.
- `mux.md` — the complementary multiplexer (server side counterpart) over a listener.
- `poll-tagged.md` — `TaggedConn` contract and the poll/dnst tag producers the tagged demux consumes.
- `stream-transforms.md` — framing/encryption layers that typically sit below demux and may provide `MaxWrite()`.
- `drivers-proto.md` — driver registration conventions and URI parameter parsing.
- `../mux-tag-poll.md` — existing overview doc touching mux/tag/poll.
