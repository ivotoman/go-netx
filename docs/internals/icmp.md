# ICMP Transport

> Internals doc. Goal: let future edits avoid unintended consequences. Every
> claim is grounded in code as `file:line`. Where the code is ambiguous or a
> claim could not be fully verified from source, it is explicitly flagged
> **(AMBIGUITY)**.

Module path: `github.com/pedramktb/go-netx` (`go.mod:1`).
Files in scope: `icmp_conn.go`, `icmp_listener.go`, `ip.go`, `packet.go`,
`dial.go`, plus the transport registration in `transport.go`.

---

## Purpose & role

`icmp` is a **base transport** (peer of `tcp`/`udp`) that tunnels an arbitrary
byte stream over ICMP **Echo Request / Echo Reply** packets, so traffic can
traverse firewalls that permit ICMP but block other protocols. The file header
states this intent (`icmp_conn.go:1-6`). It is selected by network string in
both `Dial` and `Listen` (`dial.go:79-101`, `dial.go:41-55`).

Two roles share one struct `icmpConn` (`icmp_conn.go:21-30`):

- **Client** (`reply == false`): sends Echo **Requests**, reads Echo
  **Replies** (constructed via `NewICMPClientConn`, `icmp_conn.go:46-56`).
- **Server** (`reply == true`): reads Echo **Requests**, sends Echo **Replies**
  (constructed via `NewICMPServerConn`, `icmp_conn.go:58-65`).

The listener side (`icmp_listener.go`) is a connection-oriented adapter over a
single raw ICMP `net.PacketConn`, **adapted from pion's UDP listener**
(`icmp_listener.go:1-4`). It demultiplexes many remote peers off one socket and
synthesizes a per-peer `net.Conn`, which `Accept` then wraps in an
`icmpConn` server conn (`icmp_listener.go:60-75`).

`TransportICMP = "icmp"` is registered as a known transport alongside tcp/udp
(`transport.go:9-13`, `transport.go:38-44`, `transport.go:50-58`). The comment
`// ip:1` (`transport.go:10`) refers to IP protocol number 1 (ICMP).

---

## Public API

### `NewICMPClientConn(conn net.Conn, version ipV) (net.Conn, error)`
`icmp_conn.go:46-56`

Wraps an already-dialed raw IP conn (a `*net.IPConn` from `DialIP`/`DialContext`
on `ip:icmp`) into a client `icmpConn`. Initializes:
- `reply=false`, `id=1`, `seq=1` (`icmp_conn.go:50-53`).
- `sentHashes` map sized 256 (`icmp_conn.go:54`).

**Contract / caveats:**
- Never returns a non-nil error in the current code (`icmp_conn.go:55`); the
  `error` return is forward-compatibility surface only.
- `id` is initialized to `1` and, for the client, **never changes** — `Write`
  reads `c.id` without ever updating it (`icmp_conn.go:136-137`). So every
  client request goes out with identifier `1`. **(See Change hazards: id is
  effectively a constant on the client.)**

### `NewICMPServerConn(conn net.Conn, version ipV) (net.Conn, error)`
`icmp_conn.go:58-65`

Wraps a per-peer conn (the `*icmpListenerConn` produced by the listener) into a
server `icmpConn`. Sets `reply=true`. Leaves `id`/`seq` at zero value and does
**not** allocate `sentHashes` (server never calls `rememberSent`). Called only
from `icmpListener.Accept` (`icmp_listener.go:65`).

### `icmpListenConfig` + `Listen(network string, laddr *net.IPAddr)`
`icmp_listener.go:122-145`, `icmp_listener.go:148-214`

Config struct mirrors pion's UDP `ListenConfig` (field docs copied verbatim,
`icmp_listener.go:123-145`):

| Field | Meaning | Default / validation |
|---|---|---|
| `Backlog` | max pending (unaccepted) conns; **silently discarded when full**, unlike TCP (`icmp_listener.go:124-130`) | `0` → `defaultListenBacklog = 128` (`icmp_listener.go:24`, `icmp_listener.go:149-151`) |
| `AcceptFilter func([]byte) bool` | decides whether a packet from a *new* peer spawns a conn; nil → accept all (`icmp_listener.go:132-134`, `icmp_listener.go:282-286`) | nil |
| `ReadBufferSize` | OS receive buffer; applied via `SetReadBuffer`, **error ignored** (`icmp_listener.go:136-138`, `icmp_listener.go:177-179`) | unset if `<= 0` |
| `WriteBufferSize` | OS send buffer; applied via `SetWriteBuffer`, **error ignored** (`icmp_listener.go:140-142`, `icmp_listener.go:180-182`) | unset if `<= 0` |
| `Batch pudp.BatchIOConfig` | enables pion batch read/write conn (`icmp_listener.go:144`, `icmp_listener.go:195-198`) | disabled |

