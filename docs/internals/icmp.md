# ICMP Transport

> Internals / change-safety doc. Cites code by **symbol + file**, not line numbers
> (line numbers rot on every refactor). Bare line numbers appear only for things
> with no stable symbol (a specific error literal, a numeric default in a struct
> literal), and even then the enclosing function/struct is named.

Module: `github.com/pedramktb/go-netx` (root).
Files in scope: `icmp_conn.go`, `icmp_listener.go`, `ip.go`, `packet.go`,
`dial.go` (the `Dial`/`Listen` branches), `transport.go` (`TransportICMP`).

---

## Purpose & role

`icmp` is a **base transport** (peer of `tcp`/`udp`) that tunnels an arbitrary
byte stream over ICMP **Echo Request / Echo Reply** packets, so traffic can
traverse firewalls that permit ICMP but block other protocols (file header of
`icmp_conn.go`). It is selected by network string in both `Dial` and `Listen`
(`dial.go`), and registered as `TransportICMP = "icmp"` alongside tcp/udp in
`transport.go` (the `// ip:1` comment there is IP protocol number 1 = ICMP).

Two roles share one struct `icmpConn` (`icmp_conn.go`):

- **Client** (`reply == false`): sends Echo **Requests**, reads Echo
  **Replies** (constructed via `NewICMPClientConn`).
- **Server** (`reply == true`): reads Echo **Requests**, sends Echo **Replies**
  (constructed via `NewICMPServerConn`).

The listener side (`icmp_listener.go`) is a connection-oriented adapter over a
single raw ICMP `net.PacketConn`, **adapted from pion's UDP listener** (license
header at top of file). It demultiplexes many remote peers off one socket and
synthesizes a per-peer `net.Conn`, which `icmpListener.Accept` then wraps in an
`icmpConn` server conn.

### How a caller selects it

- Through the URI pipeline the transport token is `icmp` (CLI help advertises
  `icmp: ICMP listener or dialer`, `cli/internal/help.go`), e.g. a scheme of
  `icmp://<addr>` or `icmp+<wrappers>://<addr>`.
- Through the Go API: `netx.Dial(ctx, "icmp", addr)` /
  `netx.Listen(ctx, "icmp", addr)`. `"icmp"` is normalized to `"ip:icmp"`; the
  family-explicit networks `ip4:icmp` and `ip6:ipv6-icmp` are also accepted
  (the `case` lists in `Dial`/`Listen`, `dial.go`).
- There is **no driver-registry entry** — icmp is wired only via `transport.go`
  + the `Dial`/`Listen` switch, so it is part of the core root module's
  transport set.

---

## Public API

### `NewICMPClientConn(conn net.Conn, version ipV) (net.Conn, error)`

Wraps an already-dialed raw IP conn (a `*net.IPConn` from the dialer on
`ip:icmp`) into a client `icmpConn`. Initializes `reply=false`, `id=1`, `seq=1`,
and a `sentHashes` map sized 256.

- Never returns a non-nil error in the current code; the `error` return is
  forward-compatibility surface only.
- `id` is initialized to `1` and **never changes** — `Write` reads `c.id`
  without updating it. So every client request goes out with identifier `1`
  (see Change hazard 1).

### `NewICMPServerConn(conn net.Conn, version ipV) (net.Conn, error)`

Wraps a per-peer conn (the `*icmpListenerConn` produced by the listener) into a
server `icmpConn` with `reply=true`. Leaves `id`/`seq` at zero value and does
**not** allocate `sentHashes` (the server never calls `rememberSent`). Called
only from `icmpListener.Accept`.

### `icmpListenConfig` + its `Listen(network string, laddr *net.IPAddr)`

The config struct mirrors pion's UDP `ListenConfig` (field doc-comments copied
verbatim from it):

| Field | Meaning | Default / validation |
|---|---|---|
| `Backlog` | max pending (unaccepted) conns; **silently discarded when full**, unlike TCP | `0` → `defaultListenBacklog = 128` (const in `icmp_listener.go`, applied at top of `icmpListenConfig.Listen`) |
| `AcceptFilter func([]byte) bool` | decides whether a packet from a *new* peer spawns a conn; nil → accept all (consulted in `getConn`) | nil |
| `ReadBufferSize` | OS receive buffer; applied via `SetReadBuffer`, **error ignored** | unset if `<= 0` |
| `WriteBufferSize` | OS send buffer; applied via `SetWriteBuffer`, **error ignored** | unset if `<= 0` |
| `Batch pudp.BatchIOConfig` | enables pion batch read/write conn | disabled |

