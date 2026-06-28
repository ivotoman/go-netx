# Pipeline Core (Driver / Wrapper / Scheme / URI)

Internals reference for the typed "wrapper pipeline" that turns Scheme/URI strings
(e.g. `tcp+tls{cert=..}+frame://addr`) into a `net.Listener` (server) or a dialed
`net.Conn` (client). All citations are `file:line` against the root module
(`github.com/pedramktb/go-netx`, `go.mod:1`).

## Purpose & role

The pipeline core parses a URI into a base `Transport` plus an ordered list of
`Wrapper`s, validates that the wrappers compose into a type-correct chain, and then
applies them at runtime to upgrade a raw transport into the requested
listener/dialer. Each `Wrapper` is produced by a named `Driver` looked up in a
global registry. The same driver name must serve both the server (`listener=true`)
and client (`listener=false`) directions.

Data type flowing through the chain is one of four `PipeType`s: `net.Listener`,
`Dialer`, `net.Conn`, `TaggedConn` (`wrap.go:24-29`).

- `Dialer` is a type alias `func() (net.Conn, error)` (`mux_client.go:28`).
- `TaggedConn` is a `net.Conn`-like interface adding `ReadTagged`/`WriteTagged`
  (`tagged_conn.go:11-38`).

## Public API (each exported symbol with file:line and one-line contract)

Registry (`driver.go`):
- `Driver func(params map[string]string, listener bool) (Wrapper, error)` — `driver.go:8` — factory that builds a `Wrapper` for one direction from parsed params.
- `Register(name string, d Driver)` — `driver.go:15` — installs a driver; panics on nil or duplicate name (see Concurrency / Error semantics).
- `GetDriver(name string) (Driver, error)` — `driver.go:27` — looks up a driver; returns `uri: unknown driver %q` if absent.

Wrapper layer (`wrap.go`):
- `PipeType` + `PipeTypeListener|Dialer|Conn|TaggedConn` — `wrap.go:22-29` — the four pipeline value types; iota order matters only for `String()` (`wrap.go:31-44`).
- `Wrapper` struct — `wrap.go:119-141` — one pipeline transform; holds `Name`, `Params`, `Listener` flag, and up to 15 typed function fields (4 input types × output types).
- `Wrapper.InputTypes() []PipeType` — `wrap.go:143` — which input `PipeType`s this wrapper accepts (derived from which function fields are non-nil).
- `Wrapper.OutputFor(input PipeType) (PipeType, bool)` — `wrap.go:162` — output type for a given input, or `(0,false)` if unsupported. Drives chain validation.
- `Wrapper.Apply(v any) (any, error)` — `wrap.go:211` — runtime: type-switches on the concrete value and calls the matching function field; `incompatible type %T` if none.
- `Wrapper.String()` / `MarshalText()` — `wrap.go:257`, `wrap.go:268` — render `name` or `name{k=v,...}`.
- `Wrapper.UnmarshalText(text []byte, listener bool) error` — `wrap.go:272` — parse one layer string, look up + invoke the driver, replace `*w` with the driver's `Wrapper`.
- `Wrappers []Wrapper` — `wrap.go:46` — ordered chain.
- `Wrappers.Apply(conn any) (any, error)` — `wrap.go:48` — fold `Apply` over the chain; wraps errors as `wrap %q: %w`.
- `Wrappers.String()` / `MarshalText()` — `wrap.go:59`, `wrap.go:67` — join layers with `+`.
- `Wrappers.UnmarshalText(text []byte, server bool) error` — `wrap.go:71` — split on `+`, parse each layer, then validate the whole chain (see Internal design).
- `ServerWrappers` / `ClientWrappers` — `wrap.go:9-19` — thin wrappers binding `server=true`/`false` for `encoding.TextUnmarshaler`.
- `ListenerWrapper` / `DialerWrapper` — `wrap.go:104-114` — single-layer variants binding `listener=true`/`false`.
- `ConnWrapListener(ln, wrapConn) (net.Listener, error)` — `wrap.go:327` — adapts a `func(net.Conn)(net.Conn,error)` into a Listener that wraps each `Accept`ed conn.
- `ConnWrapDialer(dial, wrapConn) (Dialer, error)` — `wrap.go:332` — adapts the same conn-wrapper into a Dialer that wraps each dialed conn.

