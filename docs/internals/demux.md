# Demux / TaggedDemux / DemuxClient

> Scope: the demux family of session multiplexers in package `netx`.
> Files: `demux.go`, `demux_client.go`, `demux_dialer.go`, `demux_listener.go`, `demux_tagged.go` (+ tests `demux_test.go`, `demux_tagged_test.go`).
> Every claim below is grounded with `file:line`. Ambiguities and gaps are flagged inline as **[GAP]** / **[AMBIGUITY]**.

## Purpose & role

`Demux` turns a *single* `net.Conn` into many virtual connections ("sessions"), exposed as a `net.Listener`. Each packet on the wire is `[fixed-length session ID prefix][payload]` (`demux.go:1-4`, `demux.go:201-202`). The read loop strips the ID prefix and routes the payload to the per-session read queue keyed by the raw ID bytes (`demux.go:208-239`). `Accept()` hands out a new session the first time an unseen ID arrives (`demux.go:160-166`, `demux.go:214-231`).

`DemuxClient` is the peer side: it produces a `Dialer` whose returned `net.Conn` prepends a fixed ID on `Write` and verifies/strips it on `Read` (`demux_client.go:17-79`).

`TaggedDemux` is the tag-aware variant over a `TaggedConn` (`demux_tagged.go:33-55`). It carries an opaque `tag any` captured on the read path through to the matching `Write` on the session, so a response reuses the request's context. This is the mechanism the DNS-tunnel path relies on to associate a reply with the originating query (`demux_tagged.go:106-138`, `demux_tagged.go:160-275`). See `poll-tagged.md` for the `TaggedConn` contract itself (`tagged_conn.go:11-38`).

It registers the `demux` URI driver in `init()` (`demux.go:24-89`).

## Public API (exported symbols + options + driver params)

### Constructors
- `NewDemux(c net.Conn, idMask uint8, opts ...DemuxOption) (net.Listener, error)` — `demux.go:136-158`.
  - `idMask` is the **ID prefix length in bytes**, stored as `idMask int` (`demux.go:100`, `demux.go:144`). There is no protocol-level ID; routing is purely positional on the first `idMask` bytes.
  - Starts the read-loop goroutine before returning (`demux.go:156`).
  - If the underlying conn exposes `MaxWrite() uint16` (and it is non-zero), demux computes its own advertised `maxWrite = underlying.MaxWrite() - idMask`, erroring if the underlying `MaxWrite() <= idMask` (`demux.go:147-152`). This accounts for the ID bytes the session must prepend.
- `NewTaggedDemux(c TaggedConn, idMask uint8, opts ...DemuxOption) (net.Listener, error)` — `demux_tagged.go:33-55`. Same shape, over `TaggedConn`; same `MaxWrite` adjustment (`demux_tagged.go:44-49`). Doc comment says "NewDemuxTagged" but the symbol is `NewTaggedDemux` (`demux_tagged.go:29`) — **[doc/typo mismatch, harmless]**.
- `NewDemuxClient(c net.Conn, id []byte) Dialer` — `demux_client.go:17-37`. Returns a `Dialer` (`type Dialer = func() (net.Conn, error)`, `mux_client.go:28`). The **`id` value itself is the ID prefix**; its length (`len(id)`) is the per-direction framing length. Same `MaxWrite` reduction logic (`demux_client.go:29-34`).
- `NewDemuxDialer(d Dialer, id []byte) Dialer` — `demux_dialer.go:5-13`. Thin adapter: dials the inner `Dialer`, then wraps with `NewDemuxClient(c, id)()`.
- `NewDemuxListener(l net.Listener, idMask uint8, opts ...DemuxOption) net.Listener` — `demux_listener.go:25-33`. **Different semantics** from `NewDemux`: instead of demuxing one already-muxed conn, it runs a *separate* `NewDemux` per accepted underlying connection, flattening all their sessions into one accept queue (`demux_listener.go:35-67`). This lets per-remote-address routing coexist with in-packet ID routing (`demux_listener.go:20-24`).

