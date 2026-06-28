# go-netx — Backlog & Improvements

> _Generated 2026-06-24 from a multi-agent code review (reviewers across each subsystem plus security / concurrency / maintainability lenses), with every finding adversarially verified against source. The Q1 bugs `1.1` (double-close panic) and `1.3` (buffer aliasing) were additionally re-confirmed by hand. Living document — prune items as they land._

This document is a code-grounded, prioritized backlog for the **go-netx** repository. Every item below was **adversarially verified against the source** — the abstract claim was reproduced or traced to concrete symbols, and findings whose stated impact, mechanism, or reachability did not hold up were either downgraded or excluded. Items are grouped in an Eisenhower-style **impact × urgency** matrix: **Q1 Critical** (high impact + high urgency — active bugs, security/correctness defects, silent corruption/leaks; *fix now*); **Q2 Strategic** (high impact + lower urgency — architecture, API consistency, test coverage; *plan/schedule*); **Q3 Quick wins** (lower impact, low effort, worth doing soon — cheap fixes, stale help/docs, misleading names); **Q4 Backlog** (low impact + low urgency — cosmetics, minor consistency). Code is cited by **symbol + file** per repo convention (not `file:line`). Items whose verification verdict was *partial* (claim correct but originally overstated, here re-scoped to the verified kernel) are marked **[partial]**. Effort is **S/M/L**.

## Summary

| Quadrant | Theme | Count |
|---|---|---|
| **Q1** | Critical — bugs / security / corruption / leaks | 9 |
| **Q2** | Strategic — architecture / API / test coverage | 13 |
| **Q3** | Quick wins — cheap fixes, stale help/docs, names | 21 |
| **Q4** | Backlog — cosmetics / minor consistency | 21 |
| **Total** | | **64** |

---

## Q1 — Critical (fix now)

High impact and/or active correctness, security, or resource defects. Ordered by impact then urgency.

> **Status (2026-06-24, branch `ivo-20260613-speedup-integration`):** 1.1, 1.2, 1.3, 1.4, 1.5, and 1.9 are **fixed** on this branch with regression tests (see `CHANGELOG.md` → [Unreleased]). **1.6 is deferred** (see its entry). 1.7 and 1.8 (ICMP-NAT keying, DNST/EDNS0 — both wire/protocol changes) remain open.

### 1.1 `demuxSess.Close` double-closes `rQueue` → panic
- **Location**: `demuxSess.Close` / `demux.Close` (`demux.go`)
- **Why it matters**: `demux.Close` closes every session's `rQueue` and sets `sessions=nil`, but `demuxSess.Close` calls `close(s.rQueue)` **unconditionally** before its `if s.demux.sessions != nil` guard. Closing the parent (listener) and then an accepted session — an ordinary teardown order — panics with `close of closed channel`, killing the process. The tagged variant (`taggedDemuxSess.Close`, `demux_tagged.go`) already nests the close inside the guard, proving the asymmetry.
- **Fix**: Move `close(s.rQueue)` inside the `if s.demux.sessions != nil { ... }` block, mirroring the tagged variant. Add a regression test: `Accept` → `listener.Close()` → `sess.Close()` asserting no panic.
- **Effort**: S

### 1.2 SPKI pin uses `sha256.New().Sum(spki)` — does not hash the SPKI
- **Location**: `spkiVerifier` (`drivers/tls/tls.go`, `drivers/utls/utls.go`, `drivers/dtls/dtls.go`)
- **Why it matters**: `hash.Sum(b)` **appends** the digest-of-bytes-written-so-far (here, of nothing) to its argument, so the "pin" is `RawSubjectPublicKeyInfo || sha256("")` — raw public-key bytes plus a constant 32-byte suffix, **not** a hash. The central crypto control of cert-pinning does not do what its name/comments/CLI help claim. *Not currently exploitable* (both peers use the identical broken expression, so `bytes.Equal` reduces to raw-SPKI equality — no who-is-accepted weakening), but it stores/compares cleartext key material instead of a fixed digest and is brittle to any refactor assuming 32 bytes. Copy-pasted verbatim across three drivers.
- **Fix**: Replace with `sha256.Sum256(...)`; store/compare the `[32]byte` (via `bytes.Equal` or `subtle.ConstantTimeCompare`). Factor into one shared helper so the fix lands once. Add a test asserting pin length 32 and that a non-matching cert is rejected (ideally tied to known `openssl -fingerprint -sha256` output). Coordinated wire-format change — both peer sides ship from this repo.
- **Effort**: S

### 1.3 Buffer aliasing in `demuxSess.Write` / `taggedDemuxSess.Write`
- **Location**: `demuxSess.Write` (`demux.go`), `taggedDemuxSess.Write` (`demux_tagged.go`)
- **Why it matters**: `s.id` is `data[:idMask]` from the per-read buffer, so it has `len=idMask` but `cap=n`. `payload := append(s.id, b...)` then writes `b` **in place** into `s.id`'s backing array (whose tail still holds the first packet's queued payload). This corrupts queued data and is a genuine unsynchronized data race across concurrent `Write`s on one session (the `append` is not under any lock). `demux_client.go` was explicitly fixed for this exact hazard (`make`+`copy`, "Use a fresh buffer to avoid mutating m.id's underlying array…") but `demux.go`/`demux_tagged.go` were not.
- **Fix**: Build the frame with `make`+`copy` like `demux_client.go`, or force allocation via a 3-index slice `append(s.id[:len(s.id):len(s.id)], b...)`. Apply to both methods. Add a `-race` test issuing concurrent `Write`s before draining the read.
- **Effort**: S