Scheme layer (`scheme.go`):
- `Scheme struct { Transport; Wrappers }` — `scheme.go:52` — base transport plus optional wrapper chain.
- `Scheme.String()` / `MarshalText()` — `scheme.go:57`, `scheme.go:65` — render `transport[+wrappers]`.
- `Scheme.UnmarshalText(text, listener bool) error` — `scheme.go:69` — split on first `+` into transport vs wrappers, parse both.
- `ListenerScheme.Listen(ctx, addr, opts...) (net.Listener, error)` — `scheme.go:13` — base `Listen`, then `Wrappers.Apply`, assert result is `net.Listener`.
- `DialerScheme.Dial(ctx, addr, opts...) (net.Conn, error)` — `scheme.go:34` — build a base dialer, `Wrappers.Apply`, assert result is a `Dialer`, then call it.
- `ListenerScheme`/`DialerScheme.UnmarshalText` — `scheme.go:28`, `scheme.go:48` — bind server/client direction.

URI layer (`uri.go`):
- `URI struct { Scheme; Addr string }` — `uri.go:30` — scheme plus address.
- `URI.String()` / `MarshalText()` — `uri.go:35`, `uri.go:39` — render `scheme://addr`.
- `URI.UnmarshalText(text, server bool) error` — `uri.go:43` — split on `://`, trim addr, parse scheme.
- `ListenerURI.Listen(ctx, opts...) (net.Listener, error)` — `uri.go:12` — delegate to `ListenerScheme.Listen`.
- `DialerURI.Dial(ctx, opts...) (net.Conn, error)` — `uri.go:22` — delegate to `DialerScheme.Dial`.
- `ListenerURI`/`DialerURI.UnmarshalText` — `uri.go:16`, `uri.go:26` — bind server/client direction.

Transport layer (`transport.go`):
- `TransportICMP|TCP|UDP` consts (`"icmp"/"tcp"/"udp"`) — `transport.go:9-13`.
- `Transport string` — `transport.go:15`; `String()` returns `""` for unknown values (`transport.go:37-44`).
- `Transport.UnmarshalText(text, listener bool) error` — `transport.go:50` — accepts only the three known names; `uri: unknown transport %q` otherwise. Note: `listener` param is accepted but unused.
- `ListenerTransport.Listen` / `DialerTransport.Dial` — `transport.go:19`, `transport.go:29` — convenience direct base-transport access.

Dial dispatch (`dial.go`):
- `Listen(ctx, network, addr, opts...) (net.Listener, error)` — `dial.go:29` — UDP via pion packet listener, ICMP via `icmpListenConfig`, else `net.ListenConfig`.
- `Dial(ctx, network, addr, opts...) (net.Conn, error)` — `dial.go:73` — ICMP via `NewICMPClientConn`, else `net.Dialer.DialContext`.
- Options: `ListenOption`/`WithListenConfig`/`WithPacketListenConfig` (`dial.go:15-27`), `DialOption`/`WithDialConfig` (`dial.go:65-71`).

## Internal design & invariants

### PipeType transition table

`OutputFor` (`wrap.go:162-206`) is the single source of truth for transitions. A
wrapper that has a given function field non-nil maps that input type to the listed
output type. For one input type, fields are checked in the order below and the
first non-nil wins.