### Options (`DemuxOption = func(*demuxCore)`, `demux.go:107`)
- `WithDemuxAccQueue(size uint16)` — `demux.go:111-115`. Sets accept-queue capacity. **Default 1** (`demux.go:143`; doc comment at `demux.go:109` says default 1).
- `WithDemuxReadQueue(size uint16)` — `demux.go:119-123`. Sets per-session read-queue depth. **Default 128** (`demux.go:144`).
- `WithDemuxLogger(logger Logger)` — `demux.go:126-130`. Default `slog.Default()` (`demux.go:141`). `Logger` is the 4-method context logger (`logger.go:5-10`).

Options mutate the shared `demuxCore` and are applied **after** the defaults and after the `MaxWrite` computation (`demux.go:153-155`, `demux_tagged.go:50-52`).

### Driver params (`Register("demux", ...)`, `demux.go:24-89`)
- `id` — **required**, hex-decoded via `hex.DecodeString` (`demux.go:30-35`). Its decoded byte length is the ID length passed as `idMask` to listeners (`demux.go:67`, `demux.go:70`, `demux.go:73`) and the literal ID bytes for dialers (`demux.go:82`, `demux.go:85`). Empty/absent `id` is rejected (`demux.go:58-60`).
- `accq` — accept queue size, `uint16`, **listener-only** (errors for dialers) → `WithDemuxAccQueue` (`demux.go:36-44`).
- `rq` — session read queue size, `uint16`, **listener-only** → `WithDemuxReadQueue` (`demux.go:45-53`).
- Any other key → error (`demux.go:54-55`).
- The driver wires up listener wrappers `ConnToListener` / `TaggedToListener` / `ListenerToListener` (`demux.go:66-74`) and dialer wrappers `ConnToDialer` / `DialerToDialer` (`demux.go:81-86`). `Wrapper` shape: `wrap.go:119-141`. **Note:** there is no `ListenerToConn`/`TaggedToConn` etc. — demux is always a listener-producing wrapper on the server side and a dialer-producing wrapper on the client side.

### Exported behavioral surface on sessions
Sessions implement `net.Conn`. Notable extras:
- `MaxWrite() uint16` on `demuxSess` / `taggedDemuxSess` / `demuxClient` (`demux.go:256-258`, `demux_tagged.go:156-158`, `demux_client.go:39`).
- `demuxClient.ID() []byte` (`demux_client.go:81`).
- `RemoteAddr()` returns a `demuxVirtualAddr` whose `String()` is `underlyingAddr + ":" + hex(id)` (`demux.go:385-396`, `demux_tagged.go:317-319`, `demux_client.go:82-84`).
  **[AMBIGUITY/contradiction]** The test comment at `demux_test.go:52-54` claims `demuxSess.RemoteAddr()` returns the plain underlying addr; the actual code returns the ID-augmented virtual addr (`demux.go:385-387`). Code wins; the comment is stale.

## Internal design & invariants

### ID-prefix framing
- **Server read**: `id = data[:idMask]`, `payload = data[idMask:]` (`demux.go:201-202`, `demux_tagged.go:99-100`). A packet shorter than `idMask` is **silently ignored** (logged at Debug) and the loop continues (`demux.go:196-200`, `demux_tagged.go:94-98`).
- **Server write** (session→wire): `payload = append(s.id, b...)` then a single `bc.Write` (`demux.go:332-335`, tagged: `demux_tagged.go:263-266`). Returns `n - len(s.id)` so the caller sees only payload bytes written (`demux.go:343`, `demux_tagged.go:274`). A short write `< len(s.id)` is reported as `io.ErrShortWrite` (`demux.go:340-342`, `demux_tagged.go:271-273`).
  **[HAZARD — buffer aliasing]** `append(s.id, b...)` may reuse `s.id`'s backing array if it has spare capacity, mutating the shared ID. `s.id` here is a sub-slice of the read buffer copy (`data[:m.idMask]` from a freshly `make`d `data`, `demux.go:193-201`), so its capacity extends into the payload region of *that* packet's buffer — but that buffer is unique per packet and `s.id` is captured once at session creation (`demux.go:217`). In practice the append target is the session's own `id`. Contrast `demuxClient.Write`, which **deliberately allocates a fresh buffer** to avoid exactly this aliasing (`demux_client.go:65-70` + comment). The two write paths are inconsistent; the session path relies on `s.id` having no usable spare capacity. See Change hazards.