- `icmpListenConfig.Listen` **mutates its receiver**: it sets
  `lc.Backlog = defaultListenBacklog` when zero. The config is built fresh on
  each `netx.Listen` call (in `dial.go`), so this is not observable to callers
  today, but it is a side-effect on the passed pointer.
- Batch validation: if `Batch.Enable` and either `WriteBatchSize <= 0` or
  `WriteBatchInterval <= 0`, returns `ErrInvalidBatchConfig`.
- The underlying socket is opened with **`net.ListenIP(network, laddr)`** — a
  raw IP socket (see Operational constraints).

### Path from `dial.go`

- **Listen** accepts `icmp` / `ip:icmp` / `ip4:icmp` / `ip6:ipv6-icmp`
  (`icmp` → `ip:icmp` via fallthrough), resolves with `net.ResolveIPAddr`, then
  builds an `icmpListenConfig` by copying fields **directly from the pion
  `pudp.ListenConfig`** supplied via `WithPacketListenConfig`. So all listener
  tunables (backlog, accept filter, buffer sizes, batch) originate from the same
  struct that configures the UDP listener — there is no ICMP-specific option
  type exposed to callers.
- **Dial** uses the standard `net.Dialer` (`WithDialConfig`) to produce a raw
  `ip:icmp` conn, which is wrapped by `NewICMPClientConn`. There is **no**
  packet-config path for dial.
- v4/v6 detection is shared between both (see "v4 vs v6 detection" below).

---

## ICMP framing (echo request/reply mapping)

### Write — encode a payload chunk into one ICMP message (`icmpConn.Write`)

1. **Type / code selection:**
   - server (`reply`): `ipv6.ICMPTypeEchoReply` (v6) or `ipv4.ICMPTypeEchoReply` (v4).
   - client: `ipv6.ICMPTypeEchoRequest` (v6) or `ipv4.ICMPTypeEcho` (v4).
   - `Code` is always `0`.
2. **Identifier / sequence:**
   - Client: `id = c.id` (constant `1`); `seq` is `++c.seq` under the write
     lock, then `rememberSent(seq, b)` records a hash of the payload.
   - Server: copies the **last-seen** `id`/`seq` it observed on inbound requests
     under `RLock`. This is how a reply echoes the peer's identifier/sequence so
     the peer's OS/stack accepts it.
3. The whole user buffer `b` becomes `icmp.Echo.Data` — **no chunking**.
4. Marshalled via `msg.Marshal(nil)` (only the ICMP message; the OS prepends the
   IP header on send) and written to the underlying conn. On short write returns
   `io.ErrShortWrite`; on success returns `len(b)` (the user-payload length,
   **not** the marshalled length).

### Read — extract a payload chunk from one ICMP message (`icmpConn.Read`)

The read loop reads raw bytes from the underlying conn, then parses depending on
`ipV` and role:

| Path | Proto # | Slice parsed | Why |
|---|---|---|---|
| v6 server | 58 | `b[:n]` | server reads via the listener buffer with **no IP header** |
| v6 client | 58 | `b[40:n]` | raw IPv6 socket read includes a **40-byte IPv6 header** |
| v4 server | 1 | `b[:n]` | listener-buffered, no IP header |
| v4 client | 1 | `b[20:n]` | raw IPv4 socket read includes a **20-byte IPv4 header** |

(`1` = ICMPv4, `58` = ICMPv6.)

**Header-strip guards (client only):** before stripping, v6 checks `len(b) < 40`
→ `io.ErrShortBuffer`, `n < 40` → `io.ErrUnexpectedEOF`; v4 uses the same checks
against 20. The fixed 20/40 strip assumes **no IPv4 options** and the standard
fixed IPv6 header (see Change hazard 3).

**Body handling:**
- Only `*icmp.Echo` bodies are accepted; any other body → `io.ErrUnexpectedEOF`
  (so time-exceeded / error replies surface as `io.ErrUnexpectedEOF`).
- Server: records inbound `pkt.ID`/`pkt.Seq` into `c.id`/`c.seq` under the write
  lock so its next `Write` reply can echo them.
- Payload copied out with `n = copy(b, pkt.Data)`, overwriting the raw bytes
  just read. If `pkt.Data` is larger than `b`, **the tail is silently dropped**
  (no `io.ErrShortBuffer` for the echo payload — see Change hazard 4).
