# DNS-Tunnel Stack: Mux + DNST + Demux + Poll + Split + Frame

This doc covers ONE thing that lives nowhere else: how the stateless-DNS-tunnel layers
compose, in the **real wire order**, and the cross-layer invariants that make that order
load-bearing. It deliberately does **not** re-document each type's API — for constructors,
options, and per-type mechanics see:

- `docs/internals/mux.md` — `Mux` / `MuxClient`
- `docs/internals/demux.md` — `Demux` / `TaggedDemux` / `DemuxClient` / `DemuxDialer`
- `docs/internals/poll-tagged.md` — `PollConn` / `PollServerConn`, `TaggedConn`
- `docs/internals/stream-transforms.md` — `SplitConn` / `FrameConn` (and `BufConn`)
- `docs/internals/drivers-proto.md` — `dnst` encoding

---

## Why this stack exists

DNS is **stateless and strictly request→response**: a server can only emit bytes as the
reply to a query it just received, queries carry tiny payloads, and the protocol cannot
distinguish clients or sessions on its own. To run a persistent, multiplexed, bidirectional
tunnel over it, netx stacks six wrappers, each fixing one of those limitations:

| Wrapper | Fixes |
|---------|-------|
| `mux` (server) / `mux` client | adapts the connection-oriented transport to a single (Tagged)Conn |
| `dnst` | encodes payload into DNS query QNAME (up) / TXT answer (down) |
| `demux` | carves multiple sessions out of the single conn via an ID prefix |
| `poll` | turns request→response into a persistent stream (client polls when idle) |
| `split` | chops writes down to the per-packet `MaxWrite` budget dnst imposes |
| `frame` | restores message boundaries (2-byte length prefix) over the resulting byte stream |

---

## Canonical URI (same scheme on both ends)

The e2e suite wires the full tunnel with one scheme string, used **identically** on the
server `--from` and the client `--to`:

```
udp+mux+dnst{domain=t.com}+demux{id=0000}+poll+split+frame://<addr>
```

- `id=0000` is two hex bytes → a **2-byte** session ID (not 4).
- See `Taskfile.yml` (the `SDNST_*`/`CDNST` tunnels) for the live invocation, and run
  `task test:e2e:tun` to exercise it end-to-end (`task test:e2e:tun lib=true` for the
  c-shared build).

The scheme is parsed left→right (`Wrappers.UnmarshalText` in `wrap.go`), so **the leftmost
wrapper (`mux`) sits closest to the transport and the rightmost (`frame`) closest to the
application.** The same string resolves to different wrappers per side because the
`listener bool` is threaded through every driver:

| Wrapper | Server side (starts at `Listener`) | Client side (starts at `Dialer`) |
|---------|------------------------------------|----------------------------------|
| `mux`   | `NewMux`: Listener → **TaggedConn** | `NewMuxClient`: Dialer → Conn |
| `dnst`  | `dnst.NewTaggedServerConn`: TaggedConn → TaggedConn | `dnst.NewClientConn`: Conn → Conn |
| `demux` | `NewTaggedDemux`: TaggedConn → **Listener** | `NewDemuxClient`: Conn → **Dialer** |
| `poll`  | `NewPollServerConn` (per accepted conn): Listener → Listener | `NewPollConn`: Dialer → Dialer |
| `split` | `NewSplitConn`: Listener → Listener | Dialer → Dialer |
| `frame` | `NewFrameConn`: Listener → Listener | Dialer → Dialer |

The chain only validates if the final type is `Listener` (server) or `Dialer` (client) —
which is why an invalid ordering fails at parse time, not at connect time.

> Constructor reminders (these have drifted in older docs): `NewMux` returns a
> **`TaggedConn`** (its tag is the source `net.Conn`, used by `WriteTagged` to reply on the
> exact connection a request arrived on). The dnst constructors are
> `dnst.NewServerConn` / `dnst.NewTaggedServerConn` / `dnst.NewClientConn` — there is no
> `NewDNSTServerConn`.

---

## Data flow

Both diagrams show **all six layers** in the real order. `App` is the tunneled payload
(whatever the tun relay carries).

### Upstream (client → server)