**Contract / caveats:**
- `Listen` mutates the receiver: `lc.Backlog = defaultListenBacklog` when zero
  (`icmp_listener.go:149-151`). The config is built fresh each call in
  `dial.go:49-55`, so this is not observable to callers today, but it is a
  side-effect on the passed pointer.
- Batch validation: if `Batch.Enable` and either `WriteBatchSize <= 0` or
  `WriteBatchInterval <= 0`, returns `ErrInvalidBatchConfig`
  (`icmp_listener.go:153-155`, `icmp_listener.go:31`).
- The underlying socket is opened with **`net.ListenIP(network, laddr)`**
  (`icmp_listener.go:157-160`) — a raw IP socket (see Operational constraints).

### Options path from `dial.go`

**Listen** (`dial.go:29-59`):
1. `"icmp"` is normalized to `"ip:icmp"` then falls through
   (`dial.go:41-43`).
2. The accepted networks are `ip:icmp`, `ip4:icmp`, `ip6:ipv6-icmp`
   (`dial.go:44`).
3. Address resolved via `net.ResolveIPAddr` (`dial.go:45-48`).
4. An `icmpListenConfig` is built by copying fields **directly from the pion
   `pudp.ListenConfig`** supplied via `WithPacketListenConfig`
   (`dial.go:23-27`, `dial.go:49-55`). So all listener tunables (backlog,
   accept filter, buffer sizes, batch) originate from the same struct that
   configures the UDP listener — there is no ICMP-specific option type exposed
   to callers.

**Dial** (`dial.go:73-105`):
1. Same `"icmp"` → `"ip:icmp"` normalization + fallthrough
   (`dial.go:79-82`).
2. Uses the standard `net.Dialer` (`cfg.DialContext`) — `WithDialConfig`
   wraps a `net.Dialer` (`dial.go:67-71`, `dial.go:83`). There is **no**
   packet-config path for dial; the dialer just produces a raw `ip:icmp`
   conn, which is wrapped by `NewICMPClientConn` (`dial.go:101`).

---

## ICMP framing (echo request/reply mapping)

### Write — encode a payload chunk into one ICMP message
`icmp_conn.go:119-170`

1. **Type / code selection** (`icmp_conn.go:120-133`):
   - server (`reply`): `ipv6.ICMPTypeEchoReply` (v6) or `ipv4.ICMPTypeEchoReply`
     (v4).
   - client: `ipv6.ICMPTypeEchoRequest` (v6) or `ipv4.ICMPTypeEcho` (v4).
   - `Code` is always `0` (`icmp_conn.go:151`).
2. **Identifier / sequence** (`icmp_conn.go:135-148`):
   - Client: `id = c.id` (constant `1`); `seq` is `++c.seq` under the write lock,
     then `rememberSent(seq, b)` records a hash of the payload
     (`icmp_conn.go:136-142`).
   - Server: copies the **last-seen** `id`/`seq` it observed on inbound requests
     under `RLock` (`icmp_conn.go:143-148`). This is how a reply echoes the
     peer's identifier/sequence so the peer's OS/stack accepts it.
3. The whole user buffer `b` becomes `icmp.Echo.Data` (`icmp_conn.go:149-157`).
4. Marshalled via `msg.Marshal(nil)` (`icmp_conn.go:158`) and written to the
   underlying conn (`icmp_conn.go:162`). On short write returns
   `io.ErrShortWrite` (`icmp_conn.go:166-168`); on success returns `len(b)`
   (the user-payload length, **not** the marshalled length) (`icmp_conn.go:169`).

`msg.Marshal(nil)` produces only the ICMP message (type/code/checksum +
echo header + data); the OS prepends the IP header on send. **(AMBIGUITY:
checksum handling for the IPv6 case is delegated to `x/net/icmp`/the OS; the
`nil` pseudo-header argument means no IPv6 pseudo-header checksum is computed in
user space — verified only at the `Marshal(nil)` call site, `icmp_conn.go:158`.)**