- **Client write**: fresh buffer, `copy(id)` + `copy(payload)`, single `Conn.Write` (`demux_client.go:65-79`).
- **Client read**: reads into a pooled buffer; if `n == 0` returns `(0,nil)` as a "no-data cycle" (e.g. empty DNS TXT) (`demux_client.go:50-54`); if `n < len(id)` → `io.ErrUnexpectedEOF` (`demux_client.go:55-57`); if `buf[:len(id)] != id` → error `"mismatched ID"` (`demux_client.go:58-60`); else copies `buf[len(id):n]` into the caller buffer and returns `n - len(id)` (`demux_client.go:61-62`).

### Session map
- `sessions map[string]*demuxSess` keyed by `string(id)` (raw bytes, not hex) (`demux.go:95`, `demux.go:214`, `demux.go:223`). Tagged: `map[string]*taggedDemuxSess` (`demux_tagged.go:25`).
- Guarded entirely by `m.mu sync.Mutex` (`demux.go:93`, `demux.go:209-238`). `m.sessions == nil` is the "closed" sentinel (`demux.go:177`, `demux.go:210-213`).

### Accept queue
- `accQueue chan net.Conn` (`demux.go:101`). On a brand-new ID a session is created and a **non-blocking** send is attempted; if full, the session is **dropped** (deleted from the map, warning logged) so the read loop never blocks (`demux.go:224-230`). The packet that created it is therefore lost along with the session.

### Per-session read queue & backpressure
- `rQueue chan []byte` sized `sessReadQueueSize` (default 128) (`demux.go:219`, `demux.go:248`). Tagged: `rQueue chan taggedDemuxPacket` (`demux_tagged.go:117`, `demux_tagged.go:146`).
- Payload delivery is **non-blocking**: if the session's `rQueue` is full, the packet is **dropped** (warning logged), never blocking the read loop (`demux.go:232-237`, `demux_tagged.go:132-137`). This is lossy by design — confirmed by `TestDemux_DroppedPackets` (`demux_test.go:217-294`).
- `demuxSess.Read` drains `rQueue`; partial reads stash the remainder in `s.unread` and return it first next call (`demux.go:260-317`).

### Tagged variant tag carry-through
- Read loop captures `tag` per packet via `ReadTagged(buf, &tag)` and enqueues `taggedDemuxPacket{data, tag}` (`demux_tagged.go:83-102`, `demux_tagged.go:133`).
- On `Read`, after dequeuing a packet, the tag is pushed onto a **separate** `tagQueue chan any` (sized `sessReadQueueSize*2`) *before* the data is returned to the caller (`demux_tagged.go:200-212`). This push is `select`-guarded against `s.closed` (`demux_tagged.go:200-204`).
- On `Write`, the session **consumes one tag** from `tagQueue` and passes it to `WriteTagged(payload, tag)` (`demux_tagged.go:246-266`). Write blocks until a tag is available (or close/deadline) (`demux_tagged.go:247-257`).
- **Invariant: tag is produced exactly once per successful `Read` and consumed exactly once per `Write`.** This couples Read and Write call counts: the response model assumes one write per read. See Change hazards.

## Concurrency model

### Goroutines
- One **read-loop goroutine per demux**, started in the constructor (`demux.go:156`, `demux_tagged.go:53`). It is the *sole* reader of the underlying conn and the *sole* writer of the session map's queues (`demux.go:182-206`).
- `demuxListener` adds: one dispatcher goroutine (started once via `sync.Once`), plus one goroutine per accepted underlying conn, each forwarding that conn's demux `Accept()`s into the shared `accQueue chan demuxAccept` (`demux_listener.go:35-61`).
- Session `Read`/`Write` run on **caller** goroutines.

