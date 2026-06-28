# Stream Transforms (Buffered / Framed / Split)

> Internals reference for the three byte-level `net.Conn` transforms in the root
> `netx` package: `bufConn` (buffered I/O + explicit `Flush`), `frameConn`
> (length-prefixed framing), and `splitConn` (write chunking to honour an
> underlying `MaxWrite` limit). Goal: let future changes avoid breaking wire
> compatibility, capability detection, or composition.
>
> Code is cited by **symbol + file** (e.g. `NewFrameConn (frame_conn.go)`), not
> line number, so cites survive refactors. Where the code contradicts its own doc
> comments or the names commonly assumed for it, that is flagged explicitly under
> **Naming / doc drift** — do not trust the comments over the code.

---

## Purpose & role

These three wrappers are pure byte/stream transforms layered onto an existing
`net.Conn`. They sit low in a connection pipeline and are composed by the driver
registry (see [drivers-proto.md](drivers-proto.md), [pipeline.md](pipeline.md)).

- **`bufConn`** (`buffered_conn.go`) — wraps a `net.Conn` with a `bufio.Reader`
  and `bufio.Writer` to reduce syscalls for small I/O. Adds an explicit
  `Flush() error`.
- **`frameConn`** (`frame_conn.go`) — adds a length-prefixed framing protocol so
  packet/message boundaries survive a stream transport (e.g. UDP semantics over
  TCP+TLS). One `Write` == one frame; each `Read` returns at most one frame's
  bytes.
- **`splitConn`** (`split_conn.go`) — removes a `MaxWrite` limitation of the
  conn beneath it by splitting large `Write` calls into `<= MaxWrite`-byte
  chunks.

Typical stacking order (innermost transport first): a transport that imposes a
`MaxWrite` (e.g. `dnst` client conn) -> `splitConn` -> ... -> `bufConn` ->
`frameConn`. See **Composition & dependencies**.

### Running the tests

These live in the **root module**, so run them from the repo root (no `cd`):

```bash
go test -run TestBufConn .    # buffered_conn_test.go
go test -run TestFrameConn .  # frame_conn_test.go
go test -run TestSplitConn .  # split_conn_test.go
```

---

## Naming / doc drift (read first)

The actual exported symbols differ from several commonly-assumed names. Use the
real ones:

| Assumed name           | Actual symbol (file)                               |
|------------------------|----------------------------------------------------|
| `NewFramedConn`        | `NewFrameConn` (frame_conn.go)                     |
| `FramedConn` type      | unexported `frameConn` (frame_conn.go); constructor returns plain `net.Conn` |
| `WithBufSize` / `WithBufReaderSize` / `WithBufWriterSize` | `WithBufRead` / `WithBufWrite` (buffered_conn.go) |
| `WithMaxFrameSize` option | **does not exist**; the `frame` driver rejects all params |
| `maxsize` frame param  | **does not exist**; see above                      |
| `ErrFrameTooLarge`     | **does not exist** in this code (verified absent)  |

Two **stale source comments** contradict the code. They are deliberately NOT
fixed in the code; this doc is the source of truth. Do not rely on the comments:

- The header comment on `NewBufConn` (buffered_conn.go) says "By default, the
  buffer size is 4KB. Use WithBufWriterSize and WithBufReaderSize…". The 4KB
  figure is whatever `bufio.NewReader`/`bufio.NewWriter` default to (4096 at time
  of writing), and the named options **do not exist** — the real options are
  `WithBufRead`/`WithBufWrite`. If you ever correct it, the accurate text is:
  *"By default, the buffer size is the bufio default (4096). Use WithBufRead and
  WithBufWrite to customize the sizes."*
- The `frame_conn.go` file header and the `NewFrameConn` doc comment claim a
  **"4-byte big-endian length header"**. The code uses a **2-byte** (`uint16`)
  header (see **Frame header layout**). This is a real wire-format hazard. The
  accurate text is *"2-byte big-endian unsigned integer"*. **Fix the comment, not
  the code** — see **Change hazards #1**.

---

## Public API (exported symbols + options + driver params)

### BufConn

- `type BufConn interface { net.Conn; Flush() error }` (buffered_conn.go). This
  interface is also the **Flush-capability interface** the framing layer detects
  (see below).
