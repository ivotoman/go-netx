# Stream Transforms (Buffered / Framed / Split)

> Internals reference for the three byte-level `net.Conn` transforms in the root
> `netx` package: `bufConn` (buffered I/O + explicit `Flush`), `frameConn`
> (length-prefixed framing), and `splitConn` (write chunking to honour an
> underlying `MaxWrite` limit). Goal: let future changes avoid breaking wire
> compatibility, capability detection, or composition.
>
> Every claim below is grounded in source as `file:line`. Where the code
> contradicts its own doc comments or the names commonly assumed for it, that is
> flagged explicitly under **Naming / doc drift** — do not trust the comments
> over the code.

---

## Purpose & role

These three wrappers are pure byte/stream transforms layered onto an existing
`net.Conn`. They sit low in a connection pipeline and are composed by the driver
registry (see [drivers-proto.md](drivers-proto.md), [pipeline.md](pipeline.md)).

- **`bufConn`** — wraps a `net.Conn` with a `bufio.Reader` and `bufio.Writer` to
  reduce syscalls for small I/O. Adds an explicit `Flush() error`. Source header
  comment `buffered_conn.go:1-8`.
- **`frameConn`** — adds a length-prefixed framing protocol so packet/message
  boundaries survive a stream transport (e.g. UDP semantics over TCP+TLS). One
  `Write` == one frame; each `Read` returns at most one frame's bytes. Source
  header comment `frame_conn.go:1-10`.
- **`splitConn`** — removes a `MaxWrite` limitation of the conn beneath it by
  splitting large `Write` calls into `<= MaxWrite`-byte chunks. Source header
  comment `split_conn.go:1-6`.

Typical stacking order (innermost transport first): a transport that imposes a
`MaxWrite` (e.g. `dnst` client conn) -> `splitConn` -> ... -> `bufConn` ->
`frameConn`. See **Composition & dependencies**.

---

## Naming / doc drift (read first)

The actual exported symbols differ from several commonly-assumed names. Use the
real ones:

| Assumed name           | Actual symbol (file:line)                          |
|------------------------|----------------------------------------------------|
| `NewFramedConn`        | `NewFrameConn` (`frame_conn.go:53`)                |
| `FramedConn` type      | unexported `frameConn` (`frame_conn.go:44`); constructor returns plain `net.Conn` |
| `WithBufSize` / `WithBufReaderSize` / `WithBufWriterSize` | `WithBufRead` (`buffered_conn.go:77`), `WithBufWrite` (`buffered_conn.go:71`) |
| `WithMaxFrameSize` option | **does not exist**; `frame` driver rejects all params (`frame_conn.go:24-26`) |
| `maxsize` frame param  | **does not exist**; see above                      |
| `ErrFrameTooLarge`     | **does not exist** in this code (verified absent)  |

Two stale doc comments contradict the code — **do not rely on them**:

- `buffered_conn.go:84` says "By default, the buffer size is 4KB. Use
  WithBufWriterSize and WithBufReaderSize…". The 4KB figure is whatever
  `bufio.NewReader`/`bufio.NewWriter` default to (`buffered_conn.go:88-89`), and
  the named options do not exist.
- `frame_conn.go:4-5` (and `:52`) claim a **"4-byte big-endian length header"**.
  The code uses a **2-byte** (`uint16`) header: `var hdr [2]byte` at
  `frame_conn.go:71` and `:94`, `binary.BigEndian.Uint16` at `:75`,
  `binary.BigEndian.PutUint16` at `:95`. This is a real wire-format hazard — see
  **Change hazards**.

---

## Public API (exported symbols + options + driver params)

### BufConn

- `type BufConn interface { net.Conn; Flush() error }` — `buffered_conn.go:58-61`.
  This interface is also the **Flush-capability interface** the framing layer
  detects (see below).
- `func NewBufConn(c net.Conn, opts ...BufConnOption) BufConn` —
  `buffered_conn.go:85-95`. Defaults: `br = bufio.NewReader(c)`,
  `bw = bufio.NewWriter(c)` (`:88-89`), i.e. `bufio` package defaults
  (4096 bytes each at time of writing).
- `type BufConnOption func(*bufConn)` — `buffered_conn.go:69`.
- `func WithBufWrite(size uint16) BufConnOption` — `buffered_conn.go:71-75`.
  Replaces the writer with `bufio.NewWriterSize(bc.Conn, int(size))`.