### Locks & channels
- `m.mu` protects `sessions` and the create-or-drop logic; held across both the accept-queue send attempt and the rQueue send attempt (`demux.go:209-238`). The read loop therefore holds `m.mu` while doing non-blocking channel sends only — it never blocks under the lock.
- Session-level `s.mu` protects `unread`, deadlines, and `readDlNotify` (`demux.go:250`, `demux.go:261-316`, `demux.go:359-382`).
- `closing atomic.Bool` makes `Close` idempotent for both demux and sessions (`demux.go:169`, `demux.go:347`, `demux_tagged.go:66`, `demux_tagged.go:278`).
- Read deadline changes are signaled by **closing and replacing** `readDlNotify`, waking a blocked `Read` so it re-reads the new deadline (`demux.go:310-315`, `demux.go:366-374`). Same pattern in tagged (`demux_tagged.go:215-219`, `demux_tagged.go:298-306`).

### Backpressure direction
- **There is no backpressure to the underlying conn.** Both the accept queue and per-session queues use non-blocking sends with drop-on-full (`demux.go:224-230`, `demux.go:232-237`). The read loop always drains the wire as fast as possible; slow consumers lose packets rather than stalling the loop. This protects all sessions from one slow session, at the cost of silent loss.
- **Exception — TaggedDemux write path.** `taggedDemuxSess.Write` **blocks** waiting for a tag from `tagQueue` (`demux_tagged.go:247-257`). Tags are only produced by `Read` (`demux_tagged.go:200-201`). So a Write with no preceding Read blocks until a Read happens, the session closes, or the (optional) write deadline fires. This is a deliberate request/response coupling, not wire backpressure.

## Lifecycle & ownership

### Session creation
- Lazily, in `processPacket`, the first time a given ID is seen (`demux.go:214-231`, `demux_tagged.go:112-131`). Created session is registered, pushed to `accQueue`, and (on success) handed to the next `Accept()`.
- **Unknown/new ID = new session.** There is no handshake; any first packet with a novel prefix spawns a session. An attacker or noise on the wire can create sessions (bounded only by distinct IDs and the drop-on-full accept queue).

### Session teardown
- `demuxSess.Close()`: idempotent; under `m.mu` it **closes `rQueue`** and deletes itself from the map (`demux.go:346-357`). Closing `rQueue` makes a blocked/future `Read` return `io.EOF` (`demux.go:297-299`).
  **[HAZARD — double close]** Demux `Close()` also closes every session's `rQueue` (`demux.go:174-176`). If a session `Close()` races *after* demux `Close()` set `sessions = nil`: `demuxSess.Close` still unconditionally `close(s.rQueue)` (it only guards the *map delete* with `sessions != nil`, `demux.go:351-354`), so a channel could be closed twice → **panic**. The `s.closing` CAS prevents a session closing its own queue twice, but does **not** coordinate with demux `Close()` closing the same queue. The tagged variant is safer here: it gates *both* the `close(rQueue)` and the delete behind `sessions != nil` (`demux_tagged.go:281-285`), so after demux `Close` niled the map, session `Close` skips closing `rQueue`. **The plain `demux` and the `taggedDemux` Close paths are inconsistent; the plain path has a latent double-close-of-rQueue race.** See Change hazards.
- `taggedDemuxSess.Close()`: closes `rQueue` (only if `sessions != nil`) and **always** closes `s.closed` (`demux_tagged.go:277-289`). `closed` unblocks any `Write` waiting on a tag and any in-flight tag push (`demux_tagged.go:200-204`, `demux_tagged.go:253-254`).

### Demux Close vs sessions vs underlying conn
- `demux.Close()` (`demux.go:168-180`): CAS `closing`; under lock `close(accQueue)`, close every session `rQueue`, set `sessions = nil`; then `bc.Close()` on the **underlying conn**. So closing the listener closes the underlying transport and fans EOF to all sessions.
- The read loop **`defer m.Close()`** (`demux.go:183`, `demux_tagged.go:80`): any underlying read error tears down the whole demux (see Error semantics).
- **Ownership**: demux owns the underlying conn — it calls `bc.Close()` (`demux.go:179`, `demux_tagged.go:76`). `demuxClient` does **not** own its conn in `Close` (it embeds `net.Conn`, so `Close` delegates to the underlying, `demux_client.go:11`) — **[note]** every `demuxClient` produced by the `Dialer` shares the *same* underlying `c` (captured in the closure, `demux_client.go:17-19`); closing one client closes the shared conn for all. This is fine for the intended single-conn-multiplexed model but is a footgun if callers expect per-dial isolation.