- `func NewBufConn(c net.Conn, opts ...BufConnOption) BufConn` (buffered_conn.go).
  Defaults: `br = bufio.NewReader(c)`, `bw = bufio.NewWriter(c)` — i.e. `bufio`
  package defaults (4096 bytes each at time of writing).
- `type BufConnOption func(*bufConn)` (buffered_conn.go).
- `func WithBufWrite(size uint16) BufConnOption` (buffered_conn.go) — replaces
  the writer with `bufio.NewWriterSize(bc.Conn, int(size))`.
- `func WithBufRead(size uint16) BufConnOption` (buffered_conn.go) — replaces the
  reader with `bufio.NewReaderSize(bc.Conn, int(size))`.
  - **Contract note:** `size` is `uint16`, so max configurable buffer is 65535.
    A `0` does **not** create a zero-length buffer, but the two clamp
    *differently*: `bufio.NewReaderSize` uses `max(size, 16)` so `WithBufRead(0)`
    gets a 16-byte buffer (the bufio read minimum); `bufio.NewWriterSize` treats
    `size <= 0` as `defaultBufSize`, so `WithBufWrite(0)` gets a 4096-byte buffer,
    **not** 16.

**`buf` driver** (registered name `"buf"` in the `init` of buffered_conn.go).
Params:
  - `r` -> `WithBufRead(uint16)`; value parsed via `strconv.ParseUint(value,10,16)`.
  - `w` -> `WithBufWrite(uint16)`; same parse.
  - a non-numeric/out-of-range value for `r`/`w` -> error
    `buf: invalid read size parameter %q` / `buf: invalid write size parameter %q`.
  - any other key -> error `buf: unknown buffered parameter %q`.
  - Provides `ListenerToListener`, `DialerToDialer`, `ConnToConn` wrappers.

### FrameConn

- `func NewFrameConn(c net.Conn) net.Conn` (frame_conn.go). Returns `*frameConn`
  (unexported) as a plain `net.Conn`. Allocates a read scratch buffer
  `buf = make([]byte, MaxPacketSize)` (`MaxPacketSize = 65535`, packet.go).
- No options. The `frame` driver (name `"frame"`, in the `init` of frame_conn.go)
  accepts **zero** params: the first param key triggers
  `uri: unknown frame parameter %q`. Provides the same three wrapper hooks.

### SplitConn

- `func NewSplitConn(c net.Conn) (net.Conn, error)` (split_conn.go). Detects the
  MaxWrite-capability via type assertion (see below). Returns a `*splitConn`
  (unexported) or an error.
- No options. The `split` driver (name `"split"`, in the `init` of split_conn.go)
  accepts **zero** params (the first key errors `split: unknown parameter %q`).
  Provides the three wrapper hooks.

### Driver registration mechanics (shared)

Each file calls `Register(name, Driver)` in its `init()`. `Register` (driver.go)
**panics on a duplicate name** (`"uri: Register called twice for driver "`) and
on a `nil` driver (`"uri: Register driver is nil"`). `Driver` is
`func(params map[string]string, listener bool) (Wrapper, error)` (driver.go).
The `Wrapper` struct is defined in wrap.go. The conn-level closures use
`ConnWrapListener`/`ConnWrapDialer` (wrap.go); `Dialer` is
`type Dialer = func() (net.Conn, error)` (mux_client.go).

---

## Wire formats & capability interfaces

### Frame header layout (the wire format)

```
+--------+--------+============================+
| len hi | len lo |  payload (len bytes)       |
+--------+--------+============================+
   byte0   byte1     0..65535 bytes
```

- **2 bytes, big-endian, `uint16`.** Write side:
  `binary.BigEndian.PutUint16(hdr[:], uint16(len(p)))` in `frameConn.Write`; read
  side: `binary.BigEndian.Uint16(hdr[:])` in `frameConn.Read`.
- Header and payload are written as **two separate `Conn.Write` calls** in
  `frameConn.Write`. This is why buffering+Flush coalescing exists (next section).
- **Max payload is implicitly 65535**, because `uint16(len(p))` silently
  truncates anything larger in `frameConn.Write`. There is no explicit length
  check and no `ErrFrameTooLarge`. A `Write` of > 65535 bytes writes a wrong
  (truncated) length header and the full payload, corrupting the stream. See
  **Change hazards**. The companion `MaxPacketSize = 65535` constant (packet.go)
  is the intended ceiling but is **not enforced** in `frameConn.Write`.