### Read — extract a payload chunk from one ICMP message
`icmp_conn.go:67-117`

The read loop (`for {}`) reads raw bytes from the underlying conn
(`icmp_conn.go:68-72`), then parses depending on `ipV` and role:

| Path | Proto # | Slice parsed | Why |
|---|---|---|---|
| v6 server | 58 | `b[:n]` (`icmp_conn.go:74-76`) | server reads via listener buffer with **no IP header** |
| v6 client | 58 | `b[40:n]` (`icmp_conn.go:77-84`) | raw IPv6 socket read includes a **40-byte IPv6 header** |
| v4 server | 1 | `b[:n]` (`icmp_conn.go:86-87`) | listener-buffered, no IP header |
| v4 client | 1 | `b[20:n]` (`icmp_conn.go:88-94`) | raw IPv4 socket read includes a **20-byte IPv4 header** |

Protocol numbers: `1` = ICMPv4, `58` = ICMPv6 (`icmp_conn.go:76,83,87,94`).

**Header-strip guards (client only):**
- v6: `len(b) < 40` → `io.ErrShortBuffer`; `n < 40` → `io.ErrUnexpectedEOF`
  (`icmp_conn.go:78-82`).
- v4: `len(b) < 20` → `io.ErrShortBuffer`; `n < 20` → `io.ErrUnexpectedEOF`
  (`icmp_conn.go:89-93`).
- The fixed 20/40 strip assumes **no IPv4 options** and the standard fixed IPv6
  header. **(AMBIGUITY / hazard: IPv4 packets with options have headers > 20
  bytes; this code would then misparse. See Change hazards.)**

**Body handling** (`icmp_conn.go:100-115`):
- Only `*icmp.Echo` bodies are accepted; anything else → `io.ErrUnexpectedEOF`
  (`icmp_conn.go:113-114`). So Echo Reply error bodies, time-exceeded, etc.
  surface as `io.ErrUnexpectedEOF`.
- Server: records the inbound `pkt.ID`/`pkt.Seq` into `c.id`/`c.seq` under the
  write lock (`icmp_conn.go:102-107`) so its next `Write` reply can echo them.
- Payload copied out: `n = copy(b, pkt.Data)` (`icmp_conn.go:108`). The copy is
  into `b`, overwriting the raw bytes just read. The number of bytes returned is
  bounded by `len(b)`; if `pkt.Data` is larger than `b`, **the tail is silently
  dropped** (no `io.ErrShortBuffer` for the echo payload). **(Change hazard.)**
- **Client self-echo suppression** (`icmp_conn.go:109-111`): if
  `!reply && consumeSent(pkt.Seq, b[:n])` matches a hash this client recorded,
  the packet is skipped (`continue`). This discards the **OS auto-generated
  Echo Reply** that the kernel produces for the client's own Echo Request when
  client and server run on the same host / loopback (intent comment at
  `icmp_conn.go:26`).

### Self-echo suppression internals
`icmp_conn.go:32-44`

- `rememberSent(seq, data)`: `sentHashes[uint8(seq%256)] = sha256(data)`
  (`icmp_conn.go:32-37`).
- `consumeSent(seq, data)`: returns `sentHashes[uint8(seq%256)] == sha256(data)`
  (`icmp_conn.go:39-44`).
- The key is `seq % 256` (a `uint8`), so only the **low 8 bits of seq** index a
  256-slot table; sequence wrap collisions are possible (see Change hazards).
- `consumeSent` does **not** delete the entry (despite the name "consume"); a
  matching hash stays and could suppress a later legitimately-identical payload
  with the same low-8-bit seq. **(Change hazard.)**

---

## Internal design & invariants

### `ipV` version type
`ip.go` (full file):

```go
package netx

type ipV uint8

const (
	IPv4 ipV = 4
	IPv6 ipV = 6
)
```

A 1-byte enum holding the literal IP version number. Used to branch framing in
`icmpConn` (`icmp_conn.go:74-95`, `icmp_conn.go:122-131`) and to tag the
listener (`icmp_listener.go:36`). Note the listener stores `version` as a raw
literal `4`/`6` in its switch (`icmp_listener.go:164-174`) rather than the named
`IPv4`/`IPv6` constants — same values, different spelling.

### `packet.go`
Full file:

```go
package netx

// MaxPacketSize is the maximum allowed packet size for framed or packet-based protocols.
const MaxPacketSize = 65535
```