- **Client self-echo suppression:** if `!reply && consumeSent(pkt.Seq, b[:n])`
  matches a hash this client recorded, the packet is skipped (`continue`). This
  discards the **OS auto-generated Echo Reply** the kernel produces for the
  client's own Echo Request when client and server share a host / loopback
  (intent comment on the `sentHashes` field).

### Self-echo suppression internals

- `rememberSent(seq, data)`: `sentHashes[uint8(seq%256)] = sha256(data)`.
- `consumeSent(seq, data)`: returns `sentHashes[uint8(seq%256)] == sha256(data)`.
- The key is `seq % 256` (a `uint8`), so only the **low 8 bits of seq** index a
  256-slot table (wrap collisions possible).
- `consumeSent` does **not** delete the entry (despite the name "consume") and
  takes the write `Lock` even though it only reads (see Change hazard 2 and
  Concurrency model).

---

## Internal design & invariants

- **`ip.go`:** `type ipV uint8` with `IPv4 = 4`, `IPv6 = 6` — a 1-byte enum
  holding the literal IP version number. Branches the framing in `icmpConn` and
  tags the listener. Note `icmpListenConfig.Listen` stores `version` via the raw
  literals `4`/`6` in its switch rather than the named `IPv4`/`IPv6` constants
  (same values, different spelling).
- **`packet.go`:** `MaxPacketSize = 65535`. It is **not referenced** by any ICMP
  code in scope; the ICMP read path is instead bounded by `receiveMTU = 8192`
  (const in `icmp_listener.go`). There is no enforcement that a single `Write`
  payload fits ICMP MTU (see Change hazard 4).

### v4 vs v6 detection

Identical logic appears in both `Dial` and `icmpListenConfig.Listen`:

1. Network explicitly `ip4:icmp` → version 4.
2. Network explicitly `ip6:ipv6-icmp` → version 6.
3. Otherwise (the `ip:icmp` default, after the `"icmp"` alias is normalized):
   inspect `conn.LocalAddr().(*net.IPAddr).IP.To4()`. Non-nil → v4; nil → v6.

The generic `ip:icmp` path therefore picks the version from the **local**
address family; a mismatch with the actual peer family picks the wrong framing
(see Change hazard 5). `icmpListenConfig.Listen` also ignores the `_` error from
the `*net.IPAddr` type assertion, so a non-`*net.IPAddr` `LocalAddr` would
nil-panic on `iaddr.IP`.

### Listener accept loop & per-peer demux

- One `readLoop` goroutine owns the socket. It chooses `readBatch` if the conn
  implements `pudp.BatchReader` **and** `readBatchSize > 1`, else single `read`.
- `read` loops `pConn.ReadFrom` into a `receiveMTU`-sized buffer and dispatches.
  `readBatch` pre-allocates `readBatchSize` messages each with a `receiveMTU`
  buffer + 40-byte OOB, then dispatches each.
- `dispatchMsg` → `getConn(addr, buf)`; if a conn exists/was created, the raw
  bytes are appended to that conn's packet buffer via `conn.buffer.Write(buf)`.
- **Per-peer keying:** conns are keyed by `raddr.String()` in
  `l.conns map[string]*icmpListenerConn`. Peers are distinguished **purely by
  source IP address string** — *not* by ICMP identifier (see Change hazard 1).
- **New-peer path** (in `getConn`): under `connLock`, if no conn for the addr:
  check `accepting` (closed → `ErrClosedListener`), run `acceptFilter(buf)` if
  set (false → drop, no conn), build a conn via `newConn`, then non-blocking
  `acceptCh <- conn`. On success register in `l.conns`; if the channel is full
  (backlog exceeded) return `ErrListenQueueExceeded` and the conn is **not**
  registered (silently dropped, matching the documented backlog behavior).

**Critical demux note:** the bytes written into the per-peer buffer in
`dispatchMsg` are the **raw socket bytes** (an entire ICMP message without IP
header, since `ListenIP` on a protocol-specific raw socket delivers ICMP
payload). The server-side `icmpConn.Read` therefore parses with `b[:n]` (no
header strip) — the matching half of the client's header-stripping reads.
Whether `net.ListenIP("ip4:icmp", ...)` strips the IP header is OS/stdlib
behavior that the `b[:n]` server parse *asserts*; on platforms where `ListenIP`
includes the IP header the server framing breaks (see Change hazard 3).

### Per-peer conn (`icmpListenerConn`)

