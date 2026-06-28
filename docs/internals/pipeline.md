# Pipeline Core (Driver / Wrapper / Scheme / URI)

Internals reference for the typed "wrapper pipeline" that turns Scheme/URI strings
(e.g. `tcp+tls{cert=..}+frame://addr`) into a `net.Listener` (server) or a dialed
`net.Conn` (client). Citations are by **symbol + file** against the root module
(`github.com/pedramktb/go-netx`); exact line numbers are deliberately avoided
because they rot on every refactor. To re-anchor a symbol:
`grep -n 'func (w Wrapper)' wrap.go` or `go doc github.com/pedramktb/go-netx Wrapper`.

## Purpose & role

The pipeline core parses a URI into a base `Transport` plus an ordered list of
`Wrapper`s, validates that the wrappers compose into a type-correct chain, and then
applies them at runtime to upgrade a raw transport into the requested
listener/dialer. Each `Wrapper` is produced by a named `Driver` looked up in a
global registry. The same driver name must serve both the server (`listener=true`)
and client (`listener=false`) directions.

Data type flowing through the chain is one of four `PipeType`s: `net.Listener`,
`Dialer`, `net.Conn`, `TaggedConn` (the `PipeType` consts in `wrap.go`).

- `Dialer` is a type alias `func() (net.Conn, error)` (`Dialer` in `mux_client.go`).
- `TaggedConn` is a `net.Conn`-like interface (Close/addr/deadline methods) that
  replaces `Read`/`Write` with `ReadTagged`/`WriteTagged`; it does **not** embed
  `net.Conn` (`TaggedConn` in `tagged_conn.go`).

## Public API (each exported symbol + file with a one-line contract)

Registry (`driver.go`):
- `Driver func(params map[string]string, listener bool) (Wrapper, error)` — factory that builds a `Wrapper` for one direction from parsed params.
- `Register(name string, d Driver)` — installs a driver; panics on nil or duplicate name (see Concurrency / Error semantics).
- `GetDriver(name string) (Driver, error)` — looks up a driver; returns `uri: unknown driver %q` if absent.

Wrapper layer (`wrap.go`):
- `PipeType` + `PipeTypeListener|Dialer|Conn|TaggedConn` — the four pipeline value types; iota order matters only for `PipeType.String()`.
- `Wrapper` struct — one pipeline transform; holds `Name`, `Params`, `Listener` flag, a `SecretParams []SecretParam` field (see Secret masking), and **14** typed function fields (one per `inputType -> outputType` transition: 3 Listener-, 3 Dialer-, 4 Conn-, 4 Tagged-).
- `Wrapper.InputTypes() []PipeType` — which input `PipeType`s this wrapper accepts (derived from which function fields are non-nil).
- `Wrapper.OutputFor(input PipeType) (PipeType, bool)` — output type for a given input, or `(0,false)` if unsupported. Drives chain validation.
- `Wrapper.Apply(v any) (any, error)` — runtime: type-switches on the concrete value and calls the matching function field; `incompatible type %T` if none.
- `Wrapper.String()` / `MarshalText()` — render `name` or `name{k=v,...}` via the shared `Wrapper.render` helper; emits **raw** param values (NOT secret-masked).
- `Wrapper.Redacted()` — like `String()` but masks `SecretParams` values; the only log-safe renderer (see Secret masking).
- `Wrapper.UnmarshalText(text []byte, listener bool) error` — parse one layer string, look up + invoke the driver, replace `*w` with the driver's `Wrapper`.
- `SecretParam struct { Name, Fingerprint string }` — declares one sensitive param key (see Secret masking).
- `Wrappers []Wrapper` — ordered chain.
- `Wrappers.Apply(conn any) (any, error)` — fold `Apply` over the chain; wraps errors as `wrap %q: %w`.
- `Wrappers.String()` / `MarshalText()` — join layers with `+` (raw values).
- `Wrappers.Redacted()` — join via each `Wrapper.Redacted()` (log-safe).
- `Wrappers.UnmarshalText(text []byte, server bool) error` — split on `+`, parse each layer, then validate the whole chain (see Internal design).
- `ServerWrappers` / `ClientWrappers` — thin wrappers binding `server=true`/`false` for `encoding.TextUnmarshaler`.
- `ListenerWrapper` / `DialerWrapper` — single-layer variants binding `listener=true`/`false`.
- `ConnWrapListener(ln, wrapConn) (net.Listener, error)` — adapts a `func(net.Conn)(net.Conn,error)` into a Listener that wraps each `Accept`ed conn.
- `ConnWrapDialer(dial, wrapConn) (Dialer, error)` — adapts the same conn-wrapper into a Dialer that wraps each dialed conn.