- `func WithBufRead(size uint16) BufConnOption` — `buffered_conn.go:77-81`.
  Replaces the reader with `bufio.NewReaderSize(bc.Conn, int(size))`.
  - **Contract note:** `size` is `uint16`, so max configurable buffer is 65535.
    `bufio.NewReaderSize`/`NewWriterSize` clamp a size below the minimum
    (16 bytes) up to that minimum — a `WithBufRead(0)`/`WithBufWrite(0)` does not
    create a zero-length buffer, it gets the bufio minimum.

**`buf` driver** (`buffered_conn.go:20-56`): registered name `"buf"`. Params:
  - `r` -> `WithBufRead(uint16)` (`:25-30`); value parsed `ParseUint(…,10,16)`.
  - `w` -> `WithBufWrite(uint16)` (`:31-36`).
  - any other key -> error `buf: unknown buffered parameter %q` (`:37-38`).
  - Provides `ListenerToListener`, `DialerToDialer`, `ConnToConn` wrappers
    (`:44-54`).

### FrameConn

- `func NewFrameConn(c net.Conn) net.Conn` — `frame_conn.go:53-58`. Returns
  `*frameConn` (unexported) as a plain `net.Conn`. Allocates a read scratch
  buffer `buf = make([]byte, MaxPacketSize)` (`:56`, `MaxPacketSize = 65535`,
  `packet.go:3-4`).
- No options. The `frame` driver (`frame_conn.go:22-42`, name `"frame"`) accepts
  **zero** params: the first param key triggers
  `uri: unknown frame parameter %q` (`:24-25`). Provides the same three wrapper
  hooks (`:30-40`).

### SplitConn

- `func NewSplitConn(c net.Conn) (net.Conn, error)` — `split_conn.go:46-55`.
  Detects the MaxWrite-capability via type assertion (see below). Returns a
  `*splitConn` (unexported) or an error.
- No options. The `split` driver (`split_conn.go:16-35`, name `"split"`) accepts
  **zero** params (`:18-19` errors `split: unknown parameter %q`). Provides the
  three wrapper hooks (`:24-34`).

### Driver registration mechanics (shared)

Each file calls `Register(name, Driver)` in its `init()` (`buffered_conn.go:21`,
`frame_conn.go:23`, `split_conn.go:17`). `Register` (`driver.go:15-25`) **panics
on duplicate name** (`:21-23`) and on `nil` driver (`:18-19`). `Driver` is
`func(params map[string]string, listener bool) (Wrapper, error)`
(`driver.go:8`). `Wrapper` shape: `wrap.go:119-141`. The conn-level closures use
`ConnWrapListener`/`ConnWrapDialer` (`wrap.go:327`, `:332`); `Dialer` is
`func() (net.Conn, error)` (`mux_client.go:28`).

---

## Wire formats & capability interfaces

### Frame header layout (the wire format)

```
+--------+--------+============================+
| len hi | len lo |  payload (len bytes)       |
+--------+--------+============================+
   byte0   byte1     0..65535 bytes
```

- **2 bytes, big-endian, `uint16`.** Write side: `binary.BigEndian.PutUint16(hdr[:], uint16(len(p)))`
  at `frame_conn.go:94-95`; read side: `binary.BigEndian.Uint16(hdr[:])` at
  `:71-75`.
- Header and payload are written as **two separate `Conn.Write` calls**
  (`frame_conn.go:96` then `:102`). This is why buffering+Flush coalescing
  exists (next section).
- **Max payload is implicitly 65535**, because `uint16(len(p))` silently
  truncates anything larger (`:95`). There is no explicit length check and no
  `ErrFrameTooLarge`. A `Write` of > 65535 bytes writes a wrong (truncated)
  length header and the full payload, corrupting the stream. See **Change
  hazards**. The companion `MaxPacketSize = 65535` constant (`packet.go:3-4`) is
  the intended ceiling but is **not enforced** in `frameConn.Write`.

### The Flush-capability interface

Defined as `BufConn` (`buffered_conn.go:58-61`):

```go
type BufConn interface {
	net.Conn
	Flush() error
}
```