### 1.4 `Server.route` can insert a conn after `Close`/`Shutdown` — leaked conn + relay goroutine
- **Location**: `Server.route` / `Server.Close` / `Server.Shutdown` (`server.go`)
- **Why it matters**: `route()` inserts into `s.conns` with no `s.closing` check, and route goroutines are **not** tracked by `listenerGroup` (only `Serve`'s accept loop is). So `Close` can finish its force-close loop and return, then a route goroutine for a just-accepted conn inserts into `s.conns` afterward — that conn (and, for `TunMaster`, its relay goroutine) is tracked but never closed, leaking for the process lifetime. Same window in `Shutdown`'s final force-close.
- **Fix**: Under `s.mu` in `route()`, check `s.closing.Load()` after acquiring the lock; if closing, close the conn (and signal the handler) instead of inserting. (Safe because `Close`/`Shutdown` set `closing` via CAS before taking `s.mu`.) Alternatively track in-flight route goroutines in a `WaitGroup` that `Close`/`Shutdown` wait on.
- **Effort**: S

### 1.5 `frameConn.Write` silently truncates the 16-bit length header for payloads > 65535
- **Location**: `frameConn.Write` (`frame_conn.go`)
- **Why it matters**: The header is `PutUint16(hdr[:], uint16(len(p)))`. For any payload > 65535 the value wraps mod 65536 while the full payload is still written and `Write` returns `(len(p), nil)` with no error; the receiver reads the wrong length and permanently desyncs the stream. Sibling layers (`taggedDemuxSess.Write`, `aesgcmConn.Write`) correctly error on oversize. Reachable via the exported `NewFrameConn` and the public `Tun.BufferSize uint` with a stream peer delivering ≥65536 bytes/read, or any direct large `frame.Write`. **[partial]** — the shipped CLI default (~32 KB `io.CopyBuffer`) and the UDP integration test (max datagram 65507 < 65535) do not trigger it; primary exposure is embedders. 
- **Fix**: Guard at the top of `Write`: `if len(p) > MaxPacketSize { return 0, ErrFrameTooLarge }` (define `ErrFrameTooLarge`), matching `demux_tagged.go`/`aesgcm`. Consider also exposing `MaxWrite() uint16` so an outer `split` can chunk (note: it does not today because `frameConn` lacks `MaxWrite`). Boundary tests at 65535 (ok), 65536/70000 (error).
- **Effort**: S

### 1.6 Concurrent FFI `Netx` calls clobber each other's log destination
> **DEFERRED (2026-06-24):** known library-API limitation — concurrent `Netx()` calls with distinct ids share a process-global `slog` sink. Not reachable for the current single-tunnel-per-process consumer (Ironlink). The correct fix threads a per-call logger through the root/proto **public** API (a multi-module change) and was judged out of scope for this round; a minimal cli-only fix is unsafe because it only relocates the bug — the core `demux`/`mux`/`dnst` constructors capture `slog.Default()` at build time.
- **Location**: `PersistentPreRunE` (`cli/internal/root.go`), `cli/internal/tun.go`, `cli/internal/eager.go`
- **Why it matters**: The FFI promises per-id isolation (each `Netx` passes its own out/err callbacks, `activeCalls` keyed by id), but all logging routes through the **process-global** `slog` default: `root.go` calls `slog.SetDefault(...)`, `TunMaster.Logger` is never set (falls back to `slog.Default()`), and emitters use package-level `slog.*`. Two concurrent `Netx(id1)`/`Netx(id2)`: the later `SetDefault` wins and **both** tunnels' logs — including the `netx tun started` signal embedders latch onto — go to the last-registered callback. **[partial]** — it is logical log misrouting, not a `-race`-detectable memory race (`slog` default is atomic-guarded).
- **Fix**: Build a per-call `*slog.Logger` from `cfg.out` in `Run` (not via `SetDefault`), set `tm.Logger`, and thread it through `runTun`/`newDeferredConn`. Replace package-level `slog.*` in `tun.go`/`eager.go`/`tun_darwin.go` with logger methods. Drop `slog.SetDefault` from the library path.
- **Effort**: M

### 1.7 Two ICMP clients behind one NAT collapse into a single server conn
- **Location**: `icmpListener.getConn` (`icmp_listener.go`), `NewICMPClientConn`/`icmpConn.Write` (`icmp_conn.go`)
- **Why it matters**: The listener keys per-peer conns purely by source IP (`l.conns[raddr.String()]`), never by the ICMP Echo identifier, and the client identifier is hard-set to `1` and never mutated — every client emits `id=1`. Two clients sharing a public source IP (NAT, or two processes on one host) map to **one** `icmpListenerConn`; their streams interleave and corrupt. The `id` field exists to disambiguate but is unused for keying. **[partial]** — severe (silent corruption) but conditional: single-client point-to-point ICMP tunnels never hit it.
- **Fix**: Key `getConn` on `(srcIP, ICMP id)` (version-aware parse of the Echo id from the IP-header-carrying buffer, mirroring the `b[20:]`/`b[40:]` offsets in `icmpConn.Read`), and emit a randomized 16-bit client id (set once in `NewICMPClientConn` instead of `id:1`). Wire-compat change — coordinate client+server and document. Add the missing ICMP tests including a two-clients-one-IP demux test.
- **Effort**: M

### 1.8 DNST default `MaxWrite` (765) produces responses exceeding the 512-byte UDP DNS limit; no EDNS0
- **Location**: `NewServerConn`/`WriteTagged` (`proto/dnst/dnst_conn.go`)
- **Why it matters**: Default `maxWrite=765`; base32 (no padding) expands to ~1224 chars across ~5 TXT strings, yielding a >1200-byte response. No `SetEdns0`/OPT is ever set, `resp.Compress=false`, and `serverMaxRead=512`. Standard recursive resolvers cap unsolicited UDP responses at 512 bytes without EDNS0 and would truncate/drop these — breaking the file's own advertised "route data through any public DNS resolver" use case. **[partial]** — works on the direct-UDP/1500-MTU path it is sized for (and which e2e covers via a non-truncating mock resolver); only the also-advertised through-resolver path breaks.
- **Fix**: Call `SetEdns0(4096, false)` on client queries and advertise an OPT on responses; when no EDNS0 is present, honor TC/512 (or lower default `maxWrite`); raise `serverMaxRead` above 512. Fix the comment conflating link MTU (1500) with the DNS 512-byte UDP payload limit; document direct-link-only default. Lowest-effort partial fix is the comment/doc correction.
- **Effort**: M

### 1.9 `poll` client `interval<=0` removes the idle throttle (busy-poll / network flood)
- **Location**: poll driver / `pollConnClient.loop` (`poll_conn.go`)
- **Why it matters**: The driver parses `interval` with `time.ParseDuration` and never rejects `<=0`; `WithPollInterval` accepts any value. With `interval=0` (`interval=0s` in the URI) `time.After(c.interval)` fires immediately every iteration, removing the idle throttle. **[partial]** — over real network transport the realistic harm is request flooding/load-amplification (round-trip-gated), not a literal CPU spin; it degenerates to 100% CPU only on immediate-return transports, and requires operator misconfiguration.
- **Fix**: Reject `interval<=0` in the driver (like dtls's `flightinterval`); have `WithPollInterval` ignore non-positive values, falling back to the 1ms default. Add a driver test asserting `interval=0s`/negatives are rejected.
- **Effort**: S

---

## Q2 — Strategic (plan / schedule)

High-value maintainability, architecture, API-consistency, and test-coverage work without same-day urgency. Ordered by impact then urgency.

### 2.1 No unit tests for the parse / type-validation / redaction pipeline
- **Location**: `UnmarshalText` / `OutputFor` / `Apply` / `Redacted` / `Register` (`wrap.go`, `scheme.go`, `uri.go`, `driver.go`, `transport.go`)
- **Why it matters**: The central pipeline abstraction has 0.0% direct coverage (confirmed via `coverprofile`). Untested: chain final-type `Listener`/`Dialer` enforcement, `OutputFor` per `PipeType`, `Apply`'s type switch (incl. multi-interface ambiguity), `Redacted` masking with/without `Fingerprint`, param-parse edges, and `Register` panic-on-dup/nil. **[partial]** — partially exercised transitively by `drivers/dtls`/`drivers/dtlspsk` tests and end-to-end by e2e, but the generic invariants are unasserted.
- **Fix**: Add a root-module `pipeline_test.go` (table-driven): valid/invalid chains per side; `OutputFor`/`InputTypes` across all four `PipeType`s; `Apply` vs `OutputFor` agreement for a value satisfying both `net.Conn` and `TaggedConn`; `Redacted` masking; param-parse edges; `Register` panics.
- **Effort**: M

### 2.2 No tests for the ICMP transport (conn framing or listener demux)
- **Location**: `icmp_conn.go`, `icmp_listener.go`
- **Why it matters**: No `*icmp*_test.go` exists and ICMP is absent from the e2e suite, yet it is a first-class transport with many hazards (NAT collapse, 8-bit echo key, truncation, IPv4-options strip). All other transports (tcp/udp) have tests. Framing (`Read`/`Write` marshal/parse) is fully testable over `net.Pipe` without `CAP_NET_RAW`; only raw-socket `Listen`/`Dial` need privileges.
- **Fix**: Table-driven `icmpConn.Read`/`Write` tests (v4/v6, client/server, header-strip guards, self-echo suppression, short-buffer/oversized payload); a listener demux test over a fake `PacketConn` exercising `getConn`/`dispatchMsg`, `ErrListenQueueExceeded`, and `ErrClosedListener`. Privilege-gated raw-socket e2e later.
- **Effort**: M

### 2.3 No tests for `tls`/`utls`/`tlspsk` drivers; the SPKI pin verifier is untested in all four
- **Location**: `spkiVerifier` / param parsing (`drivers/tls`, `drivers/utls`, `drivers/tlspsk`)
- **Why it matters**: Only dtls/dtlspsk have tests, and those never exercise `spkiVerifier`. The broken-hash pin (item 1.2), the `InsecureSkipVerify`+`VerifyPeerCertificate` coupling, the "servername or cert required" validation, and hex-decode error paths have zero coverage — which is exactly why the SPKI bug shipped (a test asserting the pin is 32 bytes or matches openssl would have failed immediately).
- **Fix**: Unit tests for `spkiVerifier` (matching accepted; mismatched rejected; malformed PEM rejected; empty `rawCerts` rejected); table tests for param validation; an assertion tying the pin/fingerprint to known openssl output (which will fail today and force the `Sum256` fix). Couple with item 1.2.
- **Effort**: M

### 2.4 No tests for the FFI surface (`Netx`/`NetxInterrupt`, `activeCalls` re-entrancy) or version
- **Location**: `Netx` / `NetxInterrupt` / `activeCalls` helpers (`cli/internal/lib/main.go`)
- **Why it matters**: The cli module has one test file; the FFI lifecycle has zero coverage — exactly the re-entrancy/concurrency paths most likely to regress: duplicate-id supersede (`registerActiveCall` cancels prior), stale-entry no-op removal (`if activeCalls[id] == entry`), interrupt-of-running, and concurrent distinct-id isolation. The `activeCalls` logic is deterministic Go (compiles under default cgo).
- **Fix**: Add `cli/internal/lib/main_test.go` testing the `activeCalls` helpers directly with `context.WithCancel`: duplicate-id cancel/replace; stale-entry removal no-op; `interruptActiveCall` true/false; concurrent register/remove/interrupt under `-race`. Skip the C-string integration test (low value vs cost).
- **Effort**: S

### 2.5 Tests never run with the race detector
- **Location**: `task test` (`Taskfile.yml`)
- **Why it matters**: The library is concurrency-heavy (per-conn goroutines, read loops, copy goroutines) yet no target enables `-race`. **[partial]** — a `-race` run surfaces a real **test-code** race (`demux_test.go` shared `err`/`n`) and, with a concurrent-close stress test, deterministically reproduces the `demuxSess` double-close panic (item 1.1) and the `TaggedPipe.readDeadline` race (item 2.7); the originally-claimed "readDeadline and Logger races" are mostly mis-attributed (those fields are correctly synchronized).
- **Fix**: Add `task test:race` running `go test -race ./...` per module (wire into CI). Fix the `demux_test.go` shared-var test race first. Add a stress test hammering concurrent `demuxSess.Close()` vs `demux.Close()`.
- **Effort**: S

### 2.6 `pollConnClient.loop` allocates a `time.After` timer every iteration
- **Location**: `pollConnClient.loop` (`poll_conn.go`)
- **Why it matters**: `time.After(c.interval)` runs inline in `for{}` with no retained handle, so it can't be stopped when the select fires on `sendCh`/`closed`; at the default 1ms that is ~1000 allocs/sec/conn on the idle path. **[partial]** — it is allocation **churn**, not a leak: the loop is single-goroutine and strictly sequential (≤1 pending timer at a time), and Go ≥1.23 (go.mod 1.25.7) GCs orphaned `time.After` timers promptly. No unbounded accumulation.
- **Fix**: Hoist a single reusable `*time.Timer`/`Ticker` before the loop; `Reset`/drain each iteration. Optional micro-optimization.
- **Effort**: S

### 2.7 Data race on `taggedPipeConn.readDeadline`
- **Location**: `taggedPipeConn.ReadTagged` / `SetReadDeadline` (`tagged_pipe.go`)
- **Why it matters**: `ReadTagged` reads `c.readDeadline` (`IsZero()`, `time.Until`) **without** `c.mu`, while `SetReadDeadline` writes it under `c.mu`. Per the `net.Conn` contract, `SetReadDeadline` may run on a different goroutine than the blocked `Read` — a genuine data race on a multi-word `time.Time` (torn read → garbage deadline), flagged by `go test -race`. **[partial]** — `TaggedPipe` is currently used only in this repo's tests (same-goroutine deadline set), so CI doesn't trip it today.
- **Fix**: Snapshot the deadline under `c.mu` (the lock is already taken a few lines above for `c.req`): `c.mu.Lock(); req := c.req; dl := c.readDeadline; c.mu.Unlock()`. Use the local `dl`.
- **Effort**: S

### 2.8 Crypto/util helpers (`spkiVerifier` ×3, `colonHex` ×5, `memSessionStore` ×2, DTLS param block) duplicated verbatim
- **Location**: `drivers/{tls,utls,dtls}` `spkiVerifier`; `drivers/{aesgcm,tls,dtls,tlspsk,dtlspsk}` `colonHex`; `drivers/{dtls,dtlspsk}` `memSessionStore`
- **Why it matters**: A single security fix or pin-format change must currently be applied N times consistently — exactly what lets one copy drift and regress (the broken `Sum()` is duplicated in three places). Every driver `go.mod` already requires the root module, so a shared home exists. **[partial]** — `colonHex` feeds only log-redaction display, not auth, so a "pin-format change" is cosmetic; no live vuln.
- **Fix**: Extract `colonHex`, `spkiVerifier`, `memSessionStore`/`sharedSessionStore`, and a `Fingerprint` helper into the root package (or `drivers/internal/tlsutil`); fix the `Sum256` bug once there; add a cross-file test asserting the verifiers agree.
- **Effort**: S

### 2.9 `tlspsk` uses TLS 1.2 CBC-HMAC-SHA1 (no AEAD, no PFS) while `dtlspsk` uses AES-128-GCM
- **Location**: `init` (`drivers/tlspsk/tlspsk.go`)
- **Why it matters**: Pins `MinVersion==MaxVersion==TLS1.2` and the single suite `TLS_PSK_WITH_AES_256_CBC_SHA` — CBC/HMAC-SHA1 (Lucky13/padding-oracle family), deprecated SHA-1 MAC, no forward secrecy — materially weaker than every other driver, with no signal to operators that this is the weak path. **[partial]** — bounded by a library ceiling (`raff/tls-psk` offers only CBC-SHA1 suites) and still PSK-authenticated+encrypted; the CBC/no-PFS points are stronger than the SHA-1 angle.
- **Fix**: Document the CBC/no-AEAD/no-PFS limitation prominently at the call site, in `README.md`, and in `docs/internals/drivers-proto.md`; steer operators to `dtlspsk` (GCM). Treat replacing `raff/tls-psk` with a TLS1.3-PSK-capable stack as a separate large item.
- **Effort**: S (docs) / L (library swap)

### 2.10 No tests for the ssh proto/driver and the dnst/aesgcm drivers
- **Location**: `drivers/ssh/ssh.go`, `proto/ssh`, `drivers/dnst`, `drivers/aesgcm`
- **Why it matters**: `proto/ssh` and `drivers/ssh` have zero tests, so the `HostKeyCallback` MITM check and `PublicKey`/`Password` callbacks are unverified — exactly where an accept-any-host-key regression would be silent. e2e covers happy paths only (positive assertions); no negative path asserts a mismatched host key is *rejected*. `drivers/dnst`/`drivers/aesgcm` param parsing and the dnst bound helpers (`maxQNAMEPayload`, `splitString63`) against the 253/63/255 limits are uncovered.
- **Fix**: Tests asserting `HostKeyCallback` rejects a mismatched key and accepts the pinned one; driver param-parsing tests (key sizes, unknown params, ssh `pub`, dnst `maxw` client-rejection); unit tests for `maxQNAMEPayload`/`splitString63` at the boundaries.
- **Effort**: M

### 2.11 Missing tests for double-close, write-aliasing, and close-during-write leaks in mux/demux
- **Location**: `demux_test.go` (`TestDemux_Close`)
- **Why it matters**: `TestDemux_Close` closes the listener but never calls `sess.Close()`, so the double-close panic (item 1.1) is uncovered; no test exercises concurrent `demuxSess.Write` (item 1.3); none runs under `-race`. **[partial]** — the originally-claimed "no `-race` readDeadline test" is wrong (`zz_race_check_test.go` exists and *does* fail under `-race`); the real gap is that `task test` doesn't pass `-race` (item 2.5).
- **Fix**: Add (1) close-listener-then-session asserting no panic; (2) concurrent-`Write`-on-one-session under `-race` asserting payload integrity. Most important: wire `-race` into the test target (item 2.5) so the existing race test actually runs.
- **Effort**: S

### 2.12 No test covers the frame size boundary (65535/65536) or poll interval validation
- **Location**: `frame_conn_test.go`, `poll_conn_test.go`
- **Why it matters**: A single boundary test would have caught the frame silent-truncation bug (item 1.5); existing frame tests max at 1024-byte payloads. No test asserts `poll` rejects `interval<=0` (item 1.9). **[partial]** — the frame half guards a real corruption bug and is high-value; the interval half is minor.
- **Fix**: Round-trip a 65535-byte payload (must succeed intact) and `Write` 65536 (must error, never silent); optionally assert `interval<=0` is rejected/clamped.
- **Effort**: S

### 2.13 `dtls` `resume` is silently accepted but knowingly broken for the cert path
- **Location**: `init` (`drivers/dtls/dtls.go`)
- **Why it matters**: The driver accepts `resume=true` and wires `cfg.SessionStore`, but the code's own comment states pion v3.1.2 fails the full certificate handshake with `SessionStore` on both ends; the param is "accepted for forward-compat" and the test deliberately omits it. An operator setting `resume=true` on the cert path gets no benefit or a broken handshake while parse-time validation reports success. **[partial]** — gated behind explicit non-default opt-in; at best a no-op, at worst a broken handshake on the production split.
- **Fix**: Reject `resume=true` on the cert dtls driver with a clear error pointing to `dtlspsk` (where resumption is tested and works), or accept-but-ignore it (don't set `SessionStore`) and log a warning. The branch already exists.
- **Effort**: S

---

## Q3 — Quick wins (low effort, do soon)

Cheap, mostly one-site fixes: stale help/docs, misleading names, leak-prevention guards, secret-redaction gaps. Ordered by impact then urgency.

### 3.1 CLI help advertises nonexistent params and a wrong tlspsk cipher; copied examples error out
- **Location**: `uriFormat` (`cli/internal/help.go`)
- **Why it matters**: `--help` contradicts the code: `frame{maxsize=4096}` (frame rejects **all** params — the example chain hard-errors at parse), `aesgcm` `maxpacket` (aesgcm accepts only `key`), ssh `pubkey` (driver expects `pub`), and `Cipher is TLS_DHE_PSK_WITH_AES_256_CBC_SHA` (code pins `TLS_PSK_WITH_AES_256_CBC_SHA` — no DHE, a misleading forward-secrecy claim). The `dnst` driver (`domain`, `maxw`) and tlspsk `identity` are undocumented.
- **Fix**: Rewrite `help.go` to match the registries (drop `maxsize`/`maxpacket`, `pubkey`→`pub`, correct cipher name + note no-PFS, fix the example to a parseable chain, add `dnst` and tlspsk `identity`). Add a test that parses every example string, or generate help from the driver registry.
- **Effort**: S

### 3.2 Upgrade/wrap-failure errors leak raw secrets to stderr and the FFI callback
- **Location**: `ListenerScheme.Listen`/`DialerScheme.Dial` (`scheme.go`), `Wrappers.Apply` (`wrap.go`), `Run` (`cli/internal/root.go`)
- **Why it matters**: `Redacted()` exists to keep keys/passphrases out of user-visible streams (and is used for info logs), but the Listen/Dial/Apply error paths build messages from `s.String()`/`w.String()` → `render(nil)` (raw `k=v` incl. hex private keys, PSKs, SSH passwords). These propagate to `fmt.Fprintln(cfg.err, err)` → `errWriter` → C callback. **[partial]** — narrower than "any failure": the common obfuscation drivers install wrappers via `tls.NewListener`/`ConnWrap*` that return nil at Apply time, so handshake/dial failures surface post-Apply and are *not* scheme-wrapped; the leak fires only on genuine Apply-time errors.
- **Fix**: Swap `String()`→`Redacted()` at the five sites (`scheme.go` ×4, `wrap.go` ×1). Reserve `String()` for round-trippable serialization. Add a regression test asserting an Apply error contains `REDACTED`, not the raw key.
- **Effort**: S

### 3.3 SSH driver param is `pub` but help and the driver's own errors call it `pubkey`
- **Location**: `ssh` `Register` (`drivers/ssh/ssh.go`), `cli/internal/help.go`
- **Why it matters**: The driver accepts only `case "pub":` and rejects unknown keys, but help and the driver's own error strings ("requires pubkey…") say `pubkey`. A user copying the documented `ssh{pubkey=...}` gets `unknown ssh parameter "pubkey"`. **[partial]** — the working/tested path is `pub` (e2e uses it); this is doc + error-message drift, not a functional break.
- **Fix**: Align `help.go` and the two error strings to `pub` (pairs with `key`/`pass`); optionally accept `pubkey` as an alias. Add a driver test exercising the documented URI form.
- **Effort**: S

### 3.4 `--version` always reports "dev"
- **Location**: `Run` (cobra `Version`) (`cli/internal/root.go`)
- **Why it matters**: `Version: "dev"` is hardcoded and the Taskfile builds with plain `go build` (no `-ldflags -X`), so released binaries/c-shared libs all report `netx version dev` — operators can't tell which build they run, undermining field diagnosis.
- **Fix**: Add `var version = "dev"` in `cli/internal`, set `cmd.Version = version`, inject via `-ldflags "-X .../cli/internal.version=$(git describe --tags --always)"`. Factor into a Taskfile `LDFLAGS_VERSION` var; mirror in the CGO builds.
- **Effort**: S

### 3.5 darwin `dialControl` fails open silently when no usable interface is found
- **Location**: `dialControl` / `primaryPhysicalIfaceIndex` (`cli/internal/tun_darwin.go`)
- **Why it matters**: This binding exists to prevent the `rx=0` loopback inside `NEPacketTunnelProvider`, yet every failure path is silent: `!ok` returns `nil` with no log, `SetsockoptInt` results are discarded. On atypical configs (IPv6-only, non-`en` physical iface, multiple `en*`) the dial proceeds unbound and silently yields `rx=0` — the hardest symptom to diagnose. The success path logs at Info; failures don't. **[partial]** — diagnosability gap (common Mac DHCP-IPv4 case works), not a common-path bug.
- **Fix**: `slog.Warn` when no usable interface is found, and capture+log the `SetsockoptInt` errors instead of discarding them — restoring symmetry with the success log. Default-route selection / IPv6-only handling is a larger optional follow-up.
- **Effort**: S

### 3.6 `aesgcm` `Read` rejects the maximum-size valid packet (off-by-one)
- **Location**: `aesgcmConn.Read` (`proto/aesgcm/aesgcm_conn.go`)
- **Why it matters**: `Read` uses `n == MaxPacketSize` (65535) as a "buffer fully filled → truncated" sentinel, but `Write` legitimately produces a 65535-byte packet (`8 + 65511 + 16`), so the largest legal payload is rejected. Fails closed with an error (no corruption). **[partial]** — edge-case only; the shipped CLI default never reaches it and UDP datagrams cap below it.
- **Fix**: Size the read buffer `MaxPacketSize+1` so "full" unambiguously means oversize (then compare `n > MaxPacketSize`), or drop the sentinel and rely on the framed transport. Add a max-size round-trip test; fix the stale `65512`/`65536-24` comment in `TestAESGCM_MaxPacketWrite` (true boundary: 65511 accepted / 65512 rejected).
- **Effort**: S

### 3.7 `maxQNAMEPayload` uint16 underflow makes `MaxWrite()` lie for domains > 252 chars
- **Location**: `maxQNAMEPayload` (`proto/dnst/dnst_conn.go`)
- **Why it matters**: `available := uint16(252 - len(domain))` wraps to a huge value for `len(domain) > 252` (e.g. 300 → 65488 → returns 610), and `if available <= 0` can never fire on an unsigned value. `MaxWrite()` returns a bogus positive, masking the clean construction-time refusal and deferring failure to a confusing runtime `dns packet too long` on every `Write`. **[partial]** — only under a degenerate operator misconfiguration (a 253+ byte domain suffix is at/beyond max legal DNS name length).
- **Fix**: Compute in signed space: `if len(domain) >= 252 { return 0 }`, then `available` as `int`. Reject over-long domains at the driver / `NewClientConn` so misconfig fails fast. Add a `>=253`-byte domain test.
- **Effort**: S

### 3.8 FFI writers duplicate every byte to `os.Stdout`/`os.Stderr` in addition to the C callback
- **Location**: `outWriter.Write`/`errWriter.Write` (`cli/internal/lib/main.go`)
- **Why it matters**: When a callback is set, each chunk is delivered to the callback **and** written to the host process's stdout/stderr (wrong sink for an embedded library), and the writer returns stdout/stderr's `(n, err)` — so a closed/redirected host stdout makes `slog` see a write error even though the callback succeeded.
- **Fix**: When `w.cb != nil`, deliver to the callback and `return len(p), nil`; only fall back to stdout/stderr when `cb` is nil. Backward-compatible (the e2e lib harness passes nil callbacks and relies on the fallback).
- **Effort**: S

### 3.9 SSH server leaks the conn when the first channel is not `direct-tcpip`
- **Location**: `NewServerConn` (`proto/ssh/ssh_conn.go`)
- **Why it matters**: The channel-accept loop's `default` branch rejects an unsupported channel type and returns an error **without** closing `svConn`, unlike the sibling `direct-tcpip` error path. **[partial]** — in the canonical `ListenerToListener` path `connWrappedListener.Accept` closes the underlying conn on error (tearing down the ssh mux and its goroutines), so the genuine leak is only the atypical `ConnToConn` path; this is a defensive-symmetry fix.
- **Fix**: Add `_ = svConn.Close()` in the `default` branch before returning, mirroring the other error paths.
- **Effort**: S

### 3.10 `dtls`/`dtlspsk` `skipcookie` comment understates UDP reflection/amplification risk
- **Location**: `init` (`drivers/dtls/dtls.go`, `drivers/dtlspsk/dtlspsk.go`)
- **Why it matters**: `skipcookie=true` sets `InsecureSkipVerifyHello`, disabling the DTLS HelloVerifyRequest cookie. Comments argue the PSK/pinned key "gates abuse," but the cookie protects against blind source-IP spoofing: a spoofed ClientHello forces handshake state and a server flight toward the victim before any key is proven. **[partial]** — opt-in/default-off; for **dtls** (cert) the Certificate flight is a real reflection vector, for **dtlspsk** it's mainly pre-auth CPU/state (no Certificate in the PSK flight); the risk is already partly noted in internal docs.
- **Fix**: Comment/doc-only: state the actual trade-off (pre-auth state + reflection), distinguish the two drivers, recommend upstream source-validation/rate-limiting, and add a caveat to the `README.md` driver table. No code logic change.
- **Effort**: S

### 3.11 SPKI verifier matches ANY cert in the chain (not leaf-only) under `InsecureSkipVerify`
- **Location**: `spkiVerifier` (`drivers/dtls/dtls.go`, and identically `drivers/tls`, `drivers/utls`)
- **Why it matters**: The loop returns success if **any** `rawCerts` entry matches the pin; with `InsecureSkipVerify=true` the chain is never built, so an attacker presenting `[attacker_leaf, pinned_cert]` passes (handshake proves only `attacker_leaf`). "Leaf only" is not enforced. **[partial]** — narrow: real-world exposure requires pinning a CA/intermediate, which this codebase never does (self-signed single-cert pins); the originally-bundled "empty SPKI" sub-claim is unreachable and should be dropped.
- **Fix**: Pin against `rawCerts[0]` (leaf) only and reject empty `rawCerts`, across the three drivers. Fold into the same patch as the `Sum256` fix (item 1.2).
- **Effort**: S

### 3.12 `Tun.Relay` collapses both directions on first EOF — breaks half-close
- **Location**: `Tun.halfCopy` / `Tun.Relay` / `Tun.Close` (`tun.go`)
- **Why it matters**: Each `halfCopy` does `defer t.Close()`, and `Close` closes **both** `Conn` and `Peer`; the first direction to finish force-closes the other mid-flight (and the truncated direction reports `nil` via the `closing` check, so it's silent). No `CloseWrite`/half-close handling. **[partial]** — only bites raw `tcp→tcp` passthrough carrying half-close-dependent protocols; the obfuscation-tunnel use and udp/icmp are unaffected, and most wrapped conn types lack `CloseWrite`.
- **Fix**: Cheapest: document on `Tun.Relay`/`Tun.Close` that half-close is not preserved (first direction to finish closes both). Optional larger fix: type-assert `dst` to `interface{ CloseWrite() error }` on src EOF and full-`Close` only on real errors.
- **Effort**: S (doc) / M (half-close support)

### 3.13 Eager warm-peer with a failed pre-dial poisons the first real client
- **Location**: `runTun` (warm peer) (`cli/internal/tun.go`), `newDeferredConn`/`deferredConn` (`cli/internal/eager.go`)
- **Why it matters**: The route handler does `warmPeer.Swap(nil)` and assigns it **without** checking whether the deferred dial ultimately failed; the consumed peer's first I/O returns the stale dial error and the just-accepted inbound conn is torn down with no lazy fallback. **[partial]** — reclassify as resilience gap, not a leak (the failed conn is closed); eager is opt-in, blast radius is exactly the first connection (subsequent conns use the lazy `dialPeer` retry), and clients self-heal.
- **Fix**: Peek the deferred dial outcome before committing it as `peer` (add `tryResult()` reading `d.ready`/`d.err`, or call `w.await()`); on error, `w.Close()` and fall through to the lazy `dialPeer(ctx)` path.
- **Effort**: S

### 3.14 ICMP self-echo suppression keyed on only `seq%256` and never evicted
- **Location**: `rememberSent` / `consumeSent` (`icmp_conn.go`)
- **Why it matters**: The sent-payload table is indexed by `uint8(seq%256)` (low 8 bits of a 16-bit seq) and `consumeSent` never deletes a matched entry. A legitimate reply whose payload equals a previously-sent payload at a colliding low-byte seq is dropped as a "self echo"; stale hashes persist and suppress real replies 256 seq later; seq 256 apart overwrite each other. Plausible silent data loss for repeated control/keepalive frames. **[partial]** — probabilistic (needs payload + low-byte-seq collision); ICMP is niche with no coverage; the repo's own internals doc records the fix as wire-neutral.
- **Fix**: Key on the full 16-bit seq (`map[uint16][32]byte`) and have `consumeSent` actually delete on match; better, distinguish OS auto-replies by ICMP type/source rather than payload hashing. Rename `consumeSent` if it won't consume.
- **Effort**: S

### 3.15 `frame_conn.go` doc comment says 4-byte header; code uses a 2-byte uint16
- **Location**: `frameConn` package doc / `NewFrameConn` (`frame_conn.go`)
- **Why it matters**: Both the file-level doc and `NewFrameConn` godoc claim a 4-byte big-endian length header, but the code uses `[2]byte` + `Uint16`. Misleads anyone reasoning about max frame size (65535, not 4 GiB) or building an interop client. This is the known drift called out in `CLAUDE.md`.
- **Fix**: Update both comments to "2-byte big-endian uint16 (max payload 65535 bytes)". Cross-check `README.md`/`docs/mux-tag-poll.md` for the same stale claim.
- **Effort**: S

### 3.16 `NewBufConn` godoc references nonexistent `WithBufWriterSize`/`WithBufReaderSize`
- **Location**: `NewBufConn` (`buffered_conn.go`)
- **Why it matters**: The doc tells callers to use `WithBufWriterSize`/`WithBufReaderSize`, but the actual options are `WithBufWrite`/`WithBufRead` — code copied from the comment won't compile.
- **Fix**: Change the comment to reference `WithBufWrite`/`WithBufRead`.
- **Effort**: S

### 3.17 `TunMaster` doc comment references nonexistent `SetHandler` (actual: `SetRoute`)
- **Location**: `TunMaster` (`tun.go`)
- **Why it matters**: The type doc says handlers are added "via `SetHandler`," but the API is `SetRoute` (mirroring `Server`) — misleads readers of the public godoc.
- **Fix**: Change "via `SetHandler`" to "via `SetRoute`".
- **Effort**: S

### 3.18 `tlspsk` ships a hardcoded ed25519 dummy private key; X509KeyPair error swallowed
- **Location**: `dummyCert` / `init` (`drivers/tlspsk/tlspsk.go`)
- **Why it matters**: `dummyCert` embeds a full ed25519 private key in source to satisfy `raff/tls-psk`'s `Certificates` requirement, and swallows the `X509KeyPair` error. **[partial]** — no exploitable path today (pure-PSK auth + `InsecureSkipVerify`, so the cert authenticates nothing); the "forgeable identity / forward-secrecy" framing is overstated. Pure hygiene against a future refactor enabling cert verification.
- **Fix**: Stop ignoring the `X509KeyPair` error; add a comment that the cert is a tls-psk structural placeholder that must never authenticate. (The separate cipher-name typo in help is covered by item 3.1.)
- **Effort**: S

### 3.19 `colonHex` panics on an empty slice (cap underflow)
- **Location**: `colonHex` (`drivers/aesgcm/aesgcm.go` and 4 copies)
- **Why it matters**: `make([]byte, 0, len(b)*3-1)` underflows to cap `-1` for empty `b` → `makeslice: cap out of range`. **[partial]** — unreachable today (always called with a fixed 8-byte slice; helper is unexported), so a latent landmine in a 5×-copied helper.
- **Fix**: Guard `if len(b) == 0 { return "" }` (or `max(0, len(b)*3-1)`). Best done once if the helper is lifted to a shared package (item 2.8).
- **Effort**: S

### 3.20 `Wrapper.render` iterates `Params` without sorting → non-deterministic `String()`/`MarshalText()`/`Redacted()`
- **Location**: `Wrapper.render` (`wrap.go`)
- **Why it matters**: Ranging a map gives randomized order, so output is unstable for any 2+-param wrapper (reproduced: 5 distinct outputs over 50 calls). **[partial]** — semantic round-trip still works (`UnmarshalText` re-parses to a map); the only present effect is inconsistent log-line param ordering. Cheap hardening against future golden/config-equality tests.
- **Fix**: Collect keys, `sort.Strings`, build pairs in sorted order.
- **Effort**: S

### 3.21 `dnst` server `serverConn.ReadTagged` panics on a nil tag pointer while the tagged sibling guards it
- **Location**: `serverConn.ReadTagged` (`proto/dnst/dnst_conn.go`)
- **Why it matters**: `*tag = m` is dereferenced unconditionally, so a nil `*any` panics; `taggedServerConn.ReadTagged` guards the same write with `if tag != nil`. **[partial]** — latent: no in-tree caller passes nil (`demux_tagged`/`tagged_pipe` always pass `&tag`); defensive consistency, not a live crash.
- **Fix**: `if tag != nil { *tag = m }`, matching the tagged variant. Optionally document whether a nil tag is permitted in the `TaggedConn.ReadTagged` interface comment.
- **Effort**: S

---

## Q4 — Backlog (low impact / low urgency)

Cosmetics, defensive guards, naming, and minor consistency. Ordered by impact then urgency.

### 4.1 Hot-spin accept loop on persistent `Accept` errors (no backoff)
- **Location**: `Server.Serve` (`server.go`), `mux.acceptLoop` (`mux.go`)
- **Why it matters**: On a non-closing `Accept` error both loops log and immediately `continue`; under EMFILE/ENFILE the loop spins at full CPU and floods logs. `net/http.Server.Serve` uses capped backoff. Same pattern in `demux_listener.go` and `wrap.go`.
- **Fix**: Capped backoff (~5ms doubling to ~1s) on non-fatal errors, reset on success; keep the closing/closed early-return before sleeping. Apply to all four loops.
- **Effort**: S

### 4.2 Massive duplication between `pollConnServer` and `pollConnClient`
- **Location**: `pollConnServer` / `pollConnClient` (`poll_conn.go`)
- **Why it matters**: ~140 lines of `Read`/`Write`/`Close`/deadline/`MaxWrite` are copy-pasted character-for-character; only `loop()` differs, so a fix in one (e.g. the timer change) must be remembered in the other.
- **Fix**: Extract a shared embedded `pollConnBase` implementing the 9 common methods; embed in both, leaving only `loop()` and the constructors distinct. No external references exist.
- **Effort**: M

### 4.3 Unsynchronized `Server.Logger` lazy-init races with concurrent `Serve`
- **Location**: `Server.Serve` (`server.go`)
- **Why it matters**: `if s.Logger == nil { s.Logger = slog.Default() }` is unsynchronized; two concurrent `Serve` (the design-supported multi-listener pattern) write/write race (reproduced under `-race`). **[partial]** — not reachable by the shipped single-listener CLI; only by external embedders with a nil Logger.
- **Fix**: Replace the mutating lazy-init (here and the identical `Tun.Relay` pattern) with a non-mutating `logger()` helper returning `slog.Default()` when nil; call it at the use sites.
- **Effort**: S

### 4.4 `Apply()` type-switch order diverges from `OutputFor()` for dual-interface values
- **Location**: `Wrapper.Apply` / `Wrapper.OutputFor` (`wrap.go`)
- **Why it matters**: `Apply` lists `case net.Conn:` before `case TaggedConn:`, while `OutputFor` prefers `TaggedTo*`. For a value satisfying both interfaces against a wrapper setting both fields (dnst server), validation and execution pick different transforms. **[partial]** — dormant: no current chain feeds such a value into dnst's Tagged input (`*mux` is not a `net.Conn`).
- **Fix**: Reorder `Apply`'s switch so `case TaggedConn:` precedes `case net.Conn:`, mirroring `OutputFor`. Add a test asserting they agree for a dual-interface value.
- **Effort**: S

### 4.5 `URI`/`Scheme` `MarshalText` emit raw secrets via `encoding.TextMarshaler`
- **Location**: `URI.MarshalText` (`uri.go`), `Scheme.MarshalText` (`scheme.go`)
- **Why it matters**: Both implement `TextMarshaler` via `String()` (raw secrets), and `URI` embeds `Scheme` with `json:"scheme"`. An embedder JSON-marshaling or struct-logging a populated URI would serialize keys/PSKs/passwords in cleartext. **[partial]** — latent API trap: no code path currently JSON-marshals/struct-logs these (`grep encoding/json` → 0 hits), and live logging already uses `Redacted()`.
- **Fix**: Drop the `json:"scheme"` tag and/or remove `MarshalText`, **or** add redacting `MarshalJSON` + `slog.LogValuer` delegating to `Redacted()` (keeping `String()`/`MarshalText` round-trippable). Add a test asserting `json.Marshal`/`slog` don't expose a secret param.
- **Effort**: S

### 4.6 Anonymous `interface{ MaxWrite() uint16 }` duplicated at 7 call sites
- **Location**: `split_conn.go`, `demux.go`, `demux_tagged.go`, `demux_client.go`, `poll_conn.go` (×2), `proto/aesgcm/aesgcm_conn.go`
- **Why it matters**: The central packet-size-negotiation contract is re-declared inline everywhere instead of as one named interface, making it undiscoverable in godoc and un-referenceable by name. **[partial]** — external drivers already satisfy it structurally; the benefit is discoverability, not enabling something impossible.
- **Fix**: Declare `type MaxWriter interface { MaxWrite() uint16 }` once in the root package with a doc comment; use it at all sites. Note the `proto/aesgcm` site needs a root version bump per module versioning.
- **Effort**: S

### 4.7 Inconsistent driver error-message prefixes (`uri:` vs bare names) and doubled `uri: uri:`
- **Location**: `drivers/dnst/dnst.go`, core `buf`/`poll`/`split`/`mux` drivers, `Wrapper.UnmarshalText` (`wrap.go`)
- **Why it matters**: Three conventions coexist (`uri:`+name, bare name, mixed within `mux.go`); since `Wrapper.UnmarshalText` re-wraps with `uri: setup driver …`, `uri:`-prefixed driver messages get doubled.
- **Fix**: Standardize on one scheme; since `wrap.go` already prepends `uri: setup driver <name>:`, drop the leading `uri:` from driver messages. Fix `mux.go`'s internal inconsistency. (Spans versioned modules — each edit needs its own bump.)
- **Effort**: S

### 4.8 Doubled `uri: uri:` prefix in the unknown-driver error
- **Location**: `Wrapper.UnmarshalText` (`wrap.go`)
- **Why it matters**: `GetDriver` already prefixes `uri:`, and `UnmarshalText` re-wraps with `uri: %w`, producing user-facing `uri: uri: unknown driver "…"`.
- **Fix**: Return `err` directly at the wrap site (it already carries `uri:`); leave `GetDriver` self-describing.
- **Effort**: S

### 4.9 Chain-validation error reports `w.String()` (leaks secrets)
- **Location**: `Wrappers.UnmarshalText` validation loop (`wrap.go`)
- **Why it matters**: The position-`i` error prints `w.String()` (raw params), leaking secrets on a malformed chain. **[partial]** — confirmed for secrets; the "post-driver internal field-set state" rationale is inaccurate (`render` emits only Name+Params).
- **Fix**: Use `w.Redacted()` (and at the sibling sites in 3.2). Optionally echo the original chain token at index `i`.
- **Effort**: S

### 4.10 DNST reads silently truncate when the caller buffer < decoded payload
- **Location**: `clientConn.Read` / `serverConn.ReadTagged` / `taggedServerConn.ReadTagged` (`proto/dnst/dnst_conn.go`)
- **Why it matters**: All three end with `return copy(b, data), nil`; a small buffer truncates with no error and no carry-over, contrasting `aesgcm` (`io.ErrShortBuffer`) and `frameConn` (buffers remainder). **[partial]** — masked today (all callers use `MaxPacketSize` buffers; DNS payload is protocol-bounded well below that).
- **Fix**: Guard `if len(b) < len(data) { return 0, io.ErrShortBuffer }` per read path (mirroring aesgcm). Add a short-buffer test. Document the contract.
- **Effort**: S

### 4.11 `mux.readConn`/`muxClient` dereference `RemoteAddr()`/`LocalAddr()` unconditionally in logging
- **Location**: `mux.readConn` (`mux.go`), `mux_client.go`
- **Why it matters**: `conn.RemoteAddr().Network()` panics if `RemoteAddr()` returns nil. **[partial]** — unreachable for in-tree TCP/UDP/ICMP transports (always non-nil) and only at DEBUG level; matters only for exported `NewMux`/`NewMuxClient` with custom listeners/dialers.
- **Fix**: Add a helper `addrStr(a net.Addr) string` returning `""` when nil; use it at the five log sites.
- **Effort**: S

### 4.12 ICMP `Read` silently truncates payloads larger than the caller's buffer
- **Location**: `icmpConn.Read` (`icmp_conn.go`)
- **Why it matters**: `n = copy(b, pkt.Data)` discards the tail with no error and no reassembly. **[partial]** — unreachable in practice (upper layers read with 65535-byte buffers against an 8192-byte receive cap); already documented as a known hazard.
- **Fix**: `if len(pkt.Data) > len(b) { return 0, io.ErrShortBuffer }` before the copy, matching the header-strip guards.
- **Effort**: S

### 4.13 ICMP fixed 20/40-byte IP-header strip breaks on IPv4 options
- **Location**: `icmpConn.Read` (`icmp_conn.go`)
- **Why it matters**: The client read strips exactly 20 (v4) / 40 (v6) bytes before `icmp.ParseMessage`; an IPv4 packet with options (IHL up to 60) makes the strip land mid-header → misparse, broken framing. **[partial]** — IPv4 options are rare and netX's own traffic always has IHL=5; only externally-injected options trigger it.
- **Fix**: For v4, compute `ihl := int(b[0]&0x0f)*4`, validate `20<=ihl<=n`, strip `b[ihl:n]` (or use `ipv4.ParseHeader`). Comment the IPv6 fixed-40 assumption.
- **Effort**: S

### 4.14 `proto/dnst` declares `package netx`, shadowing the root package
- **Location**: `package netx` (`proto/dnst/dnst_conn.go`)
- **Why it matters**: The dnst proto package is `package netx` (siblings: `aesgcmproto`, `sshproto`) and imports the root (also `netx`), forcing the driver's `dnstproto` alias and confusing the reader's mental model.
- **Fix**: Rename to `package dnstproto` (also the two test files' clauses and the int-test's self-alias); the driver's existing alias becomes a plain import. Bundle with the next `proto/dnst` version bump.
- **Effort**: S

### 4.15 `demuxSess.Write` returns wrong byte count on partial body writes
- **Location**: `demuxSess.Write` / `taggedDemuxSess.Write` (`demux.go`, `demux_tagged.go`)
- **Why it matters**: Returns `n - len(s.id)`; only `n < len(s.id)` is treated as short write, so a partial body write (`len(s.id) <= n < len(payload)`) returns a positive-but-wrong count with nil error. **[partial]** — unreachable in practice (demux is packet-oriented; all underlying layers are datagram/all-or-nothing on Write).
- **Fix**: Treat any `n < len(payload)` as `io.ErrShortWrite`, or document the packet-oriented (atomic-write) assumption.
- **Effort**: S

### 4.16 `demuxSess.Write` captures the write deadline once and ignores it during a blocking underlying Write
- **Location**: `demuxSess.Write` (`demux.go`)
- **Why it matters**: Snapshots `writeDeadline`, checks `After` once, then calls a potentially-blocking `bc.Write`; no `writeDlNotify` mechanism (the read path has one). A deadline set to unblock an in-progress write has no effect. **[partial]** — low impact: the canonical transport is non-blocking UDP; only a TCP transport with a full send buffer exposes it.
- **Fix**: Document that the write deadline is a coarse pre-check only. Do **not** propagate `SetWriteDeadline` to the shared `bc` (would clobber other sessions).
- **Effort**: S

### 4.17 `connWrappedListener.Accept` aborts the accept loop on a single bad-client wrap/handshake error
- **Location**: `connWrappedListener.Accept` (`wrap.go`)
- **Why it matters**: On `wrapConn` failure it closes the conn and `return nil, err`, indistinguishable from listener death (concretely the SSH server path via `sshproto.NewServerConn`). **[partial]** — not a leak (conn is closed) and not an abort in any in-repo loop (`Server.Serve`/`mux` log-and-continue); only an observability/external-embedder concern.
- **Fix**: Optionally classify per-conn wrap failures (sentinel or `net.Error` Temporary) so loops/logs distinguish skip-and-continue from listener death.
- **Effort**: S

### 4.18 `consumeSent` takes the write `Lock` for a read-only op
- **Location**: `icmpConn.consumeSent` (`icmp_conn.go`)
- **Why it matters**: Only reads `sentHashes` but acquires exclusive `Lock()` (the sibling read at the server path uses `RLock`). **[partial]** — cosmetic: nanosecond critical section on a per-conn mutex; relieves only Read-vs-Write on one conn, not "all Reads."
- **Fix**: Use `RLock()`/`RUnlock()` (unless the eviction fix in 3.14 adds a delete, which needs `Lock`).
- **Effort**: S

### 4.19 ICMP `SetReadBuffer`/`SetWriteBuffer` errors ignored; raw-socket privilege failures unwrapped
- **Location**: `icmpListenConfig.Listen` (`icmp_listener.go`)
- **Why it matters**: Buffer-size errors are discarded (`_ =`) so a rejected size silently no-ops, and `net.ListenIP`/Dial errors surface verbatim with no `CAP_NET_RAW`/root hint, making the common "operation not permitted" opaque.
- **Fix**: Wrap the `ListenIP`/Dial error with a `CAP_NET_RAW`/root hint via `%w` (additive, zero-risk). Surface the `SetBuffer` errors only once a logger is threaded into the config.
- **Effort**: S

### 4.20 `errors.Join(err, cmd.Help())` is an obscure way to print help on failure
- **Location**: `tun` `RunE` (`cli/internal/tun.go`)
- **Why it matters**: `cmd.Help()` prints as a side effect and returns nil normally, so the `Join` exists only for output; a reader expects `errors.Join` to combine error values, and a non-nil `Help()` error would be concatenated into the user-facing message.
- **Fix**: `_ = cmd.Help()` then `return err`.
- **Effort**: S

### 4.21 Assorted small consistency / defensive nits
A batch of confirmed-but-trivial items, fixable together:
- **ICMP `dial`/`listen` version detection nil-deref** — `iaddr, _ := conn.LocalAddr().(*net.IPAddr)` then derefs `iaddr.IP` unchecked (`dial.go`, `icmp_listener.go`). **[partial]** unreachable on the normal POSIX path. *Fix*: check the assertion `ok` + non-nil, close conn and return a wrapped error.
- **Dead empty-scheme check** — `if len(parts) == 0` after `strings.SplitN` is unreachable (`Scheme.UnmarshalText`, `scheme.go`). *Fix*: remove it, or check `len(text)==0` before splitting.
- **`Transport.UnmarshalText` unused `listener bool`** (`transport.go`). **[partial]** intentional symmetry, documented in `CLAUDE.md`. *Fix*: leave as-is or add a one-line note; do not rename to `_` (breaks the documented pattern).
- **`mux.WriteTagged` per-conn-only deadline** undocumented (`mux.go`). **[partial]** design-correct (per-conn write vs shared-channel read); race window unreachable. *Fix*: one-line comment.
- **`mux.ReadTagged` dead `!ok` branch** for an `rQueue` that is never closed (`mux.go`). *Fix*: replace with a comment that `rQueue` is intentionally never closed and `doneCh` is the sole termination path.
- **`pollConnServer` response Write has no write deadline** (`poll_conn.go`). **[partial]** not load-bearing for the non-blocking DNS-tunnel transport. *Fix*: optional bounded deadline + an invariant note that poll assumes a non-blocking request-response transport.
- **`Close()`/`Shutdown()` share one `closing` CAS** — the second call is a silent no-op (`server.go`). **[partial]** intentional, documented in internals. *Fix*: add a godoc note on the public methods that they are mutually-exclusive one-shots (first wins; a cancelled/short Shutdown ctx forces close).
- **`Shutdown` polls remaining conns on a fixed 10ms ticker** (`server.go`). **[partial]** force-close already wakes immediately on `ctx.Done()`; only natural completion lags ≤10ms. *Fix*: optional — signal a channel from `closed()` when count hits 0; the `ctx.Err()` pre-check adds negligible value.
- **Serve/connCtx cancellation does not interrupt in-flight relays** (`tun.go`, `server.go`) — only `Close`/`Shutdown` tear them down. *Fix*: document that ctx cancellation does not stop active relays.
- **`Server.route` variable shadowing / `&connCloser` aliasing** (`server.go`). **[partial]** correct and tested; the `&local`-as-map-key idiom is just non-obvious. *Fix*: a one-line comment or rename to `returnedCloser`; do not refactor away the indirection (load-bearing).
- **`closed()` double-call deadlocks the calling goroutine** (`server.go`). **[partial]** documented intentional one-shot; only handler calls it once today. *Fix*: optional — make `closed()` idempotent via `sync.Once`/atomic so a second call is a no-op.
- **`frameConn.Read` returns `(0, nil)` for empty keepalive frames** (`frame_conn.go`). **[partial]** intended, tested, and documented in internals; the poll-churn premise is incorrect. *Fix*: only add a one-line godoc note on `Read`; do **not** loop-skip empty frames (breaks empty-packet delivery).
- **DNST client `Read` returns `(0, nil)` on answerless responses** (`proto/dnst/dnst_conn.go`). **[partial]** absorbed by the mandatory poll layer's `if n > 0` guard. *Fix*: loop to read the next packet (restoring io.Reader idiom), or document the keep-alive contract.
- **DNST server `WriteTagged` doesn't enforce its advertised `MaxWrite`** (`proto/dnst/dnst_conn.go`). **[partial]** enforced one layer up by `split`; not analogous to the client's hard 253-byte limit. *Fix*: optional `if len(b) > maxWrite` guard for symmetry.
- **`icmpConn.Write` reads `c.id` without the lock on the client path** (`icmp_conn.go`). **[partial]** benign (`c.id` immutable client-side). *Fix*: move `id = c.id` inside the locked region (above `rememberSent`, which re-acquires the non-reentrant mutex), or drop the lock there with a comment.
- **ICMP server echoes the single last-seen id/seq** (`icmp_conn.go`). **[partial]** mutex-guarded (not a race) and harmless today (client never correlates by id/seq). *Fix*: none required; if multiplexed streams ever become a goal, carry id/seq with the data (TaggedConn) — a deliberate wire change, out of scope as a bug.
- **`SetWriteDeadline` `wMu` on poll conns guards only `writeDeadline`, not writes** (`poll_conn.go`). **[partial]** correct per `net.Conn` semantics; just a misleading name. *Fix*: optional one-line comment (or rename to `deadlineMu`).
- **SPKI pins compared with non-constant-time `bytes.Equal`** (`drivers/tls/tls.go`). **[partial]** compared data is public SPKI material; low-risk hygiene. *Fix*: once pins are real 32-byte digests (item 1.2), use `subtle.ConstantTimeCompare`.
- **Process-global DTLS resume session store shared across all keypairs/PSKs** (`drivers/dtls/session.go`). **[partial]** not exploitable (resumption requires mutual proof of the stored master secret); the "cross-context splicing" framing is refuted. *Fix*: optional defense-in-depth — partition the store per key-material fingerprint and add a TTL; document the secret lives in process memory.
- **`SecretParam` redaction fingerprints the public cert to redact the private key** (`drivers/{tls,dtls}`). **[partial]** intended, cross-driver-consistent design (matches ssh; the only operator-cross-checkable choice). *Fix*: only copy the explanatory comment from `tls.go` into `dtls.go` for doc parity; do **not** change the computation.
- **`utls` handshakes eagerly in `ConnToConn` while `tls`/`dtls` are lazy** — inconsistent error timing (Dial vs first I/O) across the TLS family (`drivers/{utls,tls,dtls}`). **[partial]** the original claim wrongly grouped dtls as eager; it is lazy like tls. *Fix*: a one-line comment marking tls/dtls intentionally lazy vs utls eager (or unify by adding/dropping `Handshake`).
- **`NewDemux` parameter `idMask` is actually an ID length in bytes** (`demux.go`) — "mask" implies a bit pattern; used purely as a byte length (same misnomer in `NewDemuxListener`, `NewTaggedDemux`; tests already call it `idLen`). *Fix*: pure rename `idMask`→`idLen` (param + unexported field) across the three constructors. Non-breaking.
- **`deferredConn.LocalAddr` returns the upstream remote stub** before the conn settles (`cli/internal/eager.go`) — mislabels the peer's address as local. **[partial]** latent (nothing reads this conn's `LocalAddr`). *Fix*: return nil or a neutral local stub; reserve `d.remote` for `RemoteAddr`.
- **`Netx` with nil/empty id collides all such calls onto the same `activeCalls` key** (`cli/internal/lib/main.go`) — `C.GoString(nil)` → `""`; a second `Netx("")` silently cancels the first. **[partial]** the collide-cancels contract is documented; this is the degenerate empty case. *Fix*: reject empty id (mirror `NetxInterrupt`'s nil guard) and document ids must be non-empty + unique.
- **Unbounded `argv` index cast / nil-truncation in FFI** (`cli/internal/lib/main.go`) — `(*[1<<28]*C.char)(...)` panics for pathological `argc`; a mid-array NULL silently truncates. **[partial]** the bound is itself a fail-safe; trigger requires violating the C argv convention; contract already documented in internals. *Fix*: validate `argc` against a sane cap and return an error; pick a deliberate NULL policy.
- **`aesgcm` handshake read goroutine can block up to 5s (or forever) on IV-write failure when the conn isn't closed** (`proto/aesgcm/aesgcm_conn.go`). **[partial]** no in-repo caller leaks (standard pipeline closes the conn on wrap error); only the exported `NewAESGCMConn` write window leaks for hypothetical direct callers. *Fix*: close `conn` on any handshake error before returning (or keep the deadline in force on the error path / use a context-cancellable read).
- **`driver.go` `Register` stores names case-sensitively but lookups are lowercased** — a mixed-case driver name is permanently unreachable via URIs (`driver.go`). Latent (all built-ins lowercase). *Fix*: `strings.ToLower(strings.TrimSpace(name))` in `Register`; document names are case-insensitive.
- **`listnerer` misspelled** in `icmpListenConfig.Listen` (`icmp_listener.go`, ~12 local uses). *Fix*: rename to `listener`/`l`.

---

## Appendix — Considered and dismissed

A few notable claims that were investigated and **refuted** (excluded above):

- **"`tagQueue` (2× `rQueue`) never closed → Write blocks forever"** — The 2× sizing and the tag-before-write blocking are **documented by design** in `docs/internals/demux.md` (the authoritative change-safety reference per `CLAUDE.md`); `Close()` already unblocks any waiting `Write` via the `s.closed` select, and *not* closing `tagQueue` is correct (`Read` does `s.tagQueue <- td.tag` guarded only by `s.closed`, so closing it would panic on send).
- **"`frameConn.Write` returns `n=0` after the header is committed → callers retry and duplicate the header"** — `frameConn.Write` satisfies the `io.Writer` contract (returns `n<len(p)` **with** a non-nil error); the contract says conforming callers must **not** retry, and every real writer in the repo (`io.CopyBuffer`, `pollConnServer.loop`, `splitConn.Write`) terminates on error. No retry path exists.
- **"`Tun.Relay` logs benign EOF/`ErrClosed` closes as ERROR on every closed tunnel"** — `io.Copy` normalizes a clean EOF to `nil`, so the first finisher (reading from the closing side) logs nothing, and the other direction's `net.ErrClosed` is suppressed once `t.closing` is true. Reproduced empirically: clean closes emit **0** ERROR logs; only an abnormal RST does (which is genuinely abnormal).
- **"DTLS resume store enables cross-security-context session splicing"** — DTLS abbreviated-handshake resumption requires mutual proof of the stored master secret; a peer with different key material cannot forge a valid `Finished`, so a shared store does not cross security contexts. Server session IDs are 32-byte random, not attacker-influenced. (A minor per-key-partition / TTL hardening note survives as item in 4.21.)