Scheme layer (`scheme.go`):
- `Scheme struct { Transport; Wrappers }` — base transport plus optional wrapper chain.
- `Scheme.String()` / `MarshalText()` — render `transport[+wrappers]` (raw values).
- `Scheme.Redacted()` — render `transport[+wrappers]` via `Wrappers.Redacted()` (log-safe).
- `Scheme.UnmarshalText(text, listener bool) error` — split on first `+` into transport vs wrappers, parse both.
- `ListenerScheme.Listen(ctx, addr, opts...) (net.Listener, error)` — base `Listen`, then `Wrappers.Apply`, assert result is `net.Listener`.
- `DialerScheme.Dial(ctx, addr, opts...) (net.Conn, error)` — build a base dialer, `Wrappers.Apply`, assert result is a `Dialer`, then call it.
- `ListenerScheme`/`DialerScheme.UnmarshalText` — bind server/client direction.

URI layer (`uri.go`):
- `URI struct { Scheme; Addr string }` — scheme plus address.
- `URI.String()` / `MarshalText()` — render `scheme://addr` (raw values).
- `URI.Redacted()` — render `scheme://addr` via `Scheme.Redacted()` (log-safe).
- `URI.UnmarshalText(text, server bool) error` — split on `://`, trim addr, parse scheme.
- `ListenerURI.Listen(ctx, opts...) (net.Listener, error)` — delegate to `ListenerScheme.Listen`.
- `DialerURI.Dial(ctx, opts...) (net.Conn, error)` — delegate to `DialerScheme.Dial`.
- `ListenerURI`/`DialerURI.UnmarshalText` — bind server/client direction.

Transport layer (`transport.go`):
- `TransportICMP|TCP|UDP` consts (`"icmp"/"tcp"/"udp"`).
- `Transport string`; `Transport.String()` returns `""` for unknown values.
- `Transport.UnmarshalText(text, listener bool) error` — accepts only the three known names; `uri: unknown transport %q` otherwise. Note: `listener` param is accepted but unused.
- `ListenerTransport.Listen` / `DialerTransport.Dial` — convenience direct base-transport access.

Dial dispatch (`dial.go`):
- `Listen(ctx, network, addr, opts...) (net.Listener, error)` — UDP via pion packet listener, ICMP via `icmpListenConfig`, else `net.ListenConfig`.
- `Dial(ctx, network, addr, opts...) (net.Conn, error)` — ICMP dials via `net.Dialer` then wraps with `NewICMPClientConn`; else plain `net.Dialer.DialContext`.
- Options: `ListenOption`/`WithListenConfig`/`WithPacketListenConfig`, `DialOption`/`WithDialConfig` (all in `dial.go`).

## Internal design & invariants

### PipeType transition table

`Wrapper.OutputFor` (`wrap.go`) is the single source of truth for transitions. A
wrapper that has a given function field non-nil maps that input type to the listed
output type. For one input type, fields are checked in the order below and the
first non-nil wins.

| Input `PipeType` | Function field | Output `PipeType` |
|---|---|---|
| Listener | `ListenerToListener` | Listener |
| Listener | `ListenerToConn` | Conn |
| Listener | `ListenerToTagged` | TaggedConn |
| Dialer | `DialerToDialer` | Dialer |
| Dialer | `DialerToConn` | Conn |
| Dialer | `DialerToTagged` | TaggedConn |
| Conn | `ConnToConn` | Conn |
| Conn | `ConnToTagged` | TaggedConn |
| Conn | `ConnToListener` | Listener |
| Conn | `ConnToDialer` | Dialer |
| TaggedConn | `TaggedToTagged` | TaggedConn |
| TaggedConn | `TaggedToConn` | Conn |
| TaggedConn | `TaggedToListener` | Listener |
| TaggedConn | `TaggedToDialer` | Dialer |