- **Detected by** `frameConn.Write`: `if fw, ok := c.Conn.(BufConn); ok { fw.Flush() }`
  (`frame_conn.go:106-110`). After writing header+payload, if the underlying
  conn satisfies `BufConn` the frame layer flushes immediately, coalescing the
  two writes into one syscall.
- **Implemented by** `bufConn` only: `func (c *bufConn) Flush() error` at
  `buffered_conn.go:116`. No other conn in the repo defines `Flush() error`
  (verified by search). In particular `frameConn` and `splitConn` do **not**
  expose `Flush`.

### The MaxWrite-capability interface

There is **no named interface**; every consumer uses an inline anonymous
interface type:

```go
interface{ MaxWrite() uint16 }
```

- **Detected by** `splitConn`: `mw, ok := c.(interface{ MaxWrite() uint16 })`
  (`split_conn.go:47`). Requires `ok && mw.MaxWrite() != 0` (`:48`).
- **Same shape is consumed across the codebase** (this is a cross-subsystem
  contract — see **Composition**): `demux.go:147`, `demux_tagged.go:44`,
  `demux_client.go:29`, `proto/aesgcm/aesgcm_conn.go:64`, `poll_conn.go:243` and
  `:464`.
- **Providers of `MaxWrite() uint16`:**
  - `proto/dnst` client conn (`dnst_conn.go:335`) — computed from domain length
    in `NewClientConn` (`:318-332`) via `maxQNAMEPayload` (`:395`); this is the
    canonical upstream source feeding `splitConn`. Server variants at
    `dnst_conn.go:90` and `:221`.
  - `proto/aesgcm` conn (`aesgcm_conn.go:108`) — forwards a reduced limit.
  - demux sessions (`demux.go:256`, `demux_tagged.go:156`,
    `demux_client.go:39`) — forward `underlying - idLen`.
  - poll conns forward the inner limit (`poll_conn.go:242-245`, `:463-466`).

---

## Internal design & invariants

### bufConn

- Struct: embeds `net.Conn`, plus `br *bufio.Reader`, `bw *bufio.Writer`
  (`buffered_conn.go:63-67`).
- `Read` -> `br.Read` (`:97`); `Write` -> `bw.Write` (`:98`). Buffered until
  `Flush`/`Close`.
- Invariant relied on by `frameConn`: a `bufConn` write is not visible to the
  peer until `Flush` (asserted by `TestBufConnReadWrite`,
  `buffered_conn_test.go:35-50`).

### frameConn

- Struct: embeds `net.Conn`, plus `pending []byte`, `buf []byte`, and two
  mutexes `rmu, wmu` (`frame_conn.go:44-49`).
- **Read state machine** (`frame_conn.go:61-87`):
  1. If `pending` (leftover from a previous over-large frame) is non-empty,
     copy from it and shrink it (`:65-69`).
  2. Else read the 2-byte header with `io.ReadFull` (`:71-74`), decode `n`
     (`:75`).
  3. If caller's `p` can hold the whole frame (`len(p) >= n`), read the payload
     straight into `p` (`:76-79`).
  4. Otherwise read the full payload into the scratch `buf`, copy what fits into
     `p`, and stash the rest in `pending` (`:81-86`). Subsequent `Read`s drain
     `pending` (step 1). This is how a single large frame is delivered across
     multiple `Read` calls (asserted by `TestFrameConnPartialRead`,
     `frame_conn_test.go:61-102`).
- **Write** (`frame_conn.go:90-112`): write header (`:94-98`); if payload empty
  return `0, nil` *after* the header write (`:99-101`) — i.e. an empty frame is
  a valid 2-byte-only wire unit; else write payload (`:102`); then opportunistic
  Flush (`:106-110`); return `len(p)` (`:111`).
- **Invariant — scratch buffer size:** `buf` is `MaxPacketSize` (65535) bytes
  (`:56`). The slice expression `c.buf[:n]` (`:81`, `:84`) assumes `n <= 65535`,
  which always holds because `n` came from a `uint16` (`:75`). If the header
  width ever changes to allow larger `n`, `:81` would panic.

### splitConn

- Struct: embeds `net.Conn`, plus `maxWrite int` (`split_conn.go:38-41`),
  captured once at construction (`:53`).
