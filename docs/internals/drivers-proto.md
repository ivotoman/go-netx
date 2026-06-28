# Drivers & Proto Modules

> Internals reference for the optional protocol-wrapper tiers of go-netx.
> Code is cited by **symbol + file** (e.g. `spkiVerifier (tls.go)`), not line
> numbers, so cites survive refactors. Bare line numbers appear only for
> stable-symbol-less literals, and always name their enclosing function/struct.

---

## Purpose & role

go-netx keeps its **root module dependency-light** by exiling protocol
implementations into two tiers of *separate* Go modules:

1. **`proto/*`** — heavier `net.Conn` / `TaggedConn` protocol implementations.
   Each is its own module so its third-party deps (miekg/dns, x/crypto) never
   pollute the root. Members: `proto/aesgcm`, `proto/dnst`, `proto/ssh`.
   - `proto/aesgcm/aesgcm_conn.go` and `proto/ssh/ssh_conn.go` use their own
     package names (`aesgcmproto`, `sshproto`).
   - `proto/dnst/dnst_conn.go` declares `package netx` (NOT a proto-suffixed
     name) — its tests live in the same package and reach into internals. Flag:
     the package identifier differs from the other two proto modules.

2. **`drivers/*`** — thin adapters. Each driver's `init()` calls
   `netx.Register(name, factory)` (root `Register` in `driver.go`). Members:
   `drivers/aesgcm`, `drivers/dnst`, `drivers/dtls`, `drivers/dtlspsk`,
   `drivers/ssh`, `drivers/tls`, `drivers/tlspsk`, `drivers/utls`. A driver
   typically wires a `proto/*` type in (aesgcm/dnst/ssh) or wires an external
   library directly (dtls/dtlspsk via pion, tls via stdlib, tlspsk via raff/tls-*,
   utls via refraction-networking).

There is **no `reality` driver** despite README claims — the registered names
are exactly the eight above plus the core six (`buf`, `frame`, `mux`, `demux`,
`poll`, `split`).

### Blank-import activation
Registration is a side effect of package `init()`. A driver only exists if its
package is imported (blank import in cli mains). `Register` (`driver.go`)
**panics** on duplicate name or nil driver, so importing the same driver twice
into one binary is a hard crash.

### Server/client duality
Every factory has signature `func(params map[string]string, listener bool)
(netx.Wrapper, error)` (the `Driver` type in `driver.go`). The **same registered
name** produces a *different* `Wrapper` depending on `listener`:
- `listener == true`  → server side (cert+key for TLS/DTLS, host key for SSH,
  PSK identity hint, dnst `maxw`, dtls `skipcookie`, etc.).
- `listener == false` → client side (SPKI pinning / servername, client auth,
  required identity for PSK, no `maxw`/`skipcookie`, etc.).

### How a Wrapper is consumed
`netx.Wrapper` (the `Wrapper` struct in `wrap.go`) is a struct of optional
function fields, one per `PipeType` transition. The pipeline composition,
`Apply` dispatch, and `OutputFor`/`InputTypes` chain-validation are documented
in **pipeline.md** — the only driver-relevant rules are:
- **Set only the fields for the PipeTypes you support**, at most one per input
  type (comment on the `Wrapper` struct, `wrap.go`).
- Use `ConnWrapListener` / `ConnWrapDialer` (`wrap.go`) to lift a single
  `ConnToConn` closure into listener/dialer form. The listener form closes the
  accepted conn if the wrap fails (`connWrappedListener.Accept`, `wrap.go`).

### Secret redaction (`Wrapper.SecretParams`)
`Wrapper.String()` (`wrap.go`) renders **all** params verbatim, including
secrets — it is *not* log-safe. The log-safe renderer is `Wrapper.Redacted()`
(and `Wrappers.Redacted()`), which masks every param named in
`Wrapper.SecretParams` (`SecretParam` struct in `wrap.go`). Each `SecretParam`
carries a `Name` and a precomputed `Fingerprint` rendered inside
`REDACTED(<fingerprint>)`; an empty `Fingerprint` renders a bare `REDACTED`
(used for low-entropy secrets where a fingerprint would be brute-forceable).
See `Wrapper.render` (`wrap.go`).

**Invariant for new drivers:** any hex secret (key/psk/host key/private key)
MUST be declared in `SecretParams` or it leaks through any code path that calls
`String()`/`MarshalText()`. Fingerprint forms in use today:
- **TLS / DTLS** (`tls.go`, `dtls.go`): `sha256=` + first 8 bytes of
  `sha256.Sum256(cert DER)`, colon-hex — matches `openssl x509 -fingerprint`.
- **PSK** (`tlspsk.go`, `dtlspsk.go`): `sha256=` + first 8 bytes of
  `sha256.Sum256(psk)`, colon-hex.
- **aesgcm** (`aesgcm.go`): `sha256=` + first 8 bytes of `sha256.Sum256(key)`,
  colon-hex.
- **SSH key** (`sshSecretParams`, `ssh.go`): `SHA256:` + base64 of
  `sha256.Sum256(signer.PublicKey().Marshal())` — i.e. the fingerprint of the
  public key *derived from* the `key` private key (not the separate `pub` param);
  the full fingerprint `ssh-keygen -lf` prints.