A single `Wrapper` may set fields for *multiple* input types (one per type). E.g.
the `tls` server driver sets both `ListenerToListener` and `ConnToConn`
(`init` in `drivers/tls/tls.go`); the `demux` server driver sets `ConnToListener`,
`TaggedToListener`, and `ListenerToListener` (`init` in `demux.go`). The struct
doc comment "Maximum one function field per input type" (above the `Wrapper`
struct in `wrap.go`) means at most one field *per* `PipeType`, not one overall.
Setting two fields for the same input type (e.g. both `ConnToConn` and
`ConnToTagged`) is a latent bug: `OutputFor` and `Apply` silently pick the first
by field order; nothing enforces it.

### How OutputFor drives chain validation

`Wrappers.UnmarshalText` (`wrap.go`) validates the chain *at parse time*:

1. Each `+`-separated part is unmarshalled into a `Wrapper` via the driver
   (`Wrapper.UnmarshalText`).
2. `currentType` seeds at `PipeTypeDialer` for clients, `PipeTypeListener` for
   servers.
3. For each wrapper in order, `OutputFor(currentType)` must return `ok`; otherwise
   error `wrapper %q at position %d: incompatible input type %s, expected one of %v`.
   On success `currentType` advances to the output type.
4. Final-type invariant: server chains must end in `PipeTypeListener`; client chains
   must end in `PipeTypeDialer`. Otherwise
   `invalid wrapper chain: final output type %s is not a Listener/Dialer ...`.

So a server chain seeds `Listener` and must thread back to `Listener` by the end; a
client chain seeds `Dialer` and must end at `Dialer`.

Note the entry path: `Scheme.UnmarshalText` (`scheme.go`) splits on the first `+`
via `SplitN(text, "+", 2)` and **returns early when there is no `+`** (a
transport-only scheme), so `Wrappers.UnmarshalText` is never reached for such a
scheme — the empty-chain edge case only matters when a `+` is present. An empty
part on either side of a `+` falls through to a driver lookup that fails with
`unknown driver`.

### The listener/server bool duality

A single boolean (`listener` at the driver level, `server` at the chain level — same
value) is threaded through *every* unmarshal call and into the driver:

`URI.UnmarshalText(text, server)` →
`Scheme.UnmarshalText(text, listener)` →
`Wrappers.UnmarshalText(text, server)` →
`Wrapper.UnmarshalText(text, listener)` →
`driver(w.Params, listener)` (all in `uri.go`/`scheme.go`/`wrap.go`).

The same registered driver name produces a *different* `Wrapper` shape depending on
the flag. Example: the `tls` driver's `listener` branch returns Listener/Conn
fields; its client branch returns Dialer/Conn fields (`init` in `drivers/tls/tls.go`).
The `*Server*`/`*Client*`, `Listener*`/`Dialer*`, and `*URI`/`*Scheme`/`*Transport`
exported wrapper types exist only to bind this bool for `encoding.TextUnmarshaler`
(which has no extra arg).

### MarshalText / String round-trip

`String()` at every level reconstructs the canonical form: `URI = scheme://addr`,
`Scheme = transport[+wrappers]`, `Wrappers = layer+layer`, `Wrapper = name{k=v,...}`.
Round-trip is *lossy/non-deterministic*: `Wrapper.render` (`wrap.go`) iterates
`Wrapper.Params` (a `map`), so param order is random, and the name/keys are
lowercased+trimmed on parse (`Wrapper.UnmarshalText`) — the emitted string is the
normalized form, not the original input. A zero `Scheme`/`URI` marshals to
`://addr` because `Transport.String()` returns `""` for an unset/unknown transport.
See Hazard 7 for the operational consequence.

### Secret masking (Redacted vs String)

A redaction layer sits parallel to `String()`/`MarshalText()`:

- `Wrapper.SecretParams []SecretParam` (field on the `Wrapper` struct) declares which
  param keys hold sensitive values (private keys, passphrases). Each `SecretParam`
  is `{Name, Fingerprint}` (`wrap.go`).