A thin packet-buffer-backed `net.Conn`:
- `Read` → `buffer.Read(p)` (pion `packetio.Buffer`, packet-framed).
- `Write` → checks the write deadline, then `listener.pConn.WriteTo(p, rAddr)`.
  All peers **share the single listener socket** for writes; `rAddr` selects the
  destination.
- `buffer = packetio.NewBuffer()` preserves message boundaries: each
  `buffer.Write` of one ICMP message is one `buffer.Read`-able packet.

---

## Concurrency model

### `icmpConn` (per-conn)
- A single `*sync.RWMutex` guards `sentHashes`, `id`, `seq`. Writers take `Lock`;
  the server's reply path takes `RLock` to read `id`/`seq`.
- `Read` and `Write` may run concurrently; the mutex makes id/seq/hash fields
  safe, while the underlying `net.Conn` Read/Write are independent (stdlib conns
  allow concurrent R/W).
- `consumeSent` takes the **write** `Lock` even though it only reads, so
  concurrent Reads serialize on it.

### Listener (shared socket, many peers)
- `connLock sync.Mutex` guards the `conns` map — taken in `getConn`,
  `icmpListener.Close`, and `icmpListenerConn.Close`.
- `accepting atomic.Value` (bool) gates new-conn creation and is read lock-free.
- `acceptCh chan *icmpListenerConn` is the bounded accept queue (capacity =
  `Backlog`); producer is `getConn` (non-blocking send), consumer is `Accept`.
- `doneCh` / `doneOnce` close the listener once.
- WaitGroups:
  - `connWG` counts the listener "self" reference (+1 in
    `icmpListenConfig.Listen`) plus one per **accepted** conn (`connWG.Add(1)` in
    `Accept`); decremented in `icmpListener.Close` and `icmpListenerConn.Close`.
    When it hits zero, the goroutine spawned in `Listen` closes the socket.
  - `readWG.Add(2)` waits for `readLoop` and that socket-close goroutine.
- `errClose` / `errRead` (`atomic.Value`) carry the terminal close error and
  read error respectively (fields declared in that order in `icmpListener`).

**Hazard:** `connWG.Add(1)` for an accepted conn happens in `Accept` but the
matching `Done` is in `icmpListenerConn.Close`. If `Accept` returns a conn that
is never `Close`d, `connWG` never drains and the socket-closing goroutine never
runs. Conversely, an unaccepted conn still sitting in `acceptCh` at listener
`Close` is torn down directly (in `icmpListener.Close`) without `connWG`
accounting (it was never `Add`ed). See Change hazard 8.

---

## Lifecycle & ownership

### `icmpConn`
Has **no** `Close` override — it embeds `net.Conn`, so `Close`, `LocalAddr`,
`RemoteAddr`, and deadlines delegate to the wrapped conn. For a client that is
the raw `*net.IPConn`; for a server that is the `*icmpListenerConn`.

### Listener `Close`
- Idempotent via `doneOnce`.
- Sets `accepting=false`, closes `doneCh`.
- Drains `acceptCh` of unaccepted conns, closing each `doneCh` and removing from
  `conns`.
- `connWG.Done()` releases the listener self-reference.
- If no remaining conns, waits `readWG` and surfaces `errClose`. If conns remain,
  returns nil and the socket is closed later when the **last** conn closes.

### Per-peer conn `Close`
- Idempotent via `doneOnce`.
- `connWG.Done()`, close `doneCh`, delete from `conns` under `connLock`.
- If this is the last conn **and** the listener is already closed, waits `readWG`
  and surfaces `errClose`.
- Closes the packet buffer; preserves `errClose` precedence over buffer-close
  error.

**Ownership:** the single OS socket (`pConn`) is owned by the listener and is
closed exactly once, by the goroutine spawned in `icmpListenConfig.Listen` when
`connWG` reaches zero. Individual peer conns never close the socket; they only
`WriteTo` it.

---

## Error, EOF & deadline semantics

### `icmpConn`
- Underlying read error → returned as-is with `n=0`.
- IP-header-strip failures: `io.ErrShortBuffer` (caller buffer too small) /
  `io.ErrUnexpectedEOF` (truncated packet).
- Parse error from `icmp.ParseMessage` → returned as-is.
- Non-Echo body → `io.ErrUnexpectedEOF`.
- There is **no synthetic `io.EOF`** — stream end is whatever the underlying
  conn returns. A peer that simply stops sending ICMP yields a blocking Read (or
  a deadline error if set).