- `Write` loop (`:59-73`): take `chunk = b[:maxWrite]` (or all of `b` if
  smaller), write it via `sc.Conn.Write`, accumulate `total`, advance `b` by the
  bytes actually written `n` (`:71`), stop on error returning partial `total`
  (`:67-70`). No `Read` override — reads pass through the embedded `net.Conn`.
- **Invariant:** `maxWrite > 0` (guaranteed by the constructor check,
  `:48`), so the loop always makes progress.

---

## Concurrency model

- **bufConn:** *no internal locks.* It relies on `bufio.Reader`/`bufio.Writer`,
  which are **not** safe for concurrent use within the same direction. Concurrent
  `Read`+`Write` on a `bufConn` is safe only insofar as reader and writer are
  independent objects (they are: `br` vs `bw`). Concurrent `Write`+`Write` or
  `Read`+`Read` is **not** safe. `Close` calls `bw.Flush()` (`:104`)
  concurrently with an in-flight `Write` is a data race.
- **frameConn:** has `rmu` (read) and `wmu` (write) mutexes
  (`frame_conn.go:48`). `Read` holds `rmu` (`:62-63`); `Write` holds `wmu`
  (`:91-92`). So concurrent **Read+Write is safe**, and concurrent Read+Read /
  Write+Write are individually serialized. The two locks are independent — there
  is no cross-lock ordering, so no deadlock between them.
- **splitConn:** *no locks.* A single `Write` issues multiple underlying writes
  while holding nothing; two concurrent `splitConn.Write`s can **interleave their
  chunks** on the wire and corrupt framing. Safe concurrent use requires the
  caller (or a layer above) to serialize writes. Read passes through unguarded.

---

## Lifecycle & ownership

- **bufConn.Close** (`buffered_conn.go:99-114`): flush-then-close with a
  **joined error**. It attempts `bw.Flush()`; if it errors, joins it via
  `errors.Join` (`:103-107`) but **still** attempts `Conn.Close()` and joins
  that error too (`:108-112`). Returns the combined error (`:113`). Nil-guards on
  `bw` and `Conn`. Net effect: buffered-but-unflushed data is flushed on Close,
  and a flush failure does not skip the close.
- **frameConn:** no `Close` override -> the embedded `net.Conn.Close` is used.
  It does **not** flush an underlying `bufConn` on close (it only flushes
  per-Write at `:106-110`). If the very last `Write` succeeded, its data was
  already flushed; there is no separate close-time flush.
- **splitConn:** no `Close` override -> embedded `net.Conn.Close`. Ownership of
  the wrapped conn is transferred to the wrapper in all three cases (closing the
  wrapper closes the chain down to the innermost conn).

---

## Error semantics

- **Empty frames:** `frameConn.Write(nil)`/`Write([]byte{})` writes a
  2-byte zero-length header and returns `(0, nil)` (`frame_conn.go:99-101`). On
  the read side an empty frame yields `(0, nil)` — i.e. it is **delivered**, not
  swallowed. Asserted by `TestFrameConnDeliversEmptyFrames`
  (`frame_conn_test.go:104-144`): two empty frames each read as `n=0, err=nil`
  before the real payload. **Hazard:** code reading from a `frameConn` must treat
  `n==0, err==nil` as "received an empty packet", not as EOF/spin — a naive
  `io.ReadFull` loop will not advance on empty frames.
- **Oversized frame:** there is **no `ErrFrameTooLarge`** and no length check.
  Payloads > 65535 are silently truncated in the header (`uint16` conversion at
  `frame_conn.go:95`) while the full payload is still written -> stream
  corruption. (Contrast: the demux layer *does* enforce `MaxPacketSize`, e.g.
  `demux.go:328`, `demux_tagged.go:259` — framing does not.)
- **split: MaxWrite missing/zero:** `NewSplitConn` returns
  `errors.New("split: underlying connection does not implement MaxWrite or has no MaxWrite limit")`
  and a `nil` conn when the assertion fails or `MaxWrite()==0`
  (`split_conn.go:47-49`). Asserted by `TestSplitConn_NoMaxWriteError`
  (`split_conn_test.go:120-132`), which also checks the returned conn is `nil`.
- **split: partial write:** `splitConn.Write` returns the partial `total` plus
  the underlying error on any chunk failure (`split_conn.go:67-70`), preserving
  `io.Writer` semantics.

---

## Composition & dependencies

### Capability matrix — consumes vs exposes