- `Wrapper.Redacted()` renders via `Wrapper.render(secrets)`: a declared key renders
  as `key=REDACTED` (empty `Fingerprint`) or `key=REDACTED(<fingerprint>)`.
  `Wrappers.Redacted()`, `Scheme.Redacted()`, and `URI.Redacted()` compose this up
  the stack.
- **`Fingerprint` is the payload inside `REDACTED(...)`.** Drivers compute it once at
  parse time in their protocol's standard form (e.g. `sha256=AB:CD:...` for TLS,
  `SHA256:...` for SSH) so operators can cross-check against
  openssl/ssh-keygen/browser output. For **low-entropy** secrets (passwords) leave
  `Fingerprint` empty — a fingerprint there would be brute-forceable. See the
  `SecretParam.Fingerprint` doc comment in `wrap.go`.

**INVARIANT (log safety):** `String()` and `MarshalText()` emit **raw** secret
values — they are the round-trippable form and **must be kept out of any
user-visible stream** (logs, error strings surfaced to embedders). `Redacted()` is
the only log-safe renderer. `Redacted()` output is **NOT** round-trippable through
`UnmarshalText` (the secrets are gone). Example: the `tls` server driver populates
`SecretParams` with `{Name: "key", Fingerprint: "sha256=" + ...}` so the private
key is masked but the paired-cert fingerprint stays cross-checkable
(`init` in `drivers/tls/tls.go`).

## Concurrency model

The registry is guarded by `driversMu sync.RWMutex` (`driver.go`): `Register` takes
the write lock, `GetDriver` (called by every `Wrapper.UnmarshalText`) takes the read
lock, so reads are concurrency-safe. Drivers register in `init()` and are visible
only if their package is imported (blank import or transitive). Concurrent runtime
`Register` is locked but still panics on dup/nil (below).

## Lifecycle & ownership (who closes wrapped conns on Apply error)

- `connWrappedListener.Accept` (`wrap.go`): on a successful `Accept` it calls
  `wrapConn`; if that fails it `c.Close()`s the just-accepted conn and returns the
  error. The wrapped listener owns conns only across the wrap step.
- `ConnWrapDialer` (`wrap.go`): the returned `Dialer` dials, then wraps; if wrapping
  fails it `c.Close()`s the freshly dialed conn. Otherwise ownership passes to the
  caller.
- `Wrappers.Apply` (`wrap.go`) does **not** close intermediate values on error. If
  wrapper N succeeds (e.g. produced a Listener/Conn holding resources) and wrapper
  N+1 fails, the successful intermediate is dropped without `Close()`. This is a
  resource-leak hazard for wrappers whose `Apply` opens fds/goroutines eagerly. The
  conn-level adapters above only protect the single conn produced inside one adapter,
  not the chain.
- `ListenerScheme.Listen` (`scheme.go`): if the base `Listen` succeeds but
  `Wrappers.Apply` fails, the base listener is **not** closed. The dialer path
  (`DialerScheme.Dial`) is safer because its base "dial" is a deferred closure not
  yet invoked, so nothing is open on apply failure.

## Error semantics

Parse-time (during `UnmarshalText`, before any I/O):
- Unknown driver: `uri: unknown driver %q` (`GetDriver`), wrapped as `uri: %w` in `Wrapper.UnmarshalText`.
- Driver setup failure (bad/unknown params, missing required params): bubbles up as
  `uri: setup driver %s: %w` (`Wrapper.UnmarshalText`); driver-specific messages, e.g.
  `uri: tls server requires cert and key parameters` (`drivers/tls/tls.go`).
- Malformed layer (all in `Wrapper.UnmarshalText`): `uri: missing '}' in layer %q`,
  `uri: invalid parameter %q`, `uri: empty parameter key`.
- Malformed URI (`URI.UnmarshalText`): `uri: missing scheme delimiter in %q` (no `://`),
  `uri: empty address in %q`.
- Unknown transport: `uri: unknown transport %q` (`Transport.UnmarshalText`).
- Chain type mismatch / wrong final type (`Wrappers.UnmarshalText`).
- `Register` panics (not errors): nil driver `uri: Register driver is nil`; duplicate
  name `uri: Register called twice for driver <name>` (`driver.go`). These fire at
  `init()` time → program won't start.