- Deadlines delegate to the embedded `net.Conn` (no override).

### Listener / per-peer conn
- `Accept` returns `ErrClosedListener` on `doneCh`, or the stored read error on
  `readDoneCh`.
- Per-peer `Read` returns whatever `packetio.Buffer.Read` returns: `io.EOF` when
  the buffer is closed and empty, `io.ErrShortBuffer` if the supplied slice is
  smaller than the queued packet, and a timeout `net.Error` on read deadline.
- Per-peer `Write` returns `context.DeadlineExceeded` if the **write deadline**
  has already elapsed.
- `SetDeadline` sets both write and read deadline; `SetReadDeadline` →
  `buffer.SetReadDeadline`; `SetWriteDeadline` sets only the local
  `writeDeadline` and intentionally does **not** touch the shared socket's write
  deadline because the socket is shared across peers. **Consequence:** the write
  deadline is only a pre-check; an in-progress `WriteTo` blocking on the OS
  socket is not interrupted by a peer's write deadline.

---

## Operational constraints

- **Raw-socket privilege.** `net.ListenIP("ip4:icmp"/...)` and the dialer on
  `ip:icmp` open **raw IP sockets**. On Linux this requires `CAP_NET_RAW`
  (typically root, or the binary granted `cap_net_raw+ep`); without it the
  open fails and that error is returned verbatim. There is **no fallback** to
  unprivileged datagram ICMP and **no friendly error wrapping** — privilege
  failures surface as raw stdlib errors (see Change hazard 6).
- **MTU / payload size.** The receive side is capped at `receiveMTU = 8192`; a
  single ICMP message larger than that is truncated at read. On the send side,
  `icmpConn.Write` places the **entire** user buffer in one Echo `Data` field
  with no chunking and no check against MTU or `MaxPacketSize`. Payloads beyond
  the path MTU will be IP-fragmented by the OS or rejected. **There is no
  application-level segmentation here.**
- **OS auto-reply interaction.** On the client the kernel may itself answer the
  client's Echo Request with an Echo Reply (especially loopback / same-host);
  the hash-based `consumeSent` filter exists to discard those.
- **Batch I/O is Linux-only.** pion's `pudp.NewBatchConn` only wires the batch
  path on `runtime.GOOS == "linux"`; on other OSes batch config still constructs
  but falls back to per-packet read/write. The listener enables batch read only
  when `readBatchSize > 1`.
- **OOB buffer sizing.** `readBatch` allocates a 40-byte OOB per message — sized
  for an IPv6 header's worth of control data, copied from the UDP origin and not
  re-tuned for ICMP.

### Testing / exercising it

- **No e2e coverage today.** The `task test:e2e:tun` harness and `cli/internal/e2e/*`
  contain **no icmp case** (grep `Taskfile.yml` / `cli/internal/e2e/` for
  `icmp` → none); only `cli/internal/help.go` advertises the transport. Any
  test that opens an icmp listener/dialer needs `CAP_NET_RAW`
  (`sudo setcap cap_net_raw+ep <test-binary>`, or run as root) — otherwise the
  raw-socket open fails before any framing is exercised.

---

## Dependencies

**Internal:** `ipV` (`ip.go`) used by `icmpConn` and `icmpListener`. Selected /
registered by `dial.go` (`Dial`/`Listen` branches) and `transport.go`
(`TransportICMP`). The CLI references icmp in help text only
(`cli/internal/help.go`); no functional coupling there.

**Depended on by:** any higher layer using `netx.Dial`/`netx.Listen` with
network `"icmp"`. As a base transport it sits **below** the tunnel/mux stack
(Mux, TaggedDemux, DemuxClient, PollConn, DNST); those layers consume the
`net.Conn`/`net.Listener` it returns and assume a byte pipe — which ICMP does
**not** guarantee to be reliable/ordered. Because `TransportICMP` lives in the
core root module, changing the framing or version detection is a wire-compat
change every consumer must tolerate (see Change hazard 7).

**External:**
- `golang.org/x/net/icmp` — message parse/marshal, `icmp.Echo`.
- `golang.org/x/net/ipv4`, `.../ipv6` — ICMP message-type constants and
  `ipv4.Message` for batch.
- `github.com/pion/transport/v3/udp` (`pudp`) — `BatchIOConfig`, `NewBatchConn`,
  `BatchReader`, and the `ListenConfig` shape the icmp config is copied from.