- **SSH pass** (`sshSecretParams`, `ssh.go`): bare `REDACTED` (no fingerprint).

`utls` declares **no** `SecretParams` — its only sensitive-looking param is
`cert`, which is a public pin, not a secret.

---

## Per-driver reference

Each cert/key/psk driver populates `Wrapper.SecretParams` (see above) — noted
per driver below.

### aesgcm  (`drivers/aesgcm/aesgcm.go`)
- **Registered name:** `"aesgcm"`.
- **Params:** only `key` accepted; any other → `unknown aesgcm parameter`
  (`default:` in the param `switch`).
  - `key` (**required, both sides**): hex-decoded; must be 16/24/32 bytes else
    `invalid aesgcm key size`; empty/missing → `missing aesgcm key parameter`.
- **PipeTypes / Wrapper fields:** `ConnToConn`, `ListenerToListener` (via
  `ConnWrapListener`), `DialerToDialer` (via `ConnWrapDialer`). Same fields
  server and client (symmetric).
- **SecretParams:** `key` → `sha256=`+colon-hex of `sha256.Sum256(key)[:8]`.
- **Underlying proto:** `aesgcmproto.NewAESGCMConn(c, aeskey)`.
- **External deps:** `proto/aesgcm v1.1.0`, root `v1.4.0` (`drivers/aesgcm/go.mod`).

### dnst  (`drivers/dnst/dnst.go`)
- **Registered name:** `"dnst"`.
- **Params:** `domain`, `maxw`; else `dnst: unknown parameter`.
  - `domain` (**required, both sides**): plain string, no hex; missing/empty →
    `dnst: missing domain parameter`.
  - `maxw` (**server-only, optional**): `ParseUint(value,10,16)`; **rejected on
    client** with `dnst: max write parameter is only valid for listeners`; feeds
    `dnstproto.WithMaxWrite(uint16)`.
- **PipeTypes / Wrapper fields — asymmetric:**
  - **Server:** `ConnToTagged` → `dnstproto.NewServerConn`; `TaggedToTagged` →
    `dnstproto.NewTaggedServerConn`. Outputs a **TaggedConn** (consumed by tagged
    demux, see proto internals).
  - **Client:** `ConnToConn` → `dnstproto.NewClientConn`. Plain `net.Conn` only
    (no listener/dialer adapters).
- **SecretParams:** none (`domain` is not secret).
- **External deps:** `proto/dnst v1.1.0`, `miekg/dns v1.1.72` (via proto),
  root `v1.4.0` (`drivers/dnst/go.mod`).