### The Flush-capability interface

Defined as `BufConn` (buffered_conn.go):

```go
type BufConn interface {
	net.Conn
	Flush() error
}
```

- **Detected by** `frameConn.Write`: `if fw, ok := c.Conn.(BufConn); ok { fw.Flush() }`.
  After writing header+payload, if the underlying conn satisfies `BufConn` the
  frame layer flushes immediately, coalescing the two writes into one syscall.
- **Implemented by** `bufConn` only: `func (c *bufConn) Flush() error`
  (buffered_conn.go). No other conn in the repo defines `Flush() error`
  (verified by search). In particular `frameConn` and `splitConn` do **not**
  expose `Flush`.

### The MaxWrite-capability interface

There is **no named interface**; every consumer uses an inline anonymous
interface type:

```go
interface{ MaxWrite() uint16 }
```

- **Detected by** `NewSplitConn` (split_conn.go): asserts
  `c.(interface{ MaxWrite() uint16 })` and requires `ok && mw.MaxWrite() != 0`.
- **Same shape is consumed across the codebase** (this is a cross-subsystem
  contract — see **Composition**): in `demux.go`, `demux_tagged.go`,
  `demux_client.go`, `proto/aesgcm/aesgcm_conn.go`, and both `pollConnServer` /
  `pollConnClient` `MaxWrite` methods in poll_conn.go.
- **Providers of `MaxWrite() uint16`:**
  - `proto/dnst` client conn — `clientConn.MaxWrite` (dnst_conn.go), computed
    from domain length in `NewClientConn` via `maxQNAMEPayload`; this is the
    canonical upstream source feeding `splitConn`. Server variants:
    `serverConn.MaxWrite` and `taggedServerConn.MaxWrite`.
  - `proto/aesgcm` conn — `aesgcmConn.MaxWrite` (aesgcm_conn.go) forwards a
    reduced limit.
  - demux sessions — `demuxSess.MaxWrite`, `taggedDemuxSess.MaxWrite`,
    `demuxClient.MaxWrite`. Each returns a **stored field** (e.g.
    `s.demux.maxWrite`, `m.writeMax`) that was computed as `underlying - idLen`
    at construction, not in the accessor.
  - poll conns forward the inner limit (`pollConnServer.MaxWrite`,
    `pollConnClient.MaxWrite`).

---

## Internal design & invariants

### bufConn

- Struct: embeds `net.Conn`, plus `br *bufio.Reader`, `bw *bufio.Writer`
  (buffered_conn.go).
- `Read` -> `br.Read`; `Write` -> `bw.Write`. Buffered until `Flush`/`Close`.
- Invariant relied on by `frameConn`: a `bufConn` write is not visible to the
  peer until `Flush` (asserted by `TestBufConnReadWrite`).

### frameConn

- Struct: embeds `net.Conn`, plus `pending []byte`, `buf []byte`, and two
  mutexes `rmu, wmu` (frame_conn.go).
- **Read state machine** (`frameConn.Read`):
  1. If `pending` (leftover from a previous over-large frame) is non-empty,
     copy from it and shrink it.
  2. Else read the 2-byte header with `io.ReadFull`, decode `n`.
  3. If caller's `p` can hold the whole frame (`len(p) >= n`), read the payload
     straight into `p`.
  4. Otherwise read the full payload into the scratch `buf`, copy what fits into
     `p`, and stash the rest in `pending`. Subsequent `Read`s drain `pending`
     (step 1). This is how a single large frame is delivered across multiple
     `Read` calls (asserted by `TestFrameConnPartialRead`).
- **Write** (`frameConn.Write`): write header; if payload empty return `0, nil`
  *after* the header write — i.e. an empty frame is a valid 2-byte-only wire
  unit; else write payload; then opportunistic Flush; return `len(p)`.
- **Invariant — scratch buffer size:** `buf` is `MaxPacketSize` (65535) bytes.
  The slice expression `c.buf[:n]` in `frameConn.Read` assumes `n <= 65535`,
  which always holds because `n` came from a `uint16` header. If the header width
  ever changes to allow larger `n`, that slice would panic.

### splitConn

- Struct: embeds `net.Conn`, plus `maxWrite int` (split_conn.go), captured once
  in `NewSplitConn`.