Runtime (during `Listen`/`Dial`):
- `Wrappers.Apply` wraps each failure `wrap %q: %w`.
- `Wrapper.Apply` type mismatch: `wrapper %q: incompatible type %T` — should be
  unreachable if parse-time validation ran, because `Apply`'s type switch mirrors
  `OutputFor`. It *can* trigger if a `Wrapper` is built/applied without going through
  `Wrappers.UnmarshalText` validation.
- Scheme assertions: `wrapper(s) did not produce net.Listener` (`ListenerScheme.Listen`) /
  `... did not produce dial function` (`DialerScheme.Dial`) — defensive, also normally
  guaranteed by parse-time final-type validation.

## Dependencies

Depends on:
- `Dialer` alias (`mux_client.go`) and `TaggedConn` interface (`tagged_conn.go`).
- `dial.go` base `Listen`/`Dial`, which depend on pion `udp`, `icmpListenConfig`
  (`icmp_listener.go`) / `NewICMPClientConn` (`icmp_conn.go`).
- Std `net`, `strings`, `fmt`, `sync`, `context`, `errors`.

Depended on by:
- Built-in drivers registering in this package via `init()`: `frame` (`frame_conn.go`),
  `mux` (`mux.go`), `demux` (`demux.go`), `buf` (`buffered_conn.go`),
  `split` (`split_conn.go`), `poll` (`poll_conn.go`).
- External driver modules (`drivers/*`): `tls`, `tlspsk`, `dtls`, `dtlspsk`, `utls`,
  `aesgcm`, `dnst`, `ssh` — each `netx.Register(...)` in `init()`.
- `cli` consumes `ListenerURI`/`DialerURI` + `UnmarshalText` (`cli/internal/tun.go`).
- The server/tun layer (`server.go`, `tun.go`) consumes the produced
  `net.Listener`/`net.Conn`.

External: pion `transport/v3/udp` (`dial.go`); per-driver crypto/transport libs.

## Change hazards (READ FIRST)

1. **Adding/changing a `Wrapper` function field requires updating four places in
   lockstep** (all in `wrap.go`): the struct field, `Wrapper.InputTypes`,
   `Wrapper.OutputFor`, and `Wrapper.Apply`. `InputTypes`/`OutputFor` drive parse-time
   validation; `Apply` drives runtime. If they disagree, you get either false parse
   rejections or runtime `wrapper %q: incompatible type %T`. New `PipeType` values
   must also be added to `PipeType.String()`.

2. **A driver must behave correctly for *both* `listener=true` and `false`.** The
   flag is threaded everywhere (`URI.UnmarshalText` → `Scheme.UnmarshalText` →
   `Wrappers.UnmarshalText` → `Wrapper.UnmarshalText` → driver). Server chains must
   compose to end in `Listener`; client chains to end in `Dialer`
   (`Wrappers.UnmarshalText`). A driver that only handles one direction will
   pass-parse in that direction and fail validation in the other.

3. **Multiple fields set for the same input type are silently ambiguous.**
   `OutputFor`/`Apply` pick the first non-nil field by source order; there is no
   guard (contrary to the `Wrapper` struct doc comment). Set exactly one field per
   input type you support.

4. **`Register` panics on nil or duplicate name** (`driver.go`). Two drivers (or two
   `init`s) claiming the same name crash at startup. Names are matched *after*
   lowercasing/trimming the parsed input (`Wrapper.UnmarshalText`) but `Register`
   stores the literal name — register lowercase names only, or lookups via URI will
   never match.

5. **Param normalization asymmetry** (`Wrapper.UnmarshalText`). Keys are
   lowercased+trimmed; values are only trimmed. Hex/base64/case-sensitive values
   survive, but a driver expecting a specific key casing will never see it. Empty
   key → `uri: empty parameter key`; a value containing `=` is preserved
   (`SplitN(pair, "=", 2)`); a value containing `,` is split (params split on `,`).

6. **No `Close()` of intermediates on chain failure.** `Wrappers.Apply` and
   `ListenerScheme.Listen` leak any already-opened listener/conn if a later wrapper
   fails. If you add a wrapper whose `Apply` eagerly acquires resources, add your own
   cleanup; do not rely on the core.