### Orphan sessions
- A session dropped because the accept queue was full is deleted from the map but was **never returned by `Accept`** — its creating packet is lost, and a *later* packet with the same ID will re-create it (`demux.go:224-230`).
- After demux `Close`, the read loop has returned; sessions still held by callers will see `io.EOF` on `Read` (queue closed) but their `Write` still targets the now-closed `bc` and returns the underlying conn's error (`demux.go:335-338`). `TestDemux_Close` explicitly notes "demux does not currently guarantee EOF/ErrClosed on session reads after Close" (`demux_test.go:177-178`) — **[GAP]** see Tests.

## Error & EOF semantics

- **Underlying read error/EOF → whole-demux teardown.** The read loop logs and `return`s on any `bc.Read` error (`demux.go:187-191`, `demux_tagged.go:85-89`); the deferred `m.Close()` then closes all session `rQueue`s, so every session's pending/next `Read` returns `io.EOF` (`demux.go:297-299`, `demux_tagged.go:197-199`). **EOF is fanned out to all sessions simultaneously** — there is no per-session EOF.
- **Note:** a normal `io.EOF` from the underlying conn is logged at **Error** level (`demux.go:189`) even though it is the expected end-of-stream — **[noise/GAP]**, not a correctness bug.
- **Queue-full = drop, not error.** Neither accept-queue-full nor read-queue-full surfaces an error to anyone; only a Warn log (`demux.go:228`, `demux.go:236`). Senders get no signal (the underlying transport already "succeeded").
- **Session Read after Close**: `io.EOF` once `rQueue` is closed (`demux.go:297-299`).
- **Session Write after Close** (plain): `s.id` prepend + `bc.Write`; returns whatever the closed underlying conn returns (typically `net.ErrClosed`/`io.ErrClosedPipe`) (`demux.go:335-338`). No early `closing` check on the write path. Tagged Write returns `net.ErrClosed` via the `s.closed` select (`demux_tagged.go:253-254`).
- **Client Read**: distinguishes empty read (`0,nil`), truncated (`io.ErrUnexpectedEOF`), and ID mismatch (custom error) (`demux_client.go:50-60`).
- **Packet too large**: `demuxSess.Write` rejects `len(b)+len(s.id) > MaxPacketSize` with `"demux: packet too large"` (`demux.go:328-330`, tagged `demux_tagged.go:259-261`). **`MaxPacketSize = 65535`** (`packet.go:4`). **[HAZARD]** `demuxClient.Write` has **no such cap** (`demux_client.go:65-79`) — client can attempt to send a packet the server-side framing would reject or the transport can't carry.
- **Deadlines**: past read deadline → `os.ErrDeadlineExceeded` (`demux.go:285-286`, `demux.go:308-309`); plain Write enforces only a write deadline via `time.Now().After` (no blocking, `demux.go:324-326`); tagged Write enforces the write deadline against the tag-wait (`demux_tagged.go:232-257`).

## Dependencies

### Depends on
- `net.Conn` (plain) / `TaggedConn` (tagged) as the single underlying transport. Optional `interface{ MaxWrite() uint16 }` capability for size budgeting (`demux.go:147-152`, `demux_tagged.go:44-49`, `demux_client.go:29-34`).
- `MaxPacketSize` constant (`packet.go:4`); used as buffer size and as the write size cap.
- `Logger` interface, default `slog.Default()` (`logger.go:5-10`, `demux.go:141`).
- `Register`/`Wrapper`/`Dialer` plumbing for the URI driver (`driver.go:8-25`, `wrap.go:119-141`, `mux_client.go:28`).
- stdlib: `encoding/hex`, `sync`, `sync/atomic`, `time`, `context`, `io`, `os`, `errors`.

### Depended on by
- The URI/driver pipeline via the registered `"demux"` driver (`demux.go:24-89`). Other layers (encryption, transport mux, poll, dnst) can sit above/below it in the pipeline.
- The DNS-tunnel use case for the tagged variant (request/response correlation via tags).
- `NewDemuxListener` composes `NewDemux` per accepted conn (`demux_listener.go:28-30`).

### External
- None beyond the Go standard library.

## Change hazards (MOST IMPORTANT)