- `Write` loop (`splitConn.Write`): take `chunk = b[:maxWrite]` (or all of `b` if
  smaller), write it via `sc.Conn.Write`, accumulate `total`, advance `b` by the
  bytes actually written `n`, stop on error returning partial `total`. No `Read`
  override — reads pass through the embedded `net.Conn`.
- **Invariant:** `maxWrite > 0` (guaranteed by the `NewSplitConn` check), so the
  loop always makes progress.

---

## Concurrency model

- **bufConn:** *no internal locks.* It relies on `bufio.Reader`/`bufio.Writer`,
  which are **not** safe for concurrent use within the same direction. Concurrent
  `Read`+`Write` on a `bufConn` is safe only insofar as reader and writer are
  independent objects (they are: `br` vs `bw`). Concurrent `Write`+`Write` or
  `Read`+`Read` is **not** safe. `Close` calling `bw.Flush()` concurrently with
  an in-flight `Write` is a data race.
- **frameConn:** has `rmu` (read) and `wmu` (write) mutexes. `Read` holds `rmu`;
  `Write` holds `wmu`. So concurrent **Read+Write is safe**, and concurrent
  Read+Read / Write+Write are individually serialized. The two locks are
  independent — there is no cross-lock ordering, so no deadlock between them.
- **splitConn:** *no locks.* A single `Write` issues multiple underlying writes
  while holding nothing; two concurrent `splitConn.Write`s can **interleave their
  chunks** on the wire and corrupt framing. Safe concurrent use requires the
  caller (or a layer above) to serialize writes. Read passes through unguarded.

---

## Lifecycle & ownership

- **bufConn.Close** (buffered_conn.go): flush-then-close with a **joined error**.
  It attempts `bw.Flush()`; if it errors, joins it via `errors.Join` but
  **still** attempts `Conn.Close()` and joins that error too. Returns the
  combined error. Nil-guards on `bw` and `Conn`. Net effect: buffered-but-
  unflushed data is flushed on Close, and a flush failure does not skip the
  close.
- **frameConn:** no `Close` override -> the embedded `net.Conn.Close` is used.
  It does **not** flush an underlying `bufConn` on close (it only flushes
  per-Write). If the very last `Write` succeeded, its data was already flushed;
  there is no separate close-time flush.
- **splitConn:** no `Close` override -> embedded `net.Conn.Close`. Ownership of
  the wrapped conn is transferred to the wrapper in all three cases (closing the
  wrapper closes the chain down to the innermost conn).

---

## Error semantics

- **Empty frames:** `frameConn.Write(nil)`/`Write([]byte{})` writes a 2-byte
  zero-length header and returns `(0, nil)`. On the read side an empty frame
  yields `(0, nil)` — i.e. it is **delivered**, not swallowed. Asserted by
  `TestFrameConnDeliversEmptyFrames`: two empty frames each read as `n=0,
  err=nil` before the real payload. **Hazard:** code reading from a `frameConn`
  must treat `n==0, err==nil` as "received an empty packet", not as EOF/spin — a
  naive `io.ReadFull` loop will not advance on empty frames.
- **Oversized frame:** there is **no `ErrFrameTooLarge`** and no length check.
  Payloads > 65535 are silently truncated in the header (`uint16` conversion in
  `frameConn.Write`) while the full payload is still written -> stream
  corruption. (Contrast: the demux layer *does* enforce `MaxPacketSize` — see the
  `len(b)+len(s.id) > MaxPacketSize` guards in demux.go / demux_tagged.go —
  framing does not.)
- **split: MaxWrite missing/zero:** `NewSplitConn` returns
  `errors.New("split: underlying connection does not implement MaxWrite or has no MaxWrite limit")`
  and a `nil` conn when the assertion fails or `MaxWrite()==0`. Asserted by
  `TestSplitConn_NoMaxWriteError`, which also checks the returned conn is `nil`.
- **split: partial write:** `splitConn.Write` returns the partial `total` plus
  the underlying error on any chunk failure, preserving `io.Writer` semantics.

---

## Composition & dependencies

### Capability matrix — consumes vs exposes