### dtls  (`drivers/dtls/dtls.go`)
- **Registered name:** `"dtls"`.
- **Params:** `key`, `cert`, `servername`, `mtu`, `flightinterval`, `nobackoff`,
  `skipcookie`, `resume`; else `unknown dtls parameter` (`default:` in the param
  `switch`).
  - `key` (**server-required, client-forbidden**): hex. Client passing `key` →
    `dtls client does not support key parameter`.
  - `cert` (**server-required; client-optional**): hex.
  - `servername` (**client-only useful**): sets `cfg.ServerName`.
  - `mtu` (**optional, both sides**): `ParseUint(...,10,32)`, must be **576..1500**
    else `invalid dtls mtu parameter ... (want 576..1500)`; sets `cfg.MTU` (pion
    default 1200). Fragments handshake flights to avoid IP-fragmented records.
  - `flightinterval` (**optional, both sides**): `time.ParseDuration`, must be
    `> 0`; sets `cfg.FlightInterval` (pion default 1s) — initial retransmit
    interval.
  - `nobackoff` (**optional, both sides**): `ParseBool`; sets
    `cfg.DisableRetransmitBackoff`.
  - `skipcookie` (**server-only**): `ParseBool`; sets `cfg.InsecureSkipVerifyHello`
    (drops the HelloVerifyRequest cookie round-trip). Client → `dtls skipcookie
    parameter is only valid for servers`. **Weakens anti-spoof DoS protection** —
    safe only because the SPKI-pinned handshake cannot complete without the
    pinned key.
  - `resume` (**optional, both sides**): `ParseBool`; if true sets
    `cfg.SessionStore = sharedSessionStore()` (see [session.go](#session-resumption-store-sessiongo)).
    **CAVEAT (documented in the `resume` case, `dtls.go`):** on pion/dtls v3.1.2
    the cert-based dtls driver *fails the full handshake* when a separate
    SessionStore is set on each end (the production client/server split). The
    param is accepted for forward-compat — **prefer `resume` on `dtlspsk`**,
    where it works.
- **Server requires** both `cert` AND `key` → else `dtls server requires cert
  and key parameters`; parsed with `tls.X509KeyPair`, set as `cfg.Certificates`.
- **Client requires** `servername` OR `cert`. If `cert` set →
  `InsecureSkipVerify = true` + `VerifyPeerCertificate = spkiVerifier(cert)`
  (SPKI pinning).
- **PipeTypes / Wrapper fields:**
  - **Server:** `ListenerToListener` (`dtls.NewListener` over
    `dtlsnet.PacketListenerFromListener`), `ConnToConn` (`dtls.Server`).
  - **Client:** `DialerToDialer` (via `ConnWrapDialer` + `dtls.Client`),
    `ConnToConn` (`dtls.Client`).
- **SecretParams (server only):** `key` → `sha256=`+colon-hex of
  `sha256.Sum256(cert DER)[:8]`.
- **External deps:** `pion/dtls/v3 v3.1.2`, root `v1.4.0` (`drivers/dtls/go.mod`).
  No proto module.

### dtlspsk  (`drivers/dtlspsk/dtlspsk.go`)
- **Registered name:** `"dtlspsk"`.
- **Params:** `key`, `identity`, `mtu`, `flightinterval`, `nobackoff`,
  `skipcookie`, `resume`; else `unknown dtlspsk parameter` (`default:` in the
  param `switch`).
  - `key` (**required, both sides**): hex PSK; empty/missing → `missing dtlspsk
    key parameter`.
  - `identity` (**client-required, server-optional**): plain string; server uses
    it as `cfg.PSKIdentityHint`. Client without identity → `dtlspsk client
    requires identity parameter`.
  - `mtu` / `flightinterval` / `nobackoff` / `skipcookie` / `resume`: **same
    semantics, validation, and side-restrictions as the dtls driver** (skipcookie
    server-only → `dtlspsk skipcookie parameter is only valid for servers`).
    Unlike dtls, `resume` works here.
- **Fixed config (`cfg` literal, `dtlspsk.go`):** `PSK` callback returns `psk`;
  `CipherSuites = {dtls.TLS_PSK_WITH_AES_128_GCM_SHA256}` (single suite);
  `InsecureSkipVerify = true` (PSK auth, no cert chain). MTU/flightinterval/
  backoff/skipcookie/resume applied after the literal.
- **PipeTypes / Wrapper fields:**
  - **Server:** `ListenerToListener`, `ConnToConn`.
  - **Client:** `DialerToDialer`, `ConnToConn`.
- **SecretParams:** `key` → `sha256=`+colon-hex of `sha256.Sum256(psk)[:8]`.
- **External deps:** `pion/dtls/v3 v3.1.2`, root `v1.4.0` (`drivers/dtlspsk/go.mod`).

### ssh  (`drivers/ssh/ssh.go`)
- **Registered name:** `"ssh"`.
- **Params:** `pass`, `key`, `pub`; else `unknown ssh parameter`.
  - `pass` (**optional, both sides**): plain string.
  - `key` (hex PEM → `ssh.ParsePrivateKey`): on **server** the **host key**
    (**required**); on **client** the **private key** for pubkey auth (optional).
  - `pub` (hex authorized-key → `ssh.ParseAuthorizedKey`): on **server** the
    client's allowed pubkey (optional auth method); on **client** the **expected
    host key** for pinning (**required**).
- **Server constraints:** missing host `key` → `ssh server requires key
  parameter`; must supply at least one auth method — `pub` (PublicKeyCallback,
  compares marshaled bytes) or `pass` (PasswordCallback) — else `ssh server
  requires pubkey or pass parameter`.
- **Client constraints:** missing `pub` → `ssh client requires pubkey parameter`
  (used for `HostKeyCallback` pinning); must supply at least one auth —
  `key`→`ssh.PublicKeys` and/or `pass`→`ssh.Password` — else `ssh client
  requires key or pass parameter`.
- **PipeTypes / Wrapper fields:**
  - **Server:** `ListenerToListener` (via `ConnWrapListener`), `ConnToConn` →
    `sshproto.NewServerConn`.
  - **Client:** `DialerToDialer` (via `ConnWrapDialer`), `ConnToConn` →
    `sshproto.NewClientConn`.
- **SecretParams (`sshSecretParams`, `ssh.go`):** `pass` → bare `REDACTED`;
  `key` (if present) → `SHA256:`+base64 of
  `sha256.Sum256(signer.PublicKey().Marshal())` (public key derived from the
  private `key`, not the `pub` param).
- **External deps:** `proto/ssh v1.1.0`, `golang.org/x/crypto v0.49.0`,
  root `v1.4.0` (`drivers/ssh/go.mod`).

### tls  (`drivers/tls/tls.go`)
- **Registered name:** `"tls"`.
- **Fixed config:** `MinVersion == MaxVersion == tls.VersionTLS13` (`cfg`
  literal, `tls.go`) — **TLS 1.3 only**.
- **Params:** `key`, `cert`, `servername`; else `unknown tls parameter`. Same
  shape and rules as dtls (minus the dtls tuning params):
  - server requires `cert` AND `key` (`tls.X509KeyPair`);
  - client forbids `key` (`tls client does not support key parameter`);
  - client `cert` → `InsecureSkipVerify=true` + `spkiVerifier`;
  - client requires `servername` OR `cert`.
- **PipeTypes / Wrapper fields:**
  - **Server:** `ListenerToListener` (`tls.NewListener`), `ConnToConn`
    (`tls.Server`).
  - **Client:** `DialerToDialer` (via `ConnWrapDialer` + `tls.Client`),
    `ConnToConn` (`tls.Client`).
- **SecretParams (server only):** `key` → `sha256=`+colon-hex of
  `sha256.Sum256(cert DER)[:8]`.
- **External deps:** stdlib `crypto/tls` only; root `v1.4.0` (`drivers/tls/go.mod`).

### tlspsk  (`drivers/tlspsk/tlspsk.go`)
- **Registered name:** `"tlspsk"`.
- **Params:** `key`, `identity`; else `unknown tlspsk parameter`.
  - `key` (**required, both sides**): hex PSK; empty/missing → `missing tlspsk
    key parameter`.
  - `identity` (**client-required, server-optional**): client without it →
    `tlspsk client requires identity parameter`.
- **Fixed config (`cfg` literal, `tlspsk.go`):** `MinVersion == MaxVersion ==
  tls.VersionTLS12` (**TLS 1.2 only**); `Extra` PSKConfig supplies identity + key
  callbacks; `CipherSuites = {tlspks.TLS_PSK_WITH_AES_256_CBC_SHA}` (single
  suite); `InsecureSkipVerify = true`.
- **Server quirk:** sets a hardcoded ed25519 self-signed cert from `dummyCert()`
  (`tlspsk.go`) because raff/tls-psk requires server-side `Certificates` to be
  present. The cert/key are embedded PEM literals inside `dummyCert`.
- **PipeTypes / Wrapper fields:**
  - **Server:** `ListenerToListener` (via `ConnWrapListener` +
    `tlswithpks.Server`), `ConnToConn`.
  - **Client:** `DialerToDialer` (via `ConnWrapDialer` + `tlswithpks.Client`),
    `ConnToConn`.
- **SecretParams:** `key` → `sha256=`+colon-hex of `sha256.Sum256(psk)[:8]`.
- **External deps:** `raff/tls-ext v1.0.0`, `raff/tls-psk v1.0.0`, root `v1.4.0`
  (`drivers/tlspsk/go.mod`). Import aliases: `tlswithpks` =
  `github.com/raff/tls-ext`, `tlspks` = `github.com/raff/tls-psk`.

### utls  (`drivers/utls/utls.go`)
- **Registered name:** `"utls"`.
- **CLIENT-ONLY:** `listener == true` → immediate error `utls is exclusive to
  clients, use tls for servers instead`.
- **Fixed config:** `MinVersion == MaxVersion == tls.VersionTLS13` (`cfg`
  literal, `utls.go`).
- **Params:** `cert`, `servername`, `hello`; else `unknown utls parameter`.
  - `cert` (optional): hex; if set → `InsecureSkipVerify=true` + `spkiVerifier`.
  - `servername` (optional): `cfg.ServerName`.
  - Requires `servername` OR `cert`.
  - `hello` (optional, default `HelloChrome_Auto`): case-insensitive map —
    `chrome`→HelloChrome_Auto, `firefox`→HelloFirefox_Auto, `ios`→HelloIOS_Auto,
    `android`→HelloAndroid_11_OkHttp, `safari`→HelloSafari_Auto,
    `edge`→HelloEdge_Auto, `randomized`→HelloRandomizedALPN,
    `randomizednoalpn`→HelloRandomized; unknown → `unknown utls hello profile`.
- **PipeTypes / Wrapper fields:** `DialerToDialer` (via `ConnWrapDialer`),
  `ConnToConn`. Both call `utls.UClient(c, cfg, id)` then **eagerly invoke
  `uc.Handshake()`** and return its error — unlike stdlib `tls.Client` which is
  lazy.
- **SecretParams:** none (`cert` is a public pin).
- **External deps:** `refraction-networking/utls v1.8.2` (pulls brotli,
  klauspost/compress), root `v1.4.0` (`drivers/utls/go.mod`).

---

## Param matrix

Encoding key: **hex** = `hex.DecodeString`; **str** = raw string;
**uint16** = `ParseUint(…,10,16)`; **uint32** = `ParseUint(…,10,32)`;
**bool** = `ParseBool`; **dur** = `time.ParseDuration`. R = required, O = optional,
✗ = forbidden, n/a = not applicable.

| driver   | param          | server | client | encoding | notes |
|----------|----------------|:------:|:------:|----------|-------|
| aesgcm   | key            | R      | R      | hex      | size must be 16/24/32 bytes |
| dnst     | domain         | R      | R      | str      | trailing `.` normalized in proto |
| dnst     | maxw           | O      | ✗      | uint16   | client → error "only valid for listeners" |
| dtls     | cert           | R      | O      | hex(PEM) | client cert ⇒ SPKI pin |
| dtls     | key            | R      | ✗      | hex(PEM) | client key ⇒ error |
| dtls     | servername     | O      | O*     | str      | client needs servername OR cert |
| dtls     | mtu            | O      | O      | uint32   | 576..1500; sets cfg.MTU |
| dtls     | flightinterval | O      | O      | dur      | > 0; initial retransmit interval |
| dtls     | nobackoff      | O      | O      | bool     | DisableRetransmitBackoff |
| dtls     | skipcookie     | O      | ✗      | bool     | InsecureSkipVerifyHello; weakens anti-spoof |
| dtls     | resume         | O      | O      | bool     | shared store; CAVEAT: broken on cert dtls |
| dtlspsk  | key            | R      | R      | hex      | the PSK |
| dtlspsk  | identity       | O      | R      | str      | server uses as PSKIdentityHint |
| dtlspsk  | mtu            | O      | O      | uint32   | 576..1500 |
| dtlspsk  | flightinterval | O      | O      | dur      | > 0 |
| dtlspsk  | nobackoff      | O      | O      | bool     | |
| dtlspsk  | skipcookie     | O      | ✗      | bool     | server-only |
| dtlspsk  | resume         | O      | O      | bool     | shared store; works here |
| ssh      | key            | R      | O      | hex(PEM) | server=host key; client=priv key (auth) |
| ssh      | pub            | O      | R      | hex(authkey) | server=allowed client key; client=host key pin |
| ssh      | pass           | O      | O      | str      | server needs pub OR pass; client needs key OR pass |
| tls      | cert           | R      | O      | hex(PEM) | client cert ⇒ SPKI pin |
| tls      | key            | R      | ✗      | hex(PEM) | client key ⇒ error |
| tls      | servername     | O      | O*     | str      | client needs servername OR cert |
| tlspsk   | key            | R      | R      | hex      | the PSK |
| tlspsk   | identity       | O      | R      | str      | |
| utls     | cert           | n/a    | O      | hex(PEM) | server side rejected entirely |
| utls     | servername     | n/a    | O*     | str      | client needs servername OR cert |
| utls     | hello          | n/a    | O      | str      | default chrome |

`O*` = optional individually but at least one of {servername, cert} is required
on the client.

---

## Session resumption store (session.go)

`drivers/dtls/session.go` and `drivers/dtlspsk/session.go` each define an
identical `memSessionStore` and `sharedSessionStore()` singleton, used only when
a wrapper opts in with `resume=true`.

- **Process-global & shared across all instances** of that driver:
  `sharedSessionStore()` lazily builds one `memSessionStore` via `sync.Once`
  and hands the *same* store to every listener/dialer in the process. This is
  deliberate — the relay rebuilds its listener on each switch, but the store
  outlives it so a returning peer can still resume. **Change hazard:** this is
  shared mutable state; do not assume per-connection isolation.
- **Bounded:** `max = 4096` entries (the literal in `sharedSessionStore`,
  `session.go`). On overflow `Set` evicts one **arbitrary** entry (relies on Go's
  randomized map iteration — a DoS-safe backstop, not LRU). A lost ticket only
  forces a full handshake.
- **`Get` on a miss returns a zero `dtls.Session` (ID == nil), not an error** —
  pion treats that as "no resumption".

---

## Proto conn internals

### aesgcm — `proto/aesgcm/aesgcm_conn.go`
- **Wire framing (per datagram, packet-boundary preserving):**
  `[8-byte seq big-endian][GCM(ciphertext||tag)]`. The conn does **not** add its
  own length framing — it assumes the underlying conn preserves boundaries (tests
  wrap in `netx.NewFrameConn`).
- **Nonce derivation:** 12-byte IV; per-packet nonce = IV with its **last 8
  bytes XORed** with the 8-byte seq (XOR at `nonce[4+i]` in both Read and Write).
  The seq is also the GCM AAD (`buf[:8]` as additional data). seq is an atomic
  counter, `seq.Add(1)-1` starts at 0.
- **Passive IV handshake (on creation):** write IV is `crypto/rand`; duplex
  exchange under a **5s deadline** — a goroutine `io.ReadFull`s the peer's 12-byte
  IV while the current side writes its own; deadline cleared after. Both ends must
  construct simultaneously or the handshake blocks (tests build the pair
  concurrently).
- **Size limits:** total packet must fit `netx.MaxPacketSize` (= 65535, the
  `MaxPacketSize` const in root `packet.go`). Write rejects
  `len(p)+8+Overhead > MaxPacketSize`; Read rejects `n == MaxPacketSize` and
  `n < 8+Overhead`. Short read buffer → `io.ErrShortBuffer`, packet dropped.
- **MaxWrite chaining:** if the *underlying* conn exposes `MaxWrite() uint16`
  (e.g. dnst), aesgcm subtracts its own `8+Overhead` header and re-exposes the
  remainder via its own `MaxWrite()` (`NewAESGCMConn` + `aesgcmConn.MaxWrite`);
  too-small underlying MaxWrite → constructor error.

### dnst — `proto/dnst/dnst_conn.go`
- **Direction split:** client→server data rides in the DNS query **QNAME**;
  server→client data rides in the response **TXT** record. Single
  request/response per exchange; cannot distinguish clients without payload help.
- **Encoding:** base32 **StdEncoding, no padding** (`encoding` field, set in each
  constructor).
- **Client Write (`clientConn.Write`):** base32-encode → `splitString63` into
  ≤63-byte DNS labels joined by `.` → append `domain + "."` → reject if full
  QNAME > 253 → build TXT question with random `dns.Id()`, RecursionDesired.
- **Client Read (`clientConn.Read`):** unpack DNS; empty Answer → `(0,nil)`;
  first answer must be `*dns.TXT` else `invalid dns response type`; join TXT
  strings, base32-decode.
- **Server ReadTagged (`serverConn.ReadTagged`):** **silently skips** invalid DNS
  packets, no-question messages, wrong-domain queries, and bad-encoding labels
  (continues the loop, logs Debug) so port-53 noise doesn't kill the conn. Domain
  match is suffix + case-insensitive; label-separator dots are stripped before
  decode. **The parsed `*dns.Msg` is returned as the tag.**
- **Server WriteTagged (`serverConn.WriteTagged`):** tag MUST be the `*dns.Msg`
  from the matching Read (`SetReply`); else `invalid context for dnst write`.
  Encoded payload is `splitString`-chunked to 255-byte TXT segments.
- **TaggedConn variant (`NewTaggedServerConn`):** wraps an underlying
  `netx.TaggedConn` (e.g. a Mux) instead of `net.Conn`. Its tag is a
  `serverConnTagged{dnsMsg, connTag}` so the underlying routing tag (e.g. which
  TCP conn in a Mux) is carried end-to-end alongside the DNS message; WriteTagged
  forwards `ct.connTag` to the underlying `WriteTagged`.
- **MaxWrite contract (consumed by `split` and tagged-demux):**
  - **Server `maxWrite` default = 765** (struct-literal default in
    `NewServerConn`/`NewTaggedServerConn`, dnst_conn.go; reasoning on
    `WithMaxWrite`: 765 = max ciphertext given a 255-byte QNAME on a 1500-MTU UDP
    path), overridable via `WithMaxWrite` / driver `maxw`. Exposed by
    `serverConn.MaxWrite()` / `taggedServerConn.MaxWrite()`.
  - **Client `maxWrite`** is **computed from domain length** by `maxQNAMEPayload`
    accounting for base32 expansion (5 raw → 8 chars) and 63-byte label splitting
    within the 253-char QNAME budget; exposed by `clientConn.MaxWrite()`.
  - Both `serverConn`/`taggedServerConn` are `netx.TaggedConn`s → feed
    `NewTaggedDemux`, whose MaxWrite math subtracts the session id mask. The
    client `net.Conn`'s MaxWrite is consumed by `split` (`NewSplitConn`) and by
    the demux client (`NewDemuxClient`, which subtracts the id length).
- **DNS constants:** `serverMaxRead = 512` read buffer (const, dnst_conn.go);
  TXT chunks 255; QNAME labels 63 (`splitString63`); QNAME total ≤253.
- **Address/deadline passthrough** to the underlying conn for both server
  variants.

### ssh — `proto/ssh/ssh_conn.go`
- **Channel model:** after the SSH handshake, exactly one **`direct-tcpip`**
  channel carries the byte stream; all SSH global/channel requests are discarded.
- **Server (`NewServerConn`):** `ssh.NewServerConn`; `go DiscardRequests`;
  iterate incoming channels — accept the first `direct-tcpip` (returns the
  `ssh.Channel` as the `net.Conn` body), reject any other channel type with
  `UnknownChannelType` and error out; if the channel range ends with none opened
  → error.
- **Client (`NewClientConn`):** `ssh.NewClientConn` then
  `OpenChannel("direct-tcpip", nil)` (extra data **nil** — no SOCKS-style
  host/port payload).
- **Conn shape (`sshConn`):** embeds `ssh.Channel`; `Close` joins channel+SSH
  conn close; `CloseWrite` forwards to channel and to the base conn if it supports
  CloseWrite; `LocalAddr`/`RemoteAddr` from the SSH conn; deadlines delegate to
  the **base** conn `bc` — the SSH channel itself has no deadline support.

---

## Security-critical patterns

These behaviors are load-bearing for confidentiality/integrity. Changing them
silently weakens security.

1. **SPKI pinning (tls / dtls / utls clients).** Identical `spkiVerifier` is
   copy-pasted in all three (`spkiVerifier` in `tls.go`, `dtls.go`, `utls.go`).
   When a client `cert` is supplied: `InsecureSkipVerify = true` is set **and**
   `VerifyPeerCertificate` pins on `RawSubjectPublicKeyInfo`. **The two are a
   unit — removing the verifier while keeping InsecureSkipVerify = no auth at
   all.** Note: the verifier uses `sha256.New().Sum(spki)` (appends `spki` to an
   empty hash) rather than `Sum256`; it is self-consistent on both the pin and
   the peer compare, so pinning works, but any switch to `Sum256` must change
   both call sites in `spkiVerifier` together or pinning breaks.
2. **PSK cipher suites are pinned to a single suite each — interop-critical:**
   - tlspsk: `tlspks.TLS_PSK_WITH_AES_256_CBC_SHA`, TLS **1.2 only** (`cfg`
     literal, `tlspsk.go`).
   - dtlspsk: `dtls.TLS_PSK_WITH_AES_128_GCM_SHA256` (`cfg` literal,
     `dtlspsk.go`).
   Both set `InsecureSkipVerify = true` (auth is the PSK). Changing suite or
   version breaks all peers.
3. **TLS version pinning:** tls/utls are **TLS 1.3 only** (Min==Max==1.3,
   `cfg` literals in `tls.go`/`utls.go`); tlspsk is **TLS 1.2 only**. dtls/dtlspsk
   leave the version to pion but **do** expose handshake tuning via params: `mtu`,
   `flightinterval`, `nobackoff`, and `skipcookie` (= `InsecureSkipVerifyHello`,
   which drops anti-spoof cookie verification) plus session `resume`. `skipcookie`
   is the only one with a security cost — see the dtls driver entry.
4. **SSH host-key & peer pinning is mandatory and exact-match:** client
   `HostKeyCallback` compares `key.Marshal()` to the pinned `pub`; server
   `PublicKeyCallback` compares `key.Marshal()` to `pub`. Returning `nil` =
   accept. No CA/known_hosts fallback.
5. **aesgcm nonce uniqueness depends on (a) random per-conn IV and (b) the
   monotonic seq counter.** Reusing an (IV, seq) pair with the same key is a
   catastrophic GCM nonce-reuse. seq is `atomic.Uint64` and never wraps in
   practice; the IV is freshly random per connection. Sending the IV in cleartext
   during the handshake is intentional (only the key is secret).
6. **utls fingerprinting:** the client mimics a real browser/OS TLS ClientHello
   via uTLS `ClientHelloID` (default Chrome). The handshake is forced eagerly.
   Changing the default `hello` changes the network fingerprint (anti-censorship
   behavior).
7. **dnst server tolerates hostile noise by design** — `serverConn.ReadTagged`
   silently drops malformed/foreign DNS rather than erroring. Do not convert
   those `continue`s into returns; doing so makes the server trivially DoS-able by
   any stray port-53 packet.
8. **tlspsk dummy cert** (`dummyCert`, `tlspsk.go`) is a fixed, public, embedded
   ed25519 self-signed cert. It is NOT a secret and provides NO authentication —
   auth is the PSK. Do not treat it as a real server certificate.
9. **Secrets must be redacted in logs.** `String()` leaks secret param values;
   only `Redacted()` masks them via `SecretParams`. See [Secret
   redaction](#secret-redaction-wrappersecretparams).

---

## Cross-module dependencies

This repo is a **multi-module workspace** (`go.work` lists root + proto/* +
drivers/*). Each module pins its own deps; version coupling matters for releases.

| module | requires (direct) | go-netx root pin |
|--------|-------------------|------------------|
| root `go-netx` | `pion/transport/v3 v3.1.1`, `golang.org/x/net v0.52.0` (+ x/sys indirect) | — (is root) |
| `proto/aesgcm` | root | **v1.4.0** |
| `proto/dnst` | `miekg/dns v1.1.72`, root | **v1.4.0** |
| `proto/ssh` | `golang.org/x/crypto v0.49.0` | (no root dep) |
| `drivers/aesgcm` | root, `proto/aesgcm v1.1.0` | **v1.4.0** |
| `drivers/dnst` | root, `proto/dnst v1.1.0` | **v1.4.0** |
| `drivers/dtls` | root, `pion/dtls/v3 v3.1.2` | **v1.4.0** |
| `drivers/dtlspsk` | root, `pion/dtls/v3 v3.1.2` | **v1.4.0** |
| `drivers/ssh` | root, `proto/ssh v1.1.0`, `x/crypto v0.49.0` | **v1.4.0** |
| `drivers/tls` | root | **v1.4.0** |
| `drivers/tlspsk` | root, `raff/tls-ext v1.0.0`, `raff/tls-psk v1.0.0` | **v1.4.0** |
| `drivers/utls` | root, `refraction-networking/utls v1.8.2` | **v1.4.0** |

Non-obvious facts:
- **`proto/ssh` does NOT depend on root** (pure x/crypto). `proto/aesgcm` and
  `proto/dnst` do (MaxPacketSize, Logger, TaggedConn) and pin root **v1.4.0**.
- aesgcm/dnst/ssh drivers pin both root and their proto, so a root-type change
  used by a proto is a **3-hop bump**: bump root → re-tag proto against new root →
  bump driver's proto pin. Forgetting the re-tag leaves drivers on a stale root.
- All modules are `go 1.25.7` with consistent transitive pins; a mismatch surfaces
  at `go work sync` / `go mod tidy`.

---

## Change hazards

Each item points back to the authoritative section above; don't duplicate the
detail here.

1. **Hex-decode errors must surface.** Every `key`/`cert`/`pub` goes through
   `hex.DecodeString` with a wrapped error — never swallow it (a silently-empty
   key produces a broken/insecure conn). See per-driver Params.
2. **Cert verification is a unit.** Don't detach `VerifyPeerCertificate` from
   `InsecureSkipVerify=true`, and don't change one SPKI hash construction without
   the other. See Security #1.
3. **aesgcm wire contract** (12-byte IV, last-8-byte XOR, 8-byte seq header, no
   extra framing) is interop + GCM security. See Proto > aesgcm and Security #5.
4. **dnst encoding/sizing is fixed protocol** (base32 std/no-pad, `.`-strip,
   63-byte labels, 255-byte TXT, 253 QNAME, `maxQNAMEPayload` math). See Proto >
   dnst.
5. **PSK suite/version changes break interop.** Single-element CipherSuites lists.
   See Security #2.
6. **New params need a `default:` case AND side-restrictions.** A param added to
   the parse `switch` but not validated is accepted blind; one with no case is
   rejected as unknown. Honor side-restrictions (dnst `maxw` server-only;
   tls/dtls client `key` forbidden; dtls/dtlspsk `skipcookie` server-only). See
   per-driver Params.
7. **MaxWrite / TaggedConn contracts feed `split` and the demuxes.** dnst's
   `MaxWrite()` (server 765, client computed) and TaggedConn nature flow into
   `NewSplitConn`, `NewTaggedDemux`, `NewDemuxClient`; aesgcm both consumes and
   re-exposes a reduced MaxWrite. Returning 0 disables `split`. See Proto > dnst /
   aesgcm.
8. **`resume` shares process-global mutable state** and is broken on the
   cert-based dtls driver (pion v3.1.2). See [session.go](#session-resumption-store-sessiongo)
   and the dtls driver entry.
9. **New cert/key/psk drivers MUST set `SecretParams`** or the secret leaks via
   `String()`/logs. See [Secret redaction](#secret-redaction-wrappersecretparams).
10. **Registered name is the public URI identifier.** `Register` panics on
    duplicate; renaming a name is a breaking config change. utls is **client-only**
    (listener → error). SSH assumes one `direct-tcpip` channel with nil extra data.
    See Per-driver reference + Security #7.
11. **Cross-module bumps are a 3-hop dance** for aesgcm/dnst/ssh. See
    Cross-module dependencies.

---

## How to add a driver+proto safely

Grounded in the existing pattern:

1. **(If heavy/extra-deps) create `proto/<name>/` as its own module.** Implement
   `net.Conn` (or `TaggedConn` if the write path needs read-time context, like
   dnst). Mirror `proto/aesgcm` or `proto/dnst`. If you touch root types, pin the
   root version in its `go.mod`.
2. **Implement `MaxWrite() uint16` if the protocol caps payload size** so
   `split` / demux layers compose (pattern: `serverConn.MaxWrite`,
   `aesgcmConn.MaxWrite`). Return 0 if uncapped.
3. **Create `drivers/<name>/` module with an `init()` calling
   `netx.Register("<name>", factory)`** (pattern: `aesgcm.go`). The name becomes
   the public URI keyword — choose it permanently.
4. **In the factory: loop params in a `switch` with a `default:` that returns
   `unknown <name> parameter`.** Hex-decode keys/certs/passwords and **return
   wrapped errors on decode failure**.
5. **Branch on `listener`.** Validate required-on-each-side params and reject
   forbidden / side-restricted ones (server cert+key required; client `key`
   forbidden; client requires servername-or-cert; server-only knobs like
   `skipcookie`).
6. **Declare `SecretParams` for every hex secret**, with a standard-form
   `Fingerprint` (sha256= colon-hex for TLS/PSK/aesgcm; `SHA256:`base64 for SSH;
   bare for low-entropy secrets). See [Secret redaction](#secret-redaction-wrappersecretparams).
7. **Set exactly the Wrapper fields for the PipeTypes you support.** Use
   `ConnWrapListener` / `ConnWrapDialer` to lift a `ConnToConn` closure. Set
   `ConnToTagged` / `TaggedToTagged` only if you produce a TaggedConn (pattern:
   dnst). At most one field per input type.
8. **Always set `Name`, `Params: params`, `Listener: listener`** (used by
   `String()`/`MarshalText()`).
9. **Add the driver's blank import to the cli mains** (see modules-cli.md) and add
   its `go.mod` requires (root + proto + external). Verify `go work sync` succeeds.
10. **Document cipher/version/fingerprint choices** if security-relevant and treat
    them as wire contract (§Security-critical patterns).

---

## Commands

```bash
# Test a single driver/proto module (must cd into it — workspace root won't reach it)
cd drivers/dtls    && go test ./...     # dtls_test.go
cd drivers/dtlspsk && go test ./...     # dtlspsk_test.go
cd proto/aesgcm    && go test ./...     # aesgcm_conn_test.go
cd proto/dnst      && go test ./...     # dnst_conn_test.go + dnst_conn_int_test.go

task test                                # go test across EVERY workspace module
task test:e2e:tun                        # real TLS/DTLS/SSH/aesgcm/dnst tunnels, echo round-trip
```

---

## Related docs
- `docs/internals/pipeline.md` — how Wrappers are composed; Apply/OutputFor/InputTypes/ConnWrap adapters.
- `docs/internals/demux.md` — id-prefixed session demultiplexing (consumes MaxWrite).
- `docs/internals/poll-tagged.md` — TaggedConn + PollConn (dnst's primary consumer).
- `docs/internals/stream-transforms.md` — split / frame / packet conns.
- `docs/internals/icmp.md` — ICMP transport (sibling to dnst-style tunnels).
- `docs/internals/modules-cli.md` — blank-import activation in cli mains.
- `docs/mux-tag-poll.md` — end-user guide (referenced from the constructor doc-comment in `dnst_conn.go`).