| Input `PipeType` | Function field | Output `PipeType` | Defined |
|---|---|---|---|
| Listener | `ListenerToListener` | Listener | wrap.go:166 |
| Listener | `ListenerToConn` | Conn | wrap.go:168 |
| Listener | `ListenerToTagged` | TaggedConn | wrap.go:170 |
| Dialer | `DialerToDialer` | Dialer | wrap.go:175 |
| Dialer | `DialerToConn` | Conn | wrap.go:177 |
| Dialer | `DialerToTagged` | TaggedConn | wrap.go:179 |
| Conn | `ConnToConn` | Conn | wrap.go:184 |
| Conn | `ConnToTagged` | TaggedConn | wrap.go:186 |
| Conn | `ConnToListener` | Listener | wrap.go:188 |
| Conn | `ConnToDialer` | Dialer | wrap.go:190 |
| TaggedConn | `TaggedToTagged` | TaggedConn | wrap.go:195 |
| TaggedConn | `TaggedToConn` | Conn | wrap.go:197 |
| TaggedConn | `TaggedToListener` | Listener | wrap.go:199 |
| TaggedConn | `TaggedToDialer` | Dialer | wrap.go:201 |

A single `Wrapper` may set fields for *multiple* input types (one per type). E.g.
`tls` (server) sets both `ListenerToListener` and `ConnToConn` (`tls/tls.go:56-61`);
`demux` (server) sets `ConnToListener`, `TaggedToListener`, and `ListenerToListener`
(`demux.go:66-78`). The doc comment "Maximum one function field per input type"
(`wrap.go:117`) means at most one field *per* `PipeType`, not one overall. Setting
two fields for the same input type (e.g. both `ConnToConn` and `ConnToTagged`) is a
latent bug: `OutputFor` and `Apply` silently pick the first by field order; nothing
enforces it.

### How OutputFor drives chain validation

`Wrappers.UnmarshalText` (`wrap.go:71-102`) validates the chain *at parse time*:

1. Each `+`-separated part is unmarshalled into a `Wrapper` via the driver
   (`wrap.go:74-78`).
2. `currentType` starts at `PipeTypeDialer` for clients, `PipeTypeListener` for
   servers (`wrap.go:81-84`).
3. For each wrapper in order, `OutputFor(currentType)` must return `ok`; otherwise
   error `wrapper %q at position %d: incompatible input type %s, expected one of %v`
   (`wrap.go:86-90`). On success `currentType` advances to the output type.
4. Final-type invariant: server chains must end in `PipeTypeListener`; client chains
   must end in `PipeTypeDialer` (`wrap.go:93-99`). Otherwise
   `invalid wrapper chain: final output type %s is not a Listener/Dialer ...`.

So a server chain seeds `Listener` and must thread back to `Listener` by the end; a
client chain seeds `Dialer` and must end at `Dialer`. (Empty wrapper chains never
reach this code — see Error semantics: `strings.Split` on `""` yields one empty
part whose driver lookup fails first.)

### The listener/server bool duality

A single boolean (`listener` at the driver level, `server` at the chain level — same
value) is threaded through *every* unmarshal call and into the driver:

`URI.UnmarshalText(text, server)` (`uri.go:43`) →
`Scheme.UnmarshalText(text, listener)` (`scheme.go:69`) →
`Wrappers.UnmarshalText(text, server)` (`wrap.go:71`) →
`Wrapper.UnmarshalText(text, listener)` (`wrap.go:272`) →
`driver(params, listener)` (`wrap.go:300`).

The same registered driver name produces a *different* `Wrapper` shape depending on
the flag. Example: `tls` server returns Listener/Conn fields; `tls` client returns
Dialer/Conn fields (`tls/tls.go:43-89`). The `*Server*`/`*Client*`, `Listener*`/
`Dialer*`, and `*URI`/`*Scheme`/`*Transport` exported wrapper types exist only to
bind this bool for `encoding.TextUnmarshaler` (which has no extra arg).

### MarshalText / String round-trip