| Wrapper    | Consumes (detects)        | Exposes to layers above it                                   |
|------------|---------------------------|-------------------------------------------------------------|
| `bufConn`  | nothing                   | `Flush() error` (`buffered_conn.go:116`) — the `BufConn` capability. Does **not** expose `MaxWrite`. |
| `frameConn`| `BufConn` (Flush) on its underlying conn (`frame_conn.go:106`) | **neither** `Flush` nor `MaxWrite` — only methods of the `net.Conn` interface it embeds. |
| `splitConn`| `MaxWrite() uint16` on its underlying conn (`split_conn.go:47`) | **does not** re-expose `MaxWrite` (it *satisfies* the limit, so layers above see effectively unlimited writes). |

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

`frameConn` does header-write + payload-write as two calls
(`frame_conn.go:96`, `:102`). If the underlying conn is a `bufConn`, both land
in `bw` and the trailing `Flush` (`:106-110`) emits them as one syscall. This is
the documented reason to stack `bufConn` beneath `frameConn`
(`frame_conn.go:6-9`, `buffered_conn.go:4-7`). Recommended order:
`frameConn(bufConn(rawConn))`.

### The dnst -> split MaxWrite chain

`dnst` client conn computes a `MaxWrite` from the configured domain
(`proto/dnst/dnst_conn.go:318-335`, `maxQNAMEPayload` at `:395`). `splitConn`
reads that value once (`split_conn.go:47-53`) and chunks every write to fit a
single DNS QNAME. The `dnst` driver's `maxw` listener param
(`drivers/dnst/dnst.go:21-29`) tunes the server-side `MaxWrite`. So the chain is:
`dnst conn.MaxWrite()` -> `splitConn` chunking. Breaking either the interface
shape or the dnst computation silently changes (or disables) chunking.

### Depended-on-by

- Anything detecting Flush (only `frameConn`, `frame_conn.go:106`).
- Anything detecting `MaxWrite` — `splitConn` plus demux/aesgcm/poll layers
  listed under **The MaxWrite-capability interface**. These all hardcode the
  inline `interface{ MaxWrite() uint16 }` shape; it is a de-facto repo-wide
  contract even though it is not a named type.

---

## Change hazards (MOST IMPORTANT)

1. **Frame header width / endianness is the wire format.** Changing the 2-byte
   `uint16` header (`frame_conn.go:71`, `:75`, `:94`, `:95`) — to 4 bytes (as the
   stale comments at `frame_conn.go:4-5,52` wrongly describe), or to
   little-endian — breaks compatibility with every already-deployed peer. Both
   read and write sides must change atomically, and it is an on-wire breaking
   change for any mixed-version deployment. If you "fix" the comment by changing
   the code to 4 bytes, you break the protocol; the safe fix is to correct the
   comment to say 2 bytes.
2. **No max-frame enforcement -> silent truncation.** `uint16(len(p))` at
   `frame_conn.go:95` truncates payloads > 65535 with no error, while still
   writing the full body -> corrupted stream and no `ErrFrameTooLarge`. If you
   add enforcement, decide between returning an error vs auto-splitting; either
   changes caller-visible behaviour. The read-side scratch slice `c.buf[:n]`
   (`:81`) also hard-assumes `n <= 65535`.
3. **Removing/altering Flush propagation reintroduces header/payload
   fragmentation.** If the `BufConn` assertion at `frame_conn.go:106` is removed,
   renamed, or the `BufConn` interface (`buffered_conn.go:58-61`) changes shape,
   `frameConn` reverts to two syscalls per frame and (worse) data may sit
   unflushed in the buffer until Close, stalling the peer (the buffering-blocks
   behaviour is exactly what `TestBufConnReadWrite` proves,
   `buffered_conn_test.go:35-39`).
4. **`splitConn` silently no-ops / mis-detects if the MaxWrite contract
   breaks.** The detection is an *inline* `interface{ MaxWrite() uint16 }`
   (`split_conn.go:47`) duplicated in 6+ places. Changing the method name,
   signature (e.g. to `int` or to return an error), or a provider failing to
   expose it makes `NewSplitConn` error out at construction (`:48-49`) — or, if a
   provider returns 0, also error. Worse, a layer inserted *between* the
   `MaxWrite` provider and `splitConn` that doesn't forward `MaxWrite` (e.g.
   `frameConn`) silently breaks the chain. Consider introducing a single named
   interface to make this contract greppable.