| Wrapper    | Consumes (detects)        | Exposes to layers above it                                   |
|------------|---------------------------|-------------------------------------------------------------|
| `bufConn`  | nothing                   | `Flush() error` (`bufConn.Flush`) — the `BufConn` capability. Does **not** expose `MaxWrite`. |
| `frameConn`| `BufConn` (Flush) on its underlying conn (in `frameConn.Write`) | **neither** `Flush` nor `MaxWrite` — only methods of the `net.Conn` interface it embeds. |
| `splitConn`| `MaxWrite() uint16` on its underlying conn (in `NewSplitConn`) | **does not** re-expose `MaxWrite` (it *satisfies* the limit, so layers above see effectively unlimited writes). |

Crucial embedding detail: all three embed the **`net.Conn` interface**, not a
concrete type. Go only promotes methods declared on `net.Conn`, so `Flush` and
`MaxWrite` of an inner conn are **not** auto-forwarded through `frameConn` or
`splitConn`. Consequences:

- **`frameConn` hides `MaxWrite`:** putting `frameConn` *above* a dnst/aesgcm
  conn breaks `splitConn` detection if `splitConn` sits above the frame. Put
  `splitConn` directly above the `MaxWrite`-bearing transport.
- **`frameConn` hides `Flush`:** wrapping a `bufConn` in a `frameConn` means a
  third layer above cannot detect Flush on the `frameConn`. Flush coalescing is
  only between `frameConn` and the immediately-underlying `bufConn`.
- **`splitConn` intentionally drops `MaxWrite`:** after `splitConn`, upper
  layers (e.g. another `splitConn`, aesgcm, demux) will *not* see a `MaxWrite`
  and will treat writes as unconstrained — which is the whole point.

### The frame ↔ buf interplay (Flush coalescing)

`frameConn.Write` does header-write + payload-write as two calls. If the
underlying conn is a `bufConn`, both land in `bw` and the trailing `Flush` emits
them as one syscall. This is the documented reason to stack `bufConn` beneath
`frameConn`. Recommended order: `frameConn(bufConn(rawConn))`.

### The dnst -> split MaxWrite chain

`dnst` client conn computes a `MaxWrite` from the configured domain
(`NewClientConn` / `maxQNAMEPayload` in proto/dnst/dnst_conn.go). `splitConn`
reads that value once in `NewSplitConn` and chunks every write to fit a single
DNS QNAME. The `dnst` driver's `maxw` listener param (drivers/dnst/dnst.go,
applied via `dnstproto.WithMaxWrite`) tunes the server-side `MaxWrite`. So the
chain is: `dnst conn.MaxWrite()` -> `splitConn` chunking. Breaking either the
interface shape or the dnst computation silently changes (or disables) chunking.

### Depended-on-by

- Anything detecting Flush (only `frameConn`, in `frameConn.Write`).
- Anything detecting `MaxWrite` — `splitConn` plus demux/aesgcm/poll layers
  listed under **The MaxWrite-capability interface**. These all hardcode the
  inline `interface{ MaxWrite() uint16 }` shape; it is a de-facto repo-wide
  contract even though it is not a named type.

---

## Change hazards (MOST IMPORTANT)

1. **Frame header width / endianness is the wire format.** Changing the 2-byte
   `uint16` header in `frameConn.Read`/`frameConn.Write` — to 4 bytes (as the
   stale comments in frame_conn.go wrongly describe), or to little-endian —
   breaks compatibility with every already-deployed peer. Both read and write
   sides must change atomically, and it is an on-wire breaking change for any
   mixed-version deployment. **If you "fix" the comment by changing the code to 4
   bytes, you break the protocol; the safe fix is to correct the comment to say
   2 bytes** (see **Naming / doc drift** for the exact text).
2. **No max-frame enforcement -> silent truncation.** `uint16(len(p))` in
   `frameConn.Write` truncates payloads > 65535 with no error, while still
   writing the full body -> corrupted stream and no `ErrFrameTooLarge`. If you
   add enforcement, decide between returning an error vs auto-splitting; either
   changes caller-visible behaviour. The read-side scratch slice `c.buf[:n]` in
   `frameConn.Read` also hard-assumes `n <= 65535`.
3. **Removing/altering Flush propagation reintroduces header/payload
   fragmentation.** If the `BufConn` assertion in `frameConn.Write` is removed,
   renamed, or the `BufConn` interface (buffered_conn.go) changes shape,
   `frameConn` reverts to two syscalls per frame and (worse) data may sit
   unflushed in the buffer until Close, stalling the peer (the buffering-blocks
   behaviour is exactly what `TestBufConnReadWrite` proves).