`MaxPacketSize = 65535` (`packet.go:1-4`). It is **not referenced** by any ICMP
code in scope (no use in `icmp_conn.go` or `icmp_listener.go`). The ICMP read
path is instead bounded by `receiveMTU = 8192` (`icmp_listener.go:23`). **(See
Change hazards: there is no enforcement that a single Write payload fits ICMP
MTU; `MaxPacketSize` is informational here.)**

### ipV detection logic (v4 vs v6)
Identical logic appears in both `dial.go:87-100` (dial) and
`icmp_listener.go:162-175` (listen):

1. If network is explicitly `ip4:icmp` → version 4.
2. If network is explicitly `ip6:ipv6-icmp` → version 6.
3. Otherwise (the `ip:icmp` default, after the `"icmp"` alias is normalized to
   `ip:icmp`): inspect `conn.LocalAddr().(*net.IPAddr).IP.To4()`. Non-nil →
   v4; nil → v6 (`dial.go:93-99`, `icmp_listener.go:168-174`).

**Invariant / hazard:** the generic `ip:icmp` path relies entirely on
`To4()` of the **local** address. For a v6 destination resolved under the
unqualified `ip:icmp`, the local address determines version; mismatches between
local-address family and the actual peer family will pick the wrong framing.
The listener additionally ignores the `_` error from the `*net.IPAddr` type
assertion (`icmp_listener.go:169`) — a non-`*net.IPAddr` `LocalAddr` would nil-
panic on `iaddr.IP`. **(Change hazard / AMBIGUITY.)**

### Listener accept loop & per-peer demux
`icmp_listener.go:60-75`, `icmp_listener.go:220-297`

- One `readLoop` goroutine owns the socket (`icmp_listener.go:204`,
  `icmp_listener.go:220-229`). It chooses `readBatch` if the conn implements
  `pudp.BatchReader` **and** `readBatchSize > 1`, else single `read`
  (`icmp_listener.go:224-228`).
- `read` loops `pConn.ReadFrom` into a `receiveMTU`-sized buffer and dispatches
  (`icmp_listener.go:251-262`). `readBatch` pre-allocates `readBatchSize`
  messages each with a `receiveMTU` buffer + 40-byte OOB, then dispatches each
  (`icmp_listener.go:231-249`).
- `dispatchMsg` → `getConn(addr, buf)`; if a conn exists/was created, the raw
  bytes are appended to that conn's packet buffer:
  `conn.buffer.Write(buf)` (`icmp_listener.go:264-272`).
- **Per-peer keying:** conns are keyed by `raddr.String()` in
  `l.conns map[string]*icmpListenerConn` (`icmp_listener.go:49`,
  `icmp_listener.go:277`). So peers are distinguished **purely by source IP
  address string** — *not* by ICMP identifier. (See Change hazards.)
- **New-peer path** (`icmp_listener.go:278-294`): under `connLock`, if no conn
  for the addr: check `accepting` (closed → `ErrClosedListener`), run
  `acceptFilter(buf)` if set (false → drop, no conn), build a conn via
  `newConn`, then non-blocking `acceptCh <- conn`. On success register in
  `l.conns`; if the channel is full (backlog exceeded) return
  `ErrListenQueueExceeded` and the conn is **not** registered (silently
  dropped, matching the documented backlog behavior).

**Critical demux note:** the bytes written into the per-peer buffer at
`icmp_listener.go:270` are the **raw socket bytes** (an entire ICMP message
without IP header, since `ListenIP` on a protocol-specific raw socket delivers
ICMP payload). The server-side `icmpConn.Read` therefore parses with `b[:n]`
(no header strip) for the server role (`icmp_conn.go:76,87`). This is the
matching half of the client's header-stripping reads. **(AMBIGUITY: whether
`net.ListenIP("ip4:icmp", ...)` strips the IP header is OS/stdlib behavior; the
code's `b[:n]` server parse asserts it does. On Linux raw `ip:` sockets, the
IPv4 header *is* typically included on read — this is a latent inconsistency,
see Change hazards.)**

### Per-peer conn (`icmpListenerConn`)
`icmp_listener.go:299-321`

A thin packet-buffer-backed `net.Conn`:
- `Read` → `buffer.Read(p)` (pion `packetio.Buffer`, packet-framed)
  (`icmp_listener.go:324-326`).