```
CLIENT                                              SERVER
App bytes                                           App bytes (per session)
  │ Write                                             ▲ Read
  ▼                                                   │
frame   ── prepend 2-byte length                  frame   ── reassemble framed messages
  │                                                   ▲
  ▼                                                   │
split   ── chop to MaxWrite budget                split   (no-op on read)
  │                                                   ▲
  ▼                                                   │
poll    ── send on next request cycle             poll(server) ── deliver request payload via Read
  │                                                   ▲
  ▼                                                   │
demux   ── prepend session id (DemuxClient)       demux   ── strip id, route to session (TaggedDemux)
  │                                                   ▲
  ▼                                                   │
dnst    ── base32 → QNAME labels, TXT query       dnst    ── decode QNAME, tag = {dns.Msg, conn tag}
  │                                                   ▲
  ▼                                                   │
mux     ── dial conn on demand (MuxClient)        mux     ── ReadTagged from shared queue, tag = source conn
  │                                                   ▲
  ▼                                                   │
UDP ───────────────── DNS query on the wire ──────────┘
```

### Downstream (server → client)

The server can only reply to a query it has already received, so every downstream byte
rides back out as the **answer to the originating request** (carried by the tag).

```
SERVER                                              CLIENT
App bytes (session handler)                         App bytes
  │ Write                                             ▲ Read
  ▼                                                   │
frame   ── prepend 2-byte length                  frame   ── reassemble framed messages
  │                                                   ▲
  ▼                                                   │
split   ── chop to MaxWrite budget                split   (no-op on read)
  │                                                   ▲
  ▼                                                   │
poll(server) ── queue as next response            poll    ── buffer in recv queue
  │                                                   ▲
  ▼                                                   │
demux   ── prepend id, WriteTagged(payload, tag)  demux   ── verify+strip id (DemuxClient)
  │       (TaggedDemux consumes one tag)             ▲
  ▼                                                   │
dnst    ── base32 → TXT answer (255-byte strings) dnst    ── parse TXT answer, base32-decode
  │       reuses the queried dns.Msg                 ▲
  ▼                                                   │
mux     ── WriteTagged routes to the exact         mux     ── Read from current dialed conn (MuxClient)
  │        source conn (tag = net.Conn)              ▲
  ▼                                                   │
UDP ───────────────── DNS answer on the wire ─────────┘
```

**Idle keep-alive:** when the client app has nothing to send, `PollConn`'s loop fires every
`interval` (default **1ms**) and writes `nil`. `DemuxClient` still emits the bare session
ID, `dnst` still forms a valid empty query, and the server replies with any queued data.
This pending-request-at-all-times is the only way the server can push data downstream.
(Empty *user* `Write` calls are a no-op — `len(b)==0` returns `(0,nil)`; the nil poll is
driven solely by the interval timer.)

---

## Layer-ordering invariants

These are the rules that make the order above non-negotiable. Reordering layers without
respecting them silently breaks the tunnel.

**1. `MaxPacketSize` (65535) bounds every read buffer.** `MaxPacketSize` is defined in
`packet.go`. Mux, Demux/TaggedDemux, DemuxClient, and both Poll loops all allocate a
`make([]byte, MaxPacketSize)` read buffer, and demux's `Write` rejects payloads where
`len(b)+len(id) > MaxPacketSize` (the `"demux: packet too large"` error). It also caps the
2-byte frame length header's range.

**2. The `MaxWrite` budget chains downward — `split` MUST sit below something that exposes it.**
A layer advertises a per-write byte budget via `interface{ MaxWrite() uint16 }`:

- `dnst` sets the budget: server `MaxWrite` defaults to **765** (`WithMaxWrite` in
  `dnst_conn.go`); the client computes it from the domain length (`maxQNAMEPayload`).
- `Demux`/`TaggedDemux` and `DemuxClient` subtract the id length and re-export the
  remainder (see the `MaxWrite()` probe in `NewDemux`/`NewTaggedDemux`/`NewDemuxClient`);
  a too-small budget fails construction (`"...MaxWrite is too small for ID"`).
- `PollConn`/`PollServerConn` forward the budget unchanged (`MaxWrite` in `poll_conn.go`).
- `SplitConn` *consumes* it: `NewSplitConn` returns an error if the underlying conn lacks
  `MaxWrite` or reports 0. **This is why `split` is appended right after the
  budget-bearing layers** — it must wrap a conn that still exposes a non-zero `MaxWrite`,
  and it chops writes to that size so dnst never has to truncate.