- `github.com/pion/transport/v3/packetio` — per-peer packet buffer.
- `github.com/pion/transport/v3/deadline` — per-peer write deadline.
- `crypto/sha256` — self-echo suppression hashing.
- `go.mod` pins `pion/transport/v3 v3.1.1` and `golang.org/x/net v0.52.0`.

---

## Change hazards (MOST IMPORTANT)

Each fact below is stated in full at its API/Read site above; here only the
"change X → breaks Y" framing is kept.

1. **Client identifier is constant `1`; peers are demuxed by source IP only.**
   The client `id` is fixed (`NewICMPClientConn`/`icmpConn.Write`) and the
   listener keys conns solely by `raddr.String()` (`getConn`), never by ICMP
   identifier. → **Two distinct clients behind one source IP (NAT) collapse into
   one peer conn and interleave streams.** Supporting per-IP multiplexing means
   adding identifier-based keying *and* varying the client `id` — a wire-compat
   change.

2. **Self-echo filter is 8 bits wide and never evicts.** `sentHashes` is indexed
   by `seq % 256` and `consumeSent` never deletes a matched entry. → A
   legitimate server reply whose payload equals a previously-sent client payload
   with the same low-8-bit seq is **silently dropped** as a self-echo. Widening
   the key or making `consume` actually delete is wire-neutral but
   behavior-changing.

3. **Fixed 20/40-byte IP-header strip + server/client asymmetry.** The client
   read strips exactly 20 (v4) / 40 (v6) bytes (`icmpConn.Read`), while the
   server parses with no strip (`b[:n]`). This hard-codes that `DialIP` reads
   *with* the IP header but `ListenIP` delivers ICMP *without* it — OS-dependent.
   → IPv4 packets with options (header > 20 bytes), or a platform where
   `ListenIP` includes the IP header, misparse/corrupt framing. Any change to
   socket type must revisit these constants on both sides.

4. **No payload segmentation vs MTU; receive cap 8192; silent read truncation.**
   `Write` emits the whole buffer as one Echo `Data` with no `MaxPacketSize` /
   path-MTU check, and `Read`'s `copy(b, pkt.Data)` truncates silently if the
   caller's slice is smaller. → Large writes get IP-fragmented (fragile across
   firewalls) or truncated on read. Introducing chunking/reassembly is a
   wire-compat change upper tunnel layers must tolerate.

5. **Generic `ip:icmp` version detection depends on local-address `To4()`.**
   `Dial` and `icmpListenConfig.Listen` pick the version from
   `LocalAddr().(*net.IPAddr).IP.To4()`, and the listener drops the type-assert
   error (would nil-panic on a non-`*net.IPAddr` `LocalAddr`). → A v4-mapped-v6
   local address or a local/peer family mismatch selects the wrong ICMP
   type/proto and produces packets the peer rejects. Prefer the explicit
   `ip4:icmp` / `ip6:ipv6-icmp` networks.

6. **Privilege failures are unwrapped (fail-hard).** Both the `ListenIP` and
   `DialContext` paths surface raw OS errors with no `CAP_NET_RAW`/root hint.
   → Callers/tests likely assert on the stdlib error; changing error handling to
   wrap it will break them. Preserve (or improve without renaming) the
   diagnosability.

7. **The framing is the wire contract for the tunnel stack.** The Echo
   type/code/id/seq/Data layout (`icmpConn.Write`) and the "server echoes
   last-seen id/seq" reply rule (`icmpConn.Read`/`Write`) are the wire format
   that Mux/TaggedDemux/DemuxClient/PollConn/DNST ride on. → Any change to
   id/seq semantics, payload placement, or self-echo filtering can desync
   client/server or corrupt the multiplexed streams above. Treat as a stable
   wire contract.

8. **`connWG` bookkeeping coupling.** An accepted-but-never-`Close`d server conn
   pins `connWG` above zero and prevents the socket-closing goroutine from ever
   running (`Accept` does the `Add`, `icmpListenerConn.Close` does the `Done`).
   → Refactors of accept/close must keep the `Add`/`Done` pairing exact, or the
   listener leaks the socket on shutdown.

---

## Related docs

Index in [README.md](README.md). Companion subsystem docs:
[pipeline.md](pipeline.md), [mux.md](mux.md), [demux.md](demux.md),
[poll-tagged.md](poll-tagged.md), [stream-transforms.md](stream-transforms.md),
[server-tun.md](server-tun.md), [drivers-proto.md](drivers-proto.md),
[modules-cli.md](modules-cli.md).