7. **`String()`/round-trip is not byte-stable** (map-ordered params in
   `Wrapper.render`). Do not use the marshalled string as a cache key, dedup key, or
   in golden tests without sorting params first.

8. **`String()`/`MarshalText()` leak raw secrets.** They emit unmasked param values
   and are the round-trippable form. Anything that surfaces a `Wrapper`/`Wrappers`/
   `Scheme`/`URI` into a log or user-facing error MUST use `Redacted()` instead.
   `Redacted()` is not round-trippable. A new secret-bearing driver MUST populate
   `Wrapper.SecretParams` for every sensitive key (pattern: `drivers/tls/tls.go`), or
   that value leaks through `Redacted()` unmasked. Compute a `Fingerprint` at parse
   time for high-entropy secrets; leave it empty for low-entropy ones.

9. **Transport set is closed** (`Transport.String()`/`UnmarshalText` plus the `Listen`/
   `Dial` switches in `dial.go`). Adding a new base transport requires editing all
   three; `Transport`'s `listener` param is currently ignored.

> ⚠️ **Stale source comments — do not trust these comments, trust the code.**
> - `frame_conn.go`'s package doc and `NewFrameConn` doc say the frame header is
>   **4-byte**; the code actually writes/reads a **2-byte** (`uint16`) big-endian
>   header. The 4-byte claim is wrong.
> - `buffered_conn.go`'s `NewBufConn` doc references `WithBufWriterSize` /
>   `WithBufReaderSize`; the real options are `WithBufRead` / `WithBufWrite`.
>
> These comments are intentionally left unfixed in the source for now; this doc keeps
> the warning so they don't mislead.

## How to add a new driver safely (checklist grounded in code)

1. Create the package and call `netx.Register("<lowercasename>", func(params, listener) (netx.Wrapper, error){...})` in `init()` (pattern: `init` in `frame_conn.go`, `drivers/tls/tls.go`). Use a unique, lowercase name (hazard 4).
2. Validate `params`: iterate keys, reject unknown ones (e.g. `drivers/tls/tls.go`, `frame_conn.go`), return `netx.Wrapper{}, err` on bad input. Honor that values are trimmed but case-preserved.
3. Branch on `listener`: build the server-direction `Wrapper` and the client-direction `Wrapper` separately (`drivers/tls/tls.go`). Reject direction-only params in the wrong direction (e.g. demux's `accq`/`rq` are guarded by `if !listener` in `demux.go`).
4. Set the function fields that define your transitions. For a plain conn transform, set `ConnToConn` and reuse the adapters for the chain ends: `ListenerToListener: ConnWrapListener(l, connToConn)` and `DialerToDialer: ConnWrapDialer(f, connToConn)` (`frame_conn.go`). Set at most one field per input type (hazard 3).
5. Verify the resulting chain ends correctly: a Conn-producing wrapper alone makes a *server* chain end in `Conn`, which fails final-type validation unless a later wrapper (e.g. `mux`/`demux`) converts back to `Listener`. A client chain must reach `Dialer`.
6. Populate `Name`, `Params`, `Listener` on the returned `Wrapper` so `String()`/round-trip works (`drivers/tls/tls.go`).
7. For any sensitive param, populate `SecretParams` so `Redacted()` masks it (hazard 8; pattern: `drivers/tls/tls.go`).
8. If the driver lives in a separate module, ensure consumers blank-import it so its `init()` runs; otherwise `GetDriver` returns `unknown driver`.
9. Handle ownership: if your `Apply` opens resources eagerly, close them on later failure yourself (hazard 6); for per-conn wraps, the `ConnWrap*` adapters already close on wrap failure (`connWrappedListener.Accept`, `ConnWrapDialer`).
10. Test both directions through a full URI parse + `Listen`/`Dial`, not just the bare driver, to exercise `Wrappers.UnmarshalText` validation.

## Related docs

See [`docs/internals/README.md`](README.md) for the canonical subsystem index.
Most tightly coupled: [`mux.md`](mux.md), [`demux.md`](demux.md) (Listener↔Conn↔TaggedConn
transitions) and [`poll-tagged.md`](poll-tagged.md) (`poll` + `TaggedConn` semantics).