1. **ID length must match on both ends, exactly, per direction.** Server uses `idMask` (decoded `len(id)` from the URI) as the prefix length (`demux.go:67`, `demux.go:201`); client uses the literal `id` bytes and `len(id)` as its prefix length (`demux_client.go:55-62`, `demux.go:82`). If the configured `id` differs in **length** between client and server, the server slices the wrong number of prefix bytes and routes every packet to a wrong/garbage session; if it differs in **value**, the client's `Read` rejects responses with `"mismatched ID"` (`demux_client.go:58-60`). The driver derives both from the *same* `id` param (`demux.go:30-35`), so consistency is only guaranteed when both sides share identical URI `id`. Cross-layer contract: any change to how `id` is parsed (hex vs raw), or to `idMask` sizing, breaks framing silently.

2. **Queue sizing controls loss and memory, not throttling.** Both `accq` and `rq` use drop-on-full with non-blocking sends (`demux.go:224-237`). Increasing `rq` raises memory (each slot holds a full payload copy, up to `MaxPacketSize`) and reduces drops but never adds backpressure; decreasing it increases silent packet loss under burst. Tagged `tagQueue` is sized `sessReadQueueSize*2` (`demux_tagged.go:118`) — if you change `rq` you implicitly change tag buffering. There is **no head-of-line blocking across sessions** precisely because the loop never blocks; do not "fix" the drops by making the rQueue send blocking — that would let one slow session stall the read loop and starve **all** sessions (and, for net.Pipe-style transports, deadlock).

3. **Tagged: tag must be produced once per Read and consumed once per Write.** `Read` pushes a tag (`demux_tagged.go:200-201`); `Write` pops one (`demux_tagged.go:247-252`). A consumer that writes without reading **blocks**; one that reads twice then writes once leaves a stale tag that the *next* write will (incorrectly) reuse — responses could be correlated to the wrong request. `tagQueue` capacity `sessReadQueueSize*2` only bounds the imbalance before drops/blocking; it does not enforce 1:1. Any refactor of the Read/Write pairing, or adding extra internal reads, can desynchronize tags. Cross-layer: the dnst/poll-tagged layer below relies on the same tag surviving round-trip (see `poll-tagged.md`).

4. **Plain `demux` has a latent double-close of `rQueue`.** `demux.Close()` closes every session's `rQueue` (`demux.go:174-176`) while `demuxSess.Close()` *unconditionally* closes its own `rQueue` (the `sessions != nil` guard only protects the map delete, `demux.go:351-354`). A session `Close()` racing after listener `Close()` can `close` an already-closed channel → **panic**. The tagged variant already guards both behind `sessions != nil` (`demux_tagged.go:281-285`). Recommend aligning the plain path with the tagged path. Do not add session-Close logic without auditing this race.

5. **Write-path buffer aliasing inconsistency.** `demuxSess.Write`/`taggedDemuxSess.Write` use `append(s.id, b...)` (`demux.go:333`, `demux_tagged.go:264`), which can mutate `s.id` if it has spare capacity; `demuxClient.Write` deliberately allocates a fresh buffer to avoid this (`demux_client.go:65-70`). The session path is currently safe only because `s.id` is a tightly-sliced sub-array, but any change to how `s.id` is created (e.g. reusing a larger buffer with spare cap) would corrupt the routing prefix on concurrent/subsequent writes. Prefer the explicit-copy pattern from `demux_client.go` everywhere.

6. **No write-size cap on the client; cap only on sessions.** `demuxSess.Write` rejects `len(b)+len(id) > MaxPacketSize` (`demux.go:328-330`); `demuxClient.Write` does not (`demux_client.go:65-79`). Oversized client writes can produce packets the transport rejects or that exceed peer framing assumptions. Keep these symmetric if you tighten one.

7. **Sessions are created by any novel ID with no handshake.** First packet with an unseen prefix spawns a session (`demux.go:214-231`). Unauthenticated/noisy transports can create churn; the only bound is distinct IDs and the drop-on-full accept queue. Putting demux directly on an untrusted raw transport is a resource-exhaustion vector — usually it sits above an encryption/auth layer in the pipeline.