- `Write` → checks write deadline, then `listener.pConn.WriteTo(p, rAddr)`
  (`icmp_listener.go:329-337`). All peers **share the single listener socket**
  for writes; `rAddr` selects the destination.
- `buffer = packetio.NewBuffer()` preserves message boundaries
  (`icmp_listener.go:317`); each `buffer.Write` of one ICMP message is one
  `buffer.Read`-able packet.

---

## Concurrency model

### `icmpConn` (per-conn)
- A single `*sync.RWMutex` guards `sentHashes`, `id`, `seq`
  (`icmp_conn.go:25`). Writers take `Lock`; the server's reply path takes
  `RLock` to read `id`/`seq` (`icmp_conn.go:144-147`).
- `Read` and `Write` may run concurrently; the mutex makes the id/seq/hash
  fields safe, but the underlying `net.Conn` Read/Write are independent (stdlib
  conns allow concurrent R/W).
- **Note:** `consumeSent` is named "consume" but takes the **write** `Lock`
  (`icmp_conn.go:40-42`) even though it only reads — so concurrent Reads
  serialize on it.

### Listener (shared socket, many peers)
- `connLock sync.Mutex` guards the `conns` map (`icmp_listener.go:48-49`),
  taken in `getConn` (`icmp_listener.go:275-276`), `Close`
  (`icmp_listener.go:85-99`), and `icmpListenerConn.Close`
  (`icmp_listener.go:345-348`).
- `accepting atomic.Value` (bool) gates new-conn creation and is read
  lock-free (`icmp_listener.go:42`, `icmp_listener.go:200`,
  `icmp_listener.go:279`, `icmp_listener.go:350`).
- `acceptCh chan *icmpListenerConn` is the bounded accept queue (capacity =
  `Backlog`) (`icmp_listener.go:43`, `icmp_listener.go:187`); producers are
  `getConn` (non-blocking send), consumer is `Accept`.
- `doneCh` / `doneOnce` close the listener once (`icmp_listener.go:44-45`,
  `icmp_listener.go:81-83`).
- WaitGroups:
  - `connWG` counts the listener "self" reference (+1 at
    `icmp_listener.go:201`) plus one per **accepted** conn (`connWG.Add(1)` in
    `Accept`, `icmp_listener.go:63`); decremented in `Close`
    (`icmp_listener.go:101`) and `icmpListenerConn.Close`
    (`icmp_listener.go:343`). When it hits zero, a goroutine closes the socket
    (`icmp_listener.go:205-211`).
  - `readWG.Add(2)` waits for `readLoop` and the socket-close goroutine
    (`icmp_listener.go:202`, `icmp_listener.go:221`, `icmp_listener.go:210`).
- `errRead` / `errClose` (`atomic.Value`) carry the terminal read error and
  close error (`icmp_listener.go:53,56`).

**Hazard:** `connWG.Add(1)` for an accepted conn happens in `Accept`
(`icmp_listener.go:63`) but the matching `Done` is in the conn's `Close`
(`icmp_listener.go:343`). If `Accept` returns a conn that is never `Close`d,
`connWG` never drains and the socket-closing goroutine never runs. Conversely,
an unaccepted conn sitting in `acceptCh` at listener `Close` is torn down
directly (`icmp_listener.go:88-97`) without a `connWG` accounting (it was never
`Add`ed).

---

## Lifecycle & ownership

### `icmpConn`
Has **no** `Close` override — it embeds `net.Conn` (`icmp_conn.go:22`), so
`Close`, `LocalAddr`, `RemoteAddr`, and deadlines delegate to the wrapped conn.
For a client that is the raw `*net.IPConn`; for a server that is the
`*icmpListenerConn`.

### Listener `Close`
`icmp_listener.go:79-115`

- Idempotent via `doneOnce` (`icmp_listener.go:81`).
- Sets `accepting=false`, closes `doneCh` (`icmp_listener.go:82-83`).
- Drains `acceptCh` of unaccepted conns, closing each `doneCh` and removing from
  `conns` (`icmp_listener.go:87-97`).
- `connWG.Done()` releases the listener self-reference
  (`icmp_listener.go:101`).
- If no remaining conns, waits `readWG` and surfaces `errClose`
  (`icmp_listener.go:103-111`). If conns remain, returns nil and the socket is
  closed later when the **last** conn closes.

### Per-peer conn `Close`
`icmp_listener.go:340-366`