`String()` at every level reconstructs the canonical form:
`URI = scheme://addr` (`uri.go:35`), `Scheme = transport[+wrappers]`
(`scheme.go:57`), `Wrappers = layer+layer` (`wrap.go:59`),
`Wrapper = name{k=v,...}` (`wrap.go:257`). Round-trip is *lossy/non-deterministic*:

- `Wrapper.Params` is a `map`, so `Wrapper.String()` emits params in random order
  (`wrap.go:258-264`). Re-parsing yields the same wrapper but `String()` output is
  not byte-stable.
- The name is lowercased and trimmed on parse (`wrap.go:275`, `wrap.go:281`) and
  param keys are lowercased/trimmed, values only trimmed (`wrap.go:287-288`), so the
  emitted string is the normalized form, not necessarily the original input.
- `Transport.String()` returns `""` for an unset/unknown transport (`transport.go:42`),
  so a zero `Scheme`/`URI` marshals to `://addr`.

## Concurrency model

- The registry is guarded by `driversMu sync.RWMutex` (`driver.go:11`). `Register`
  takes the write lock (`driver.go:16`); `GetDriver` takes the read lock
  (`driver.go:28`). Reads (every `Wrapper.UnmarshalText`) are concurrency-safe.
- Registration timing: every built-in and driver registers in an `init()`
  (e.g. `frame_conn.go:22`, `mux.go:31`, `tls/tls.go:16`). Drivers in other modules
  register only if their package is imported (blank import or transitive). The CLI
  imports them; library users must import the driver packages they intend to use.
- After init, the map is effectively read-only in normal use; concurrent
  `Register` at runtime is locked but dup/nil still panics (below).

## Lifecycle & ownership (who closes wrapped conns on Apply error)

- `connWrappedListener.Accept` (`wrap.go:313-324`): on a successful `Accept` it
  calls `wrapConn`; if that fails it `c.Close()`s the just-accepted conn and returns
  the error. The wrapped listener owns conns only across the wrap step.
- `ConnWrapDialer` (`wrap.go:332-345`): the returned `Dialer` dials, then wraps; if
  wrapping fails it `c.Close()`s the freshly dialed conn. Otherwise ownership passes
  to the caller.
- `Wrappers.Apply` (`wrap.go:48-57`) does **not** close intermediate values on
  error. If wrapper N succeeds (e.g. produced a Listener/Conn holding resources) and
  wrapper N+1 fails, the successful intermediate is dropped without `Close()`. This
  is a resource-leak hazard for wrappers whose `Apply` opens fds/goroutines
  eagerly. The conn-level adapters above only protect the single conn produced
  inside one adapter, not the chain.
- `ListenerScheme.Listen` (`scheme.go:13-26`): if the base `Listen` succeeds but
  `Wrappers.Apply` fails, the base listener `l` is **not** closed. Same pattern for
  the dialer path, though there the base "dial" is a deferred closure not yet
  invoked, so nothing is open on parse/apply failure (`scheme.go:34-46`).

## Error semantics

Parse-time (during `UnmarshalText`, before any I/O):
- Unknown driver: `uri: unknown driver %q` (`driver.go:32`, wrapped at `wrap.go:298`).
- Driver setup failure (bad/unknown params, missing required params): bubbles up as
  `uri: setup driver %s: %w` (`wrap.go:300-303`); driver-specific messages, e.g.
  `uri: tls server requires cert and key parameters` (`tls/tls.go:45`).
- Malformed layer: missing `}` (`wrap.go:278`), invalid `k=v` pair (`wrap.go:285`),
  empty key (`wrap.go:290`).
- Malformed URI: missing `://` (`uri.go:47`), empty addr (`uri.go:52`).
- Unknown transport: `uri: unknown transport %q` (`transport.go:56`).
- Chain type mismatch / wrong final type (`wrap.go:88`, `wrap.go:94`, `wrap.go:98`).
- `Register` panics (not errors): nil driver `uri: Register driver is nil`
  (`driver.go:19`); duplicate name `uri: Register called twice for driver <name>`
  (`driver.go:22`). These fire at `init()` time → program won't start.