**3. `frame` is outermost (closest to the app) because it restores boundaries `split` destroyed.**
`split` chops one logical message into several `MaxWrite`-sized chunks; `frame` (the 2-byte
big-endian length prefix in `FrameConn`) re-delineates messages on top of that byte stream.
If `frame` sat below `split`, its length header could itself be split and corrupted. `frame`
also `Flush`es when the underlying conn implements `BufConn`, coalescing its header+payload
writes.

> ⚠️ **Stale source comment:** `frame_conn.go`'s package comment and `NewFrameConn` doc say
> the length header is **"4-byte"**, but the code uses `[2]byte` / `binary.*Uint16` — it is
> a **2-byte** (`uint16`) header. Trust the code. (Likewise `buffered_conn.go`'s doc comment
> still references `WithBufWriterSize`/`WithBufReaderSize`; the real options are
> `WithBufWrite`/`WithBufRead`.)

**4. A `TaggedDemux` session cannot push before it has received.** `TaggedDemux` carries the
DNS query (the tag) from read → write so a response reuses its originating query. A session's
`Write` **blocks on `tagQueue` until a tag is available** (`taggedDemuxSess.Write` in
`demux_tagged.go`): a tag is only enqueued when the session's `Read` consumes a request.
So a session cannot emit server-initiated data until the client has first sent (and the
session has Read) a request — exactly the DNS request/response coupling, surfaced as a hard
invariant. On the underlying `Mux`, the analogous rule holds: `WriteTagged` requires a
`net.Conn` tag (from a prior `ReadTagged`) and routes the reply to that exact connection.

---

## Operational gotchas (change hazards)

- **Drop-on-full, not block.** Demux's read loop drops a *whole new session* if the accept
  queue is full, and drops a *packet* if a session's read queue is full (both `WarnContext`
  logged, in `processPacket`). Defaults: accept queue **1** (`WithDemuxAccQueue`),
  per-session read queue **128** (`WithDemuxReadQueue`). Mux likewise drops nothing but is
  bounded by its shared read queue (`WithMuxReadQueue`). Under bursty or oversized traffic,
  tune these before assuming data loss is a bug.
- **Server idle reclamation.** `WithPollTimeout` (default **0** = off; server-only) makes
  `PollServerConn`'s loop set a read deadline and, on timeout, close the underlying conn.
  That close removes the demux session-map entry eagerly, so a reconnecting client with the
  same session ID gets a fresh session instead of landing in a dead one. Without a timeout a
  silently-gone client keeps its session alive indefinitely.
- **Poll interval is client-only; timeout is server-only.** The driver rejects the wrong
  one per side (`WithPollInterval` for clients, `WithPollTimeout` for servers).
- **`MuxClient` has no addr overrides.** Its only option is `WithMuxClientLogger`;
  `LocalAddr`/`RemoteAddr` always return a synthetic `muxVirtualAddr`.

---

## Minimal hand-wired example

You normally build this stack from the URI, but hand-wiring shows the types. Note the dnst
constructor name and that `NewTaggedDemux` returns `(net.Listener, error)`.

```go
// Server: net.Listener → Mux (TaggedConn) → dnst (TaggedConn) → TaggedDemux (Listener)
//         → poll(server) → split → frame  (per accepted session)
muxed := netx.NewMux(udpListener)                              // TaggedConn
tagged := dnst.NewTaggedServerConn(muxed, "tunnel.example.com") // TaggedConn
ln, err := netx.NewTaggedDemux(tagged, 2 /* id bytes */, netx.WithDemuxAccQueue(16))
// then wrap each Accept()ed conn with NewPollServerConn → NewSplitConn → NewFrameConn

// Client: Dialer → MuxClient (Conn) → dnst (Conn) → DemuxClient (Dialer)
//         → poll → split → frame
muxc := netx.NewMuxClient(func() (net.Conn, error) {
    return net.Dial("udp", "8.8.8.8:53")
})
dnstc := dnst.NewClientConn(muxc, "tunnel.example.com")        // net.Conn
dial := netx.NewDemuxClient(dnstc, []byte{0x00, 0x00})         // Dialer
conn, _ := dial()
poll := netx.NewPollConn(conn)                                 // wrap; then split → frame
```