- Idempotent via `doneOnce` (`icmp_listener.go:342`).
- `connWG.Done()`, close `doneCh`, delete from `conns` under `connLock`
  (`icmp_listener.go:343-348`).
- If this is the last conn **and** the listener is already closed, waits
  `readWG` and surfaces `errClose` (`icmp_listener.go:350-358`).
- Closes the packet buffer; preserves `errClose` precedence over buffer-close
  error (`icmp_listener.go:360-362`).

**Ownership:** the single OS socket (`pConn`) is owned by the listener and is
closed exactly once, by the goroutine spawned at `Listen`
(`icmp_listener.go:205-211`) when `connWG` reaches zero. Individual peer conns
never close the socket; they only `WriteTo` it.

---

## Error, EOF & deadline semantics

### `icmpConn`
- Underlying read error → returned as-is with `n=0` (`icmp_conn.go:70-72`).
- IP-header-strip failures: `io.ErrShortBuffer` (caller buffer too small) /
  `io.ErrUnexpectedEOF` (truncated packet) (`icmp_conn.go:78-82`,
  `icmp_conn.go:89-93`).
- Parse error from `icmp.ParseMessage` → returned as-is (`icmp_conn.go:97-98`).
- Non-Echo body → `io.ErrUnexpectedEOF` (`icmp_conn.go:113-114`).
- There is **no synthetic `io.EOF`** in `icmpConn` — stream end is whatever the
  underlying conn returns. A peer that simply stops sending ICMP yields a
  blocking Read (or a deadline error if set).
- Deadlines delegate to the embedded `net.Conn` (no override).

### Listener / per-peer conn
- `Accept` returns `ErrClosedListener` on `doneCh`, or the stored read error on
  `readDoneCh` (`icmp_listener.go:60-74`, `icmp_listener.go:29`).
- Per-peer `Read` returns whatever `packetio.Buffer.Read` returns: `io.EOF`
  when the buffer is closed and empty, `io.ErrShortBuffer` if the supplied
  slice is smaller than the queued packet, and a timeout `net.Error` on read
  deadline (pion `packetio/buffer.go` Read semantics, referenced from
  `icmp_listener.go:325`).
- Per-peer `Write` returns `context.DeadlineExceeded` if the **write deadline**
  has already elapsed (`icmp_listener.go:330-333`).
- `SetDeadline` sets both write deadline and read deadline
  (`icmp_listener.go:379-383`); `SetReadDeadline` → `buffer.SetReadDeadline`
  (`icmp_listener.go:386-388`); `SetWriteDeadline` sets only the local
  `writeDeadline` and intentionally does **not** touch the shared socket's
  write deadline because the socket is shared across peers
  (`icmp_listener.go:391-396`). **Consequence:** the write deadline is only
  enforced as a pre-check; an in-progress `WriteTo` blocking on the OS socket is
  not interrupted by a peer's write deadline.

---

## Operational constraints

- **Raw-socket privilege.** `net.ListenIP("ip4:icmp"/...)`
  (`icmp_listener.go:157`) and `Dial` on `ip:icmp` (`dial.go:83`) open **raw IP
  sockets**. On Linux this requires `CAP_NET_RAW` (typically root, or the binary
  granted `cap_net_raw+ep`); without it `ListenIP`/`DialContext` fail and that
  error is returned verbatim (`icmp_listener.go:158-160`, `dial.go:84-85`).
  There is **no fallback** to unprivileged datagram ICMP
  (`ip4:icmp` is used, not the Linux `udp` ping mode), and **no friendly error
  wrapping** — privilege failures surface as raw stdlib errors.
- **MTU / payload size.** Receive side is capped at `receiveMTU = 8192`
  (`icmp_listener.go:23`, `icmp_listener.go:235`, `icmp_listener.go:252`); a
  single ICMP message larger than 8192 bytes is truncated at read. On the send
  side, `icmpConn.Write` places the **entire** user buffer in one Echo `Data`
  field (`icmp_conn.go:155`) with no chunking and no check against MTU or
  against `MaxPacketSize`. Payloads beyond the path MTU will be IP-fragmented by
  the OS or rejected. **There is no application-level segmentation here.**
- **OS auto-reply interaction.** On the client, the kernel may itself answer the
  client's Echo Request with an Echo Reply (especially loopback / same-host);
  the hash-based `consumeSent` filter exists to discard those
  (`icmp_conn.go:26`, `icmp_conn.go:109-111`).