Runtime (during `Listen`/`Dial`):
- `Wrappers.Apply` wraps each failure `wrap %q: %w` (`wrap.go:53`).
- `Wrapper.Apply` type mismatch: `wrapper %q: incompatible type %T` (`wrap.go:254`)
  — should be unreachable if parse-time validation ran, because `Apply`'s type
  switch mirrors `OutputFor`. It *can* trigger if a `Wrapper` is built/applied
  without going through `Wrappers.UnmarshalText` validation.
- Scheme assertions: `wrapper(s) did not produce net.Listener` (`scheme.go:25`) /
  `... did not produce dial function` (`scheme.go:45`) — defensive, also normally
  guaranteed by parse-time final-type validation.

## Dependencies

Depends on:
- `Dialer` alias (`mux_client.go:28`) and `TaggedConn` interface (`tagged_conn.go:11`).
- `dial.go` base `Listen`/`Dial`, which depend on pion `udp` (`dial.go:7`),
  `icmpListenConfig`/`NewICMPClientConn` (`icmp_listener.go`, `icmp_conn.go`).
- Std `net`, `strings`, `fmt`, `sync`, `context`, `errors`.

Depended on by:
- Built-in drivers registering in this package: `frame` (`frame_conn.go:22`),
  `mux` (`mux.go:31`), `demux` (`demux.go:25`), `buf` (`buffered_conn.go:21`),
  `split` (`split_conn.go:17`), `poll` (`poll_conn.go:36`).
- External driver modules (`drivers/*`): `tls`, `tlspsk`, `dtls`, `dtlspsk`, `utls`,
  `aesgcm`, `dnst`, `ssh` — each `netx.Register(...)` in `init()`.
- `cli` consumes `ListenerURI`/`DialerURI` + `UnmarshalText` (`cli/internal/tun.go:58-63`).
- The server/tun layer (`server.go`, `tun.go`) consumes the produced
  `net.Listener`/`net.Conn`.

External: pion `transport/v3/udp` (`dial.go:7`); per-driver crypto/transport libs.

## Change hazards (READ FIRST)

1. **Adding/changing a `Wrapper` function field requires updating four places in
   lockstep**: the struct field (`wrap.go:124-140`), `InputTypes` (`wrap.go:143`),
   `OutputFor` (`wrap.go:162`), and `Apply` (`wrap.go:211`). `InputTypes`/`OutputFor`
   drive parse-time validation; `Apply` drives runtime. If they disagree, you get
   either false parse rejections or runtime `incompatible type %T` (`wrap.go:254`).
   New `PipeType` values must also be added to `PipeType.String()` (`wrap.go:31`).

2. **A driver must behave correctly for *both* `listener=true` and `false`.** The
   flag is threaded everywhere (`uri.go:43`→`scheme.go:69`→`wrap.go:71`→
   `wrap.go:272`→driver). Server chains must compose to end in `Listener`; client
   chains to end in `Dialer` (`wrap.go:93-99`). A driver that only handles one
   direction will pass-parse in that direction and fail validation in the other.

3. **Multiple fields set for the same input type are silently ambiguous.**
   `OutputFor`/`Apply` pick the first non-nil field by source order; there is no
   guard (contrary to the comment at `wrap.go:117`). Set exactly one field per input
   type you support.

4. **`Register` panics on nil or duplicate name (`driver.go:18-23`).** Two drivers
   (or two `init`s) claiming the same name crash at startup. Names are matched
   *after* lowercasing/trimming the parsed input (`wrap.go:275/281`) but `Register`
   stores the literal name — register lowercase names only, or lookups via URI will
   never match.