5. **Concurrent-write safety differs per layer.** `frameConn` is safe for
   concurrent Read+Write (`rmu`/`wmu`, `frame_conn.go:48,62,91`) but `bufConn`
   and `splitConn` have **no write lock**. Two goroutines writing the same
   `splitConn` can interleave chunks and corrupt frames; two writing a `bufConn`
   race on `bufio.Writer`. Adding parallel writers above these layers without
   external serialization is a latent corruption bug. Do not assume the framing
   layer's safety extends downward.
6. **Default buffer sizes affect throughput and the per-call max.** `NewBufConn`
   uses `bufio` defaults (4096) (`buffered_conn.go:88-89`); options cap at
   `uint16` 65535 (`:71`, `:77`). Shrinking defaults raises syscall count;
   the stale "4KB" comment (`:84`) will mislead anyone tuning this. Note also a
   tiny `WithBufWrite(size)` smaller than a frame does not break framing
   (bufio splits), but undersizing relative to typical frame size defeats the
   coalescing benefit.
7. **`splitConn` captures `maxWrite` once at construction** (`split_conn.go:53`).
   If an underlying transport's `MaxWrite` ever became dynamic, `splitConn` would
   not observe the change. Today all providers return a fixed value, so this is
   latent, not active.

---

## Tests covering this

- `buffered_conn_test.go`
  - `TestBufConnReadWrite` (`:13-54`): proves data is **buffered until Flush**
    (reader blocked pre-Flush `:35-39`, unblocks post-Flush `:43-50`) and
    round-trips correctly.
  - `TestBufConnCustomSizes` (`:56-73`): `WithBufRead(128)`/`WithBufWrite(256)`
    round-trip of 1024 bytes (larger than buffers) — confirms options wire
    through and oversized payloads still flow.
  - **Gaps:** no test for `Close` joined-error behaviour (`buffered_conn.go:99-114`);
    no test for the `buf` driver param parsing/errors (`:25-39`); no concurrency
    test.
- `frame_conn_test.go`
  - `TestFrameConnSimple` (`:29-59`): single frame round-trip.
  - `TestFrameConnPartialRead` (`:61-102`): 1024-byte frame read first as 100
    bytes (`r.n == 100`, `:86`) then the remainder via the `pending` path.
  - `TestFrameConnDeliversEmptyFrames` (`:104-144`): two empty frames each
    `n=0,err=nil`, then payload — locks in empty-frame delivery semantics.
  - `writeFrame` helper (`:15-27`) independently encodes the **2-byte** header,
    corroborating the wire format.
  - **Gaps:** no test for the Flush-coalescing branch (`frame_conn.go:106-110`)
    with a real `BufConn` underneath; no test for >65535 truncation; no
    concurrent Read+Write test exercising `rmu`/`wmu`; `frame` driver param
    rejection untested.
- `split_conn_test.go`
  - `maxWriteConn` test double (`:15-39`) exposes `MaxWrite()` and records each
    underlying write.
  - `TestSplitConn_SplitsLargeWrite` (`:41-86`): limit 8, 25 bytes -> exactly 4
    writes, each `<= 8` (`:77-85`); verifies returned `n == len(data)`.
  - `TestSplitConn_SmallWritePassthrough` (`:88-118`): single write when payload
    < limit.
  - `TestSplitConn_NoMaxWriteError` (`:120-132`): plain `net.Conn` -> error and
    `nil` conn.
  - **Gaps:** no test for `MaxWrite()==0` returning an error (only the
    no-method case is tested); no partial-write-on-error test; no concurrency
    test; no `split` driver param test.

---

## Related docs

- [pipeline.md](pipeline.md) — how driver wrappers are composed into a chain.
- [mux.md](mux.md) / [demux.md](demux.md) — layers that *consume* the same
  `interface{ MaxWrite() uint16 }` contract.
- [poll-tagged.md](poll-tagged.md) — poll conns forward `MaxWrite`.
- [drivers-proto.md](drivers-proto.md) — `dnst`/`aesgcm` providers of `MaxWrite`.

> Note: at the time of writing, the only file present under `docs/` is
> `docs/internals/mux-tag-poll.md`; the cross-links above are forward references
> to docs that may not yet exist.