- **Batch I/O is Linux-only.** pion's `NewBatchConn` only wires the batch path
  on `runtime.GOOS == "linux"` (pion `udp/batchconn.go`
  `NewBatchConn`); on other OSes batch config still constructs but falls back to
  per-packet read/write. The listener enables batch read only when
  `readBatchSize > 1` (`icmp_listener.go:224`).
- **OOB buffer sizing.** `readBatch` allocates 40-byte OOB per message
  (`icmp_listener.go:236`) — sized for an IPv6 header's worth of control data;
  this is copied from the UDP origin and not re-tuned for ICMP.

---

## Dependencies

**Depends on (internal):**
- `ipV` type (`ip.go`) — used by `icmpConn` and `icmpListener`.
- Selected/registered by `dial.go` (`Dial`/`Listen` branches) and
  `transport.go` (`TransportICMP`). The `cli` references icmp in help text only
  (`cli/internal/help.go`); no functional coupling found there.

**Depended on by:**
- Any higher layer that uses `netx.Dial`/`netx.Listen` with network `"icmp"`.
  As a base transport, it sits **below** the tunnel/mux stack
  (Mux, TaggedDemux, DemuxClient, PollConn, DNST) documented in the related
  docs; those layers consume the `net.Conn`/`net.Listener` it returns. The ICMP
  framing must stay compatible with what those upper layers expect (a reliable,
  ordered-ish byte pipe is **not** guaranteed by ICMP — see hazards).

**External:**
- `golang.org/x/net/icmp` — message parse/marshal, `icmp.Echo`
  (`icmp_conn.go:16`, `icmp_conn.go:73-95`, `icmp_conn.go:149-158`).
- `golang.org/x/net/ipv4`, `.../ipv6` — ICMP message-type constants and
  `ipv4.Message` for batch (`icmp_conn.go:17-18`, `icmp_listener.go:19`,
  `icmp_listener.go:232`).
- `github.com/pion/transport/v3/udp` (`pudp`) — `BatchIOConfig`,
  `NewBatchConn`, `BatchReader`, and the `ListenConfig` shape the icmp config is
  copied from (`icmp_listener.go:18`, `dial.go:7`).
- `github.com/pion/transport/v3/packetio` — per-peer packet buffer
  (`icmp_listener.go:17`, `icmp_listener.go:317`).
- `github.com/pion/transport/v3/deadline` — per-peer write deadline
  (`icmp_listener.go:16`, `icmp_listener.go:319`).
- `crypto/sha256` — self-echo suppression hashing (`icmp_conn.go:11`).
- Versions: `go.mod` pins `pion/transport/v3 v3.1.1` and `golang.org/x/net
  v0.52.0` (`go.mod:5-6`). **(AMBIGUITY: the pion source quoted for field shapes
  in this doc was read from a *vendored copy in a sibling project*, not the
  pinned `v3.1.1` module cache, which was not present in this environment. The
  field set matched, but minor behavioral differences across patch versions
  cannot be ruled out.)**

---

## Change hazards (MOST IMPORTANT)

1. **Identifier is a constant on the client; peers are demuxed by IP only.**
   - Client `id` is fixed at `1` and never updated (`icmp_conn.go:52`,
     `icmp_conn.go:136-137`).
   - The listener keys conns solely by `raddr.String()` (source IP), **never**
     by ICMP identifier (`icmp_listener.go:277`, `icmp_listener.go:290`).
   - Consequence: **two distinct clients behind the same source IP (NAT) cannot
     be told apart** — they collapse into one peer conn, interleaving their
     streams. Any future attempt to support multiplexed clients per IP must add
     identifier-based keying (and the client must vary `id`). Changing the demux
     key is a wire-compat change.

2. **Sequence-hash self-echo filter is only 8 bits wide and never evicts.**
   - `sentHashes` is indexed by `seq % 256` (`icmp_conn.go:35`,
     `icmp_conn.go:43`) and `consumeSent` does not delete matched entries
     (`icmp_conn.go:39-44`).
   - Hazards: (a) after 256 writes the table indices wrap and overwrite
     (acceptable), but (b) a legitimate **server reply** whose payload happens to
     equal a previously-sent client payload with the same low-8-bit seq will be
     **silently dropped** as a self-echo (`icmp_conn.go:109-111`). High-rate or
     repetitive payloads make this collision realistic. Widening the key or
     making `consume` actually delete are wire-neutral but behavior-changing.