4. **`splitConn` silently no-ops / mis-detects if the MaxWrite contract
   breaks.** The detection is an *inline* `interface{ MaxWrite() uint16 }` (in
   `NewSplitConn`) duplicated in 6+ places. Changing the method name, signature
   (e.g. to `int` or to return an error), or a provider failing to expose it
   makes `NewSplitConn` error out at construction — or, if a provider returns 0,
   also error. Worse, a layer inserted *between* the `MaxWrite` provider and
   `splitConn` that doesn't forward `MaxWrite` (e.g. `frameConn`) silently breaks
   the chain. Consider introducing a single named interface to make this contract
   greppable.
5. **Concurrent-write safety differs per layer.** `frameConn` is safe for
   concurrent Read+Write (`rmu`/`wmu`) but `bufConn` and `splitConn` have **no
   write lock**. Two goroutines writing the same `splitConn` can interleave
   chunks and corrupt frames; two writing a `bufConn` race on `bufio.Writer`.
   Adding parallel writers above these layers without external serialization is a
   latent corruption bug. Do not assume the framing layer's safety extends
   downward.
6. **Default buffer sizes affect throughput and the per-call max.** `NewBufConn`
   uses `bufio` defaults (4096); options cap at `uint16` 65535. Shrinking
   defaults raises syscall count; the stale "4KB" comment on `NewBufConn` will
   mislead anyone tuning this. Note also a tiny `WithBufWrite(size)` smaller than
   a frame does not break framing (bufio splits), but undersizing relative to
   typical frame size defeats the coalescing benefit.
7. **`splitConn` captures `maxWrite` once at construction** (`NewSplitConn`). If
   an underlying transport's `MaxWrite` ever became dynamic, `splitConn` would
   not observe the change. Today all providers return a fixed value, so this is
   latent, not active.

---

## Tests covering this

All three test files are in the root module. See **Running the tests** above.

- `buffered_conn_test.go`
  - `TestBufConnReadWrite`: proves data is **buffered until Flush** (reader
    blocked pre-Flush, unblocks post-Flush) and round-trips correctly.
  - `TestBufConnCustomSizes`: `WithBufRead(128)`/`WithBufWrite(256)` round-trip
    of 1024 bytes (larger than buffers) — confirms options wire through and
    oversized payloads still flow.
  - **Gaps:** no test for `Close` joined-error behaviour; no test for the `buf`
    driver param parsing (neither the invalid-value nor unknown-param error
    paths).
- `frame_conn_test.go`
  - `TestFrameConnSimple`: single frame round-trip.
  - `TestFrameConnPartialRead`: 1024-byte frame read first as 100 bytes then the
    remainder via the `pending` path.
  - `TestFrameConnDeliversEmptyFrames`: two empty frames each `n=0,err=nil`, then
    payload — locks in empty-frame delivery semantics.
  - `writeFrame` helper independently encodes the **2-byte** header,
    corroborating the wire format.
  - **Gaps:** no test for the Flush-coalescing branch with a real `BufConn`
    underneath; no test for >65535 truncation; `frame` driver param rejection
    untested.
- `split_conn_test.go`
  - `maxWriteConn` test double exposes `MaxWrite()` and records each underlying
    write.
  - `TestSplitConn_SplitsLargeWrite`: limit 8, 25 bytes -> exactly 4 writes, each
    `<= 8`; verifies returned `n == len(data)`.
  - `TestSplitConn_SmallWritePassthrough`: single write when payload < limit.
  - `TestSplitConn_NoMaxWriteError`: plain `net.Conn` -> error and `nil` conn.
  - **Gaps:** no test for `MaxWrite()==0` returning an error (only the no-method
    case is tested); no partial-write-on-error test; no `split` driver param
    test.
- **Shared gap across all three:** no concurrency tests (see **Concurrency
  model** for the per-layer safety differences).

---

## Related docs

- [pipeline.md](pipeline.md) — how driver wrappers are composed into a chain.
- [mux.md](mux.md) / [demux.md](demux.md) — layers that *consume* the same
  `interface{ MaxWrite() uint16 }` contract.
- [poll-tagged.md](poll-tagged.md) — poll conns forward `MaxWrite`.
- [drivers-proto.md](drivers-proto.md) — `dnst`/`aesgcm` providers of `MaxWrite`.