5. **Param normalization asymmetry.** Keys are lowercased+trimmed; values are only
   trimmed (`wrap.go:287-288`). Hex/base64/case-sensitive values survive, but a
   driver expecting a specific key casing will never see it. Empty key → error
   (`wrap.go:290`); a value containing `=` is preserved (`SplitN(...,2)`,
   `wrap.go:283`); a value containing `,` is not (params split on `,`,
   `wrap.go:282`).

6. **No `Close()` of intermediates on chain failure.** `Wrappers.Apply`
   (`wrap.go:48`) and `ListenerScheme.Listen` (`scheme.go:18`) leak any
   already-opened listener/conn if a later wrapper fails. If you add a wrapper whose
   `Apply` eagerly acquires resources, add your own cleanup; do not rely on the core.

7. **`String()`/round-trip is not byte-stable** (map-ordered params, `wrap.go:258`).
   Do not use the marshalled string as a cache key, dedup key, or in golden tests
   without sorting params first.

8. **Transport set is closed** (`transport.go:39`, `dial.go` switch). Adding a new
   base transport requires editing both `Transport.String()`/`UnmarshalText`
   (`transport.go`) and the `Listen`/`Dial` switches (`dial.go`); `Transport`'s
   `listener` param is currently ignored (`transport.go:50`).

## How to add a new driver safely (checklist grounded in code)

1. Create the package and call `netx.Register("<lowercasename>", func(params, listener) (netx.Wrapper, error){...})` in `init()` (pattern: `frame_conn.go:22`, `tls/tls.go:16`). Use a unique, lowercase name (hazards 4).
2. Validate `params`: iterate keys, reject unknown ones (`tls/tls.go:23-41`, `frame_conn.go:24-26`), return `netx.Wrapper{}, err` on bad input. Honor that values are trimmed but case-preserved.
3. Branch on `listener`: build the server-direction `Wrapper` and the client-direction `Wrapper` separately (`tls/tls.go:43-89`). Direction-only params should be rejected in the wrong direction (e.g. demux `accq`/`rq`, `demux.go:42-58`).
4. Set the function fields that define your transitions. For a plain conn transform, set `ConnToConn` and reuse the adapters for the chain ends: `ListenerToListener: ConnWrapListener(l, connToConn)` and `DialerToDialer: ConnWrapDialer(f, connToConn)` (`frame_conn.go:33-39`). Set at most one field per input type (hazard 3).
5. Verify the resulting chain ends correctly: a Conn-producing wrapper alone makes a *server* chain end in `Conn`, which fails final-type validation (`wrap.go:94`) unless a later wrapper (e.g. `mux`/`demux`) converts back to `Listener`. For a client chain you must reach `Dialer`.
6. Populate `Name`, `Params`, `Listener` on the returned `Wrapper` so `String()`/round-trip works (`tls/tls.go:53-55`).
7. If the driver lives in a separate module, ensure consumers blank-import it so its `init()` runs; otherwise `GetDriver` returns `unknown driver` (`driver.go:32`).
8. Handle ownership: if your `Apply` opens resources eagerly, close them on later failure yourself (hazard 6); for per-conn wraps, the `ConnWrap*` adapters already close on wrap failure (`wrap.go:319-321`, `wrap.go:340-342`).
9. Test both directions through a full URI parse + `Listen`/`Dial`, not just the bare driver, to exercise `Wrappers.UnmarshalText` validation.

## Related docs

- `pipeline.md` (this doc)
- `mux.md`, `demux.md` — `mux`/`demux` drivers (Listener↔Conn↔TaggedConn transitions)
- `poll-tagged.md` — `poll` driver + `TaggedConn` semantics
- `stream-transforms.md` — `frame`/`buf`/`split`/`aesgcm` conn transforms
- `server-tun.md` — consumers of the produced listener/conn (`server.go`, `tun.go`)
- `icmp.md` — ICMP transport in `dial.go`/`icmp_*`
- `drivers-proto.md` — external `drivers/*` and `proto/*` modules
- `modules-cli.md` — `go.work` layout and the `cli` consumer