8. **EOF is whole-demux, not per-session.** Any underlying read error closes the entire listener and all sessions (`demux.go:183`, `demux.go:187-191`). A normal `io.EOF` is logged at Error level (`demux.go:189`). Code that expects to keep some sessions alive after a transient underlying error will be surprised — the model is "single physical conn ⇒ shared fate."

9. **`demuxClient` Close closes the shared underlying conn.** All clients from one `NewDemuxClient` `Dialer` share one `c` (`demux_client.go:17-19`); `Close` delegates to the embedded `net.Conn` (`demux_client.go:11`). Closing any "virtual" client kills the real transport for all. Adding multi-session client semantics would require decoupling Close from the shared conn.

## Tests covering this

`demux_test.go`:
- `TestDemux_Basic` (`:15-87`): round-trip with `idLen=4`, `accq=4`; asserts payload delivery and that the raw client-visible packet equals `id+response` (`:83-86`). Confirms ID prepend on server write.
- `TestDemux_MultipleSessions` (`:89-147`): two IDs, two sessions; asserts both payloads delivered, order-independent (`:138-144`).
- `TestDemux_Close` (`:149-179`): closes listener mid-session; **explicitly documents a gap** — "demux does not currently guarantee EOF/ErrClosed on session reads after Close" (`:177-178`). So post-Close session-read semantics are **untested/unspecified** for the plain variant. **[GAP]**
- `TestDemux_InvalidPacket` (`:181-215`): sends a 3-byte packet (< idMask 4); asserts the loop ignores it and stays alive for a subsequent valid session (`:192-214`). Covers `demux.go:196-200`.
- `TestDemux_DroppedPackets` (`:217-294`): `rq=2`, writes 4 packets; asserts P1/P2 readable and the 3rd read **blocks** (P3/P4 dropped) (`:282-293`). Directly validates drop-on-full (`demux.go:232-237`).
- `TestDemuxSess_Deadline` (`:296-344`): past deadline → immediate timeout; future deadline → blocks ~10ms then `os.IsTimeout` (`:322-343`). Covers `demux.go:283-309`.

`demux_tagged_test.go`:
- `TestTaggedDemux_Basic` (`:13-82`): asserts the **response tag equals the request tag** ("tag-1") — the core tag carry-through invariant (`:79-81`). Also asserts response packet = `id+response` (`:74-77`).
- `TestTaggedDemux_MultipleSessions` (`:84-183`): two tagged sessions echo concurrently; asserts both `id+payload` responses appear (`:177-182`). Tags are `nil` here.
- `TestTaggedDemux_Close` (`:185-236`): after listener Close, asserts session Read errors, session Write errors, and Accept errors (`:218-235`). The tagged variant **does** specify post-Close behavior (contrast the plain `TestDemux_Close` gap).

**Test gaps / not covered:**
- Plain `demux` post-Close session Read/Write semantics (acknowledged stale in `demux_test.go:177-178`).
- Accept-queue-full drop path (`demux.go:224-230`) — no test forces it; the `accq=4` in tests avoids it.
- The double-close-of-rQueue race in plain `demux` (hazard #4) — no test.
- Tag desynchronization (write-without-read blocking, double-read single-write) — not exercised; `TestTaggedDemux_*` always pairs reads/writes 1:1.
- `demuxClient` ID-mismatch and truncated-read error paths (`demux_client.go:55-60`) — no direct test.
- `MaxWrite` budgeting and the `MaxWrite <= idMask` error (`demux.go:147-152`) — no direct test.
- `NewDemuxListener` per-conn fan-in (`demux_listener.go`) — no test in these files.
- `NewDemuxDialer` (`demux_dialer.go`) — no test in these files.

## Related docs
- `pipeline.md` — how the `demux` driver/`Wrapper` slots into the URI layer pipeline.
- `mux.md` — the complementary multiplexer (server side counterpart) over a listener.
- `poll-tagged.md` — `TaggedConn` contract and the poll/dnst tag producers the tagged demux consumes.
- `stream-transforms.md` — framing/encryption layers that typically sit below demux and may provide `MaxWrite()`.
- `drivers-proto.md` — driver registration conventions and URI parameter parsing.
- `../mux-tag-poll.md` — existing overview doc touching mux/tag/poll.