3. **Fixed 20/40-byte IP-header strip assumes no IPv4 options and a bare IPv6
   header.**
   - Client read strips exactly 20 (v4) / 40 (v6) bytes
     (`icmp_conn.go:78-94`). IPv4 packets carrying options have headers longer
     than 20 bytes, which would shift the parse and corrupt framing. Any change
     to socket type (e.g. switching to a header-included vs header-excluded raw
     socket) must revisit these constants.
   - **Server/client asymmetry:** the server parses with **no** strip
     (`b[:n]`, `icmp_conn.go:76,87`) while the client strips a header. This
     hard-codes the assumption that `net.ListenIP` delivers ICMP **without** the
     IP header but `DialIP` reads **with** it. That assumption is OS-dependent;
     on platforms where `ListenIP` includes the IP header the server framing
     breaks. (AMBIGUITY flagged above.)

4. **No payload segmentation vs ICMP/IP MTU; receive cap at 8192.**
   - `Write` emits the whole buffer as one Echo `Data` (`icmp_conn.go:155`); no
     check against `MaxPacketSize` (`packet.go:4`) or path MTU. `Read` /
     listener buffers cap at `receiveMTU = 8192` (`icmp_listener.go:23`), and
     `Read`'s `copy(b, pkt.Data)` silently truncates if the caller's slice is
     smaller than the echo payload (`icmp_conn.go:108`). Large writes will be
     IP-fragmented (fragile across firewalls) or truncated on read. Introducing
     chunking/reassembly is a wire-compat change that upper tunnel layers must
     tolerate.

5. **v4/v6 detection on the generic `ip:icmp` path depends on local-address
   `To4()` and ignores the type-assert error.**
   - `dial.go:93-99` and `icmp_listener.go:168-174` choose version from
     `LocalAddr().(*net.IPAddr).IP.To4()`. The listener drops the assertion
     error (`icmp_listener.go:169`) and would nil-panic on a non-`*net.IPAddr`
     `LocalAddr`. A v4-mapped-v6 local address or a mismatch between local
     family and peer family selects the wrong ICMP type/proto number, producing
     packets the peer rejects. Prefer the explicit `ip4:icmp` /
     `ip6:ipv6-icmp` networks.

6. **Privilege failures are unwrapped; behavior is fail-hard.**
   - Both `ListenIP` (`icmp_listener.go:157-160`) and `DialContext`
     (`dial.go:83-85`) surface raw OS errors with no `CAP_NET_RAW`/root hint.
     Any change to error handling should preserve (or improve) the diagnosability
     here; callers/tests likely assert on the stdlib error and will not match a
     wrapped message.

7. **Changing the framing breaks interop with the DNST-style tunnel stack.**
   - The Echo type/code/id/seq/Data layout (`icmp_conn.go:119-170`) and the
     "server echoes last-seen id/seq" reply contract (`icmp_conn.go:102-107`,
     `icmp_conn.go:143-148`) are the wire format. The upper layers
     (Mux/TaggedDemux/DemuxClient/PollConn/DNST, see related docs) assume a
     byte-pipe; any change to id/seq semantics, payload placement, or
     self-echo filtering can desync client/server or corrupt the multiplexed
     streams above. Treat the framing as a stable wire contract.

8. **WaitGroup bookkeeping coupling (`connWG`).** An accepted but never-`Close`d
   server conn pins `connWG` above zero and prevents the socket-closing
   goroutine from ever running (`icmp_listener.go:63`,
   `icmp_listener.go:205-211`, `icmp_listener.go:343`). Refactors of accept/close
   must keep the `Add`/`Done` pairing exact, or the listener will leak the
   socket on shutdown.

---

## Related docs

The internals set this is intended to join. **(NOTE: at time of writing only
`docs/mux-tag-poll.md` exists; the file names below are the intended targets per
the documentation plan and may not all exist yet.)**

- `pipeline.md` — overall transport→tunnel composition pipeline.
- `mux.md` — Mux / MuxClient (currently covered in `docs/mux-tag-poll.md`).
- `poll-tagged.md` — PollConn / TaggedConn / TaggedDemux (currently in
  `docs/mux-tag-poll.md`).
- `drivers-proto.md` — `drivers/` and `proto/` (e.g. DNST) layers that ride on
  top of base transports like this one.
