# Drivers & Proto Modules

> Internals reference for the optional protocol-wrapper tiers of go-netx.
> Every claim is grounded in `file:line` using **module-relative** paths.
> Source files were read in full; ambiguities are explicitly flagged.

---

## Purpose & role

go-netx keeps its **root module dependency-light** by exiling protocol
implementations into two tiers of *separate* Go modules:

1. **`proto/*`** — heavier `net.Conn` / `TaggedConn` protocol implementations.
   Each is its own module so its third-party deps (miekg/dns, x/crypto) never
   pollute the root. Members: `proto/aesgcm`, `proto/dnst`, `proto/ssh`.
   - `proto/aesgcm/aesgcm_conn.go` and `proto/ssh/ssh_conn.go` use their own
     package names (`aesgcmproto`, `sshproto`).
   - `proto/dnst/dnst_conn.go:16` declares `package netx` (NOT a proto-suffixed
     name) — its tests live in the same package and reach into internals. Flag:
     the package identifier differs from the other two proto modules.

2. **`drivers/*`** — thin adapters. Each driver's `init()` calls
   `netx.Register(name, factory)` (root `driver.go:15`). Members: `drivers/aesgcm`,
   `drivers/dnst`, `drivers/dtls`, `drivers/dtlspsk`, `drivers/ssh`, `drivers/tls`,
   `drivers/tlspsk`, `drivers/utls`. A driver typically wires a `proto/*` type in
   (aesgcm/dnst/ssh) or wires an external library directly (dtls/dtlspsk via
   pion, tls via stdlib, tlspsk via raff/tls-*, utls via refraction-networking).

### Blank-import activation
Registration is a side effect of package `init()`. A driver only exists if its
package is imported (blank import in cli mains). `Register` **panics** on
duplicate name or nil driver (`driver.go:18-24`), so importing the same driver
twice into one binary is a hard crash.

### Server/client duality
Every factory has signature `func(params map[string]string, listener bool)
(netx.Wrapper, error)` (`driver.go:8`). The **same registered name** produces a
*different* `Wrapper` depending on `listener`:
- `listener == true`  → server side (cert+key for TLS/DTLS, host key for SSH,
  PSK identity hint, dnst `maxw`, etc.).
- `listener == false` → client side (SPKI pinning / servername, client auth,
  required identity for PSK, no `maxw`, etc.).

### How a Wrapper is consumed
`netx.Wrapper` (root `wrap.go:119-141`) is a struct of optional function fields,
one per `PipeType` transition (`ListenerToListener`, `ConnToConn`,
`DialerToDialer`, `ConnToTagged`, `TaggedToTagged`, …). The pipeline calls
`Apply` (`wrap.go:211-255`) which dispatches on the concrete runtime type and
invokes the matching field. **At most one field per input type should be set**
(`wrap.go:117`); `InputTypes`/`OutputFor` (`wrap.go:143-206`) report which
PipeTypes a driver supports. Two adapters convert a `ConnToConn` closure into
listener/dialer forms: `ConnWrapListener` (`wrap.go:327`) and `ConnWrapDialer`
(`wrap.go:332`); the listener form wraps each accepted conn and closes it on
wrap error (`wrap.go:313-324`).

---

## Per-driver reference

### aesgcm  (`drivers/aesgcm/aesgcm.go`)
- **Registered name:** `"aesgcm"` (`aesgcm.go:13`).
- **Params:** only `key` accepted; any other key → `unknown aesgcm parameter`
  (`aesgcm.go:26-28`).
  - `key` (**required, both sides**): hex-decoded (`aesgcm.go:19`); must be
    16/24/32 bytes else `invalid aesgcm key size` (`aesgcm.go:23-25`); empty/
    missing → `missing aesgcm key parameter` (`aesgcm.go:30-32`).
- **PipeTypes / Wrapper fields:** `ConnToConn`, `ListenerToListener` (via
  `ConnWrapListener`), `DialerToDialer` (via `ConnWrapDialer`)
  (`aesgcm.go:40-47`). Same fields server and client (symmetric).
- **Underlying proto:** `aesgcmproto.NewAESGCMConn(c, aeskey)`
  (`aesgcm.go:33-34` → `proto/aesgcm/aesgcm_conn.go:45`).
- **External deps:** `proto/aesgcm v1.1.0`, root `v1.4.0` (`drivers/aesgcm/go.mod`).

### dnst  (`drivers/dnst/dnst.go`)
- **Registered name:** `"dnst"` (`dnst.go:13`).
- **Params:** `domain`, `maxw`; else `unknown parameter` (`dnst.go:29-30`).
  - `domain` (**required, both sides**): plain string, no hex (`dnst.go:18-19`);
    missing/empty → `missing domain parameter` (`dnst.go:33-34`).
  - `maxw` (**server-only, optional**): `strconv.ParseUint(value,10,16)`
    (`dnst.go:24`); **rejected on client** with
    `max write parameter is only valid for listeners` (`dnst.go:21-23`); feeds
    `dnstproto.WithMaxWrite(uint16)` (`dnst.go:28`).
- **PipeTypes / Wrapper fields — asymmetric:**
  - **Server** (`dnst.go:36-47`): `ConnToTagged` → `dnstproto.NewServerConn`;
    `TaggedToTagged` → `dnstproto.NewTaggedServerConn`. Outputs a **TaggedConn**
    (consumed by tagged demux, see proto internals).
  - **Client** (`dnst.go:48-54`): `ConnToConn` → `dnstproto.NewClientConn`.
    Plain `net.Conn` only (no listener/dialer adapters).
- **Underlying proto:** `proto/dnst` (`dnst.go:42,45,53`).
- **External deps:** `proto/dnst v1.1.0`, `miekg/dns v1.1.72` (indirect via proto),
  root `v1.4.0` (`drivers/dnst/go.mod`).

### dtls  (`drivers/dtls/dtls.go`)
- **Registered name:** `"dtls"` (`dtls.go:19`).
- **Params:** `key`, `cert`, `servername`; else `unknown dtls parameter`
  (`dtls.go:38-39`).
  - `key` (**server-required, client-forbidden**): hex (`dtls.go:26`).
    Client passing `key` → `dtls client does not support key parameter`
    (`dtls.go:62-64`).
  - `cert` (**server-required; client-optional**): hex (`dtls.go:32`).
  - `servername` (**client-only useful**): sets `cfg.ServerName` (`dtls.go:37`).
- **Server requires** both `cert` AND `key` → else error (`dtls.go:43-45`);
  parsed with `tls.X509KeyPair` (`dtls.go:46`), set as `cfg.Certificates`.
- **Client requires** `servername` OR `cert` (`dtls.go:73-75`). If `cert` set →
  `InsecureSkipVerify = true` + `VerifyPeerCertificate = spkiVerifier(cert)`
  (SPKI pinning, `dtls.go:65-72`).
- **PipeTypes / Wrapper fields:**
  - **Server** (`dtls.go:51-60`): `ListenerToListener` (`dtls.NewListener` over
    `dtlsnet.PacketListenerFromListener`), `ConnToConn` (`dtls.Server`).
  - **Client** (`dtls.go:76-87`): `DialerToDialer` (via `ConnWrapDialer` +
    `dtls.Client`), `ConnToConn` (`dtls.Client`).
- **External deps:** `pion/dtls/v3 v3.1.2`, root `v1.4.0` (`drivers/dtls/go.mod`).
  No proto module.

### dtlspsk  (`drivers/dtlspsk/dtlspsk.go`)
- **Registered name:** `"dtlspsk"` (`dtlspsk.go:14`).
- **Params:** `key`, `identity`; else `unknown dtlspsk parameter`
  (`dtlspsk.go:27-28`).
  - `key` (**required, both sides**): hex PSK (`dtlspsk.go:21`); empty/missing →
    `missing dtlspsk key parameter` (`dtlspsk.go:31-32`).
  - `identity` (**client-required, server-optional**): plain string
    (`dtlspsk.go:25`); server uses it as `PSKIdentityHint` (`dtlspsk.go:41`).
    Client without identity → `dtlspsk client requires identity parameter`
    (`dtlspsk.go:34-35`).
- **Fixed config (`dtlspsk.go:37-44`):** `PSK` callback returns `psk`;
  `CipherSuites = {dtls.TLS_PSK_WITH_AES_128_GCM_SHA256}`;
  `InsecureSkipVerify = true` (PSK auth, no cert chain).
- **PipeTypes / Wrapper fields:**
  - **Server** (`dtlspsk.go:45-55`): `ListenerToListener`, `ConnToConn`.
  - **Client** (`dtlspsk.go:56-69`): `DialerToDialer`, `ConnToConn`.
- **External deps:** `pion/dtls/v3 v3.1.2`, root `v1.4.0` (`drivers/dtlspsk/go.mod`).

### ssh  (`drivers/ssh/ssh.go`)
- **Registered name:** `"ssh"` (`ssh.go:15`).
- **Params:** `pass`, `key`, `pub`; else `unknown ssh parameter` (`ssh.go:41-42`).
  - `pass` (**optional, both sides**): plain string (`ssh.go:21-22`).
  - `key` (hex PEM → `ssh.ParsePrivateKey`, `ssh.go:23-31`): on **server** this
    is the **host key** (**required**, `ssh.go:47-50`); on **client** it is the
    **private key** for pubkey auth (optional, `ssh.go:93-95`).
  - `pub` (hex authorized-key → `ssh.ParseAuthorizedKey`, `ssh.go:32-40`): on
    **server** the client's allowed pubkey (optional auth method,
    `ssh.go:51-58`); on **client** the **expected host key** for pinning
    (**required**, `ssh.go:84-92`).
- **Server constraints:** must have host `key` (`ssh.go:47-49`); must supply at
  least one auth method — `pub` (PublicKeyCallback, compares marshaled bytes,
  `ssh.go:52-57`) or `pass` (PasswordCallback, `ssh.go:60-65`) — else
  `ssh server requires pubkey or pass parameter` (`ssh.go:67-69`).
- **Client constraints:** `pub` required for `HostKeyCallback` pinning
  (`ssh.go:84-92`); must supply at least one auth — `key`→`ssh.PublicKeys`
  and/or `pass`→`ssh.Password` (`ssh.go:93-98`) — else
  `ssh client requires key or pass parameter` (`ssh.go:99-101`).
- **PipeTypes / Wrapper fields:**
  - **Server** (`ssh.go:70-81`): `ListenerToListener` (via `ConnWrapListener`),
    `ConnToConn` → `sshproto.NewServerConn`.
  - **Client** (`ssh.go:102-113`): `DialerToDialer` (via `ConnWrapDialer`),
    `ConnToConn` → `sshproto.NewClientConn`.
- **Underlying proto:** `proto/ssh` (`ssh.go:76,80,108,112`).
- **External deps:** `proto/ssh v1.1.0`, `golang.org/x/crypto v0.49.0`,
  root `v1.4.0` (`drivers/ssh/go.mod`).

### tls  (`drivers/tls/tls.go`)
- **Registered name:** `"tls"` (`tls.go:17`).
- **Fixed config:** `MinVersion = MaxVersion = tls.VersionTLS13` (`tls.go:20-21`)
  — **TLS 1.3 only**.
- **Params:** `key`, `cert`, `servername`; else `unknown tls parameter`
  (`tls.go:39-40`). Same shape and rules as dtls:
  - server requires `cert` AND `key` (`tls.go:44-46`, `X509KeyPair` `tls.go:47`);
  - client forbids `key` (`tls.go:63-65`);
  - client `cert` → `InsecureSkipVerify=true` + `spkiVerifier` (`tls.go:66-73`);
  - client requires `servername` OR `cert` (`tls.go:74-76`).
- **PipeTypes / Wrapper fields:**
  - **Server** (`tls.go:52-61`): `ListenerToListener` (`tls.NewListener`),
    `ConnToConn` (`tls.Server`).
  - **Client** (`tls.go:77-89`): `DialerToDialer` (via `ConnWrapDialer` +
    `tls.Client`), `ConnToConn` (`tls.Client`).
- **External deps:** stdlib `crypto/tls` only; root `v1.4.0` (`drivers/tls/go.mod`).

### tlspsk  (`drivers/tlspsk/tlspsk.go`)
- **Registered name:** `"tlspsk"` (`tlspsk.go:15`).
- **Params:** `key`, `identity`; else `unknown tlspsk parameter`
  (`tlspsk.go:28-29`).
  - `key` (**required, both sides**): hex PSK (`tlspsk.go:22`); empty/missing →
    `missing tlspsk key parameter` (`tlspsk.go:32-33`).
  - `identity` (**client-required, server-optional**): `tlspsk.go:26`; client
    without it → `tlspsk client requires identity parameter` (`tlspsk.go:35-36`).
- **Fixed config (`tlspsk.go:38-47`):** `MinVersion=MaxVersion=tls.VersionTLS12`
  (**TLS 1.2 only**); `Extra` PSKConfig supplies identity + key callbacks;
  `CipherSuites = {tlspks.TLS_PSK_WITH_AES_256_CBC_SHA}`;
  `InsecureSkipVerify = true`.
- **Server quirk:** sets a hardcoded ed25519 self-signed `dummyCert()` because
  raff/tls-psk requires server-side `Certificates` to be present
  (`tlspsk.go:49-50,80-100`). The dummy cert/key are embedded PEM literals.
- **PipeTypes / Wrapper fields:**
  - **Server** (`tlspsk.go:48-62`): `ListenerToListener` (via `ConnWrapListener`
    + `tlswithpks.Server`), `ConnToConn`.
  - **Client** (`tlspsk.go:63-76`): `DialerToDialer` (via `ConnWrapDialer` +
    `tlswithpks.Client`), `ConnToConn`.
- **External deps:** `raff/tls-ext v1.0.0`, `raff/tls-psk v1.0.0`, root `v1.4.0`
  (`drivers/tlspsk/go.mod`). Note: `tlswithpks` = `github.com/raff/tls-ext`,
  `tlspks` = `github.com/raff/tls-psk` (`tlspsk.go:10-11`).

### utls  (`drivers/utls/utls.go`)
- **Registered name:** `"utls"` (`utls.go:20`).
- **CLIENT-ONLY:** if `listener == true` → immediate error
  `utls is exclusive to clients, use tls for servers instead` (`utls.go:21-23`).
- **Fixed config:** `MinVersion=MaxVersion=tls.VersionTLS13` (`utls.go:26-27`).
- **Params:** `cert`, `servername`, `hello`; else `unknown utls parameter`
  (`utls.go:61-62`).
  - `cert` (optional): hex (`utls.go:34`); if set → `InsecureSkipVerify=true` +
    `spkiVerifier` (`utls.go:65-72`).
  - `servername` (optional): `cfg.ServerName` (`utls.go:39`).
  - Requires `servername` OR `cert` (`utls.go:73-75`).
  - `hello` (optional, default `HelloChrome_Auto` at `utls.go:29`): case-insens.
    map (`utls.go:40-60`): `chrome`→HelloChrome_Auto, `firefox`→HelloFirefox_Auto,
    `ios`→HelloIOS_Auto, `android`→HelloAndroid_11_OkHttp, `safari`→HelloSafari_Auto,
    `edge`→HelloEdge_Auto, `randomized`→HelloRandomizedALPN,
    `randomizednoalpn`→HelloRandomized; unknown → `unknown utls hello profile`.
- **PipeTypes / Wrapper fields:** `DialerToDialer` (via `ConnWrapDialer`),
  `ConnToConn` (`utls.go:80-89`). Both call `utls.UClient(c, cfg, id)` then
  **eagerly invoke `uc.Handshake()`** and return the handshake error
  (`utls.go:82-83, 87-88`) — unlike stdlib `tls.Client` which is lazy.
- **External deps:** `refraction-networking/utls v1.8.2` (pulls brotli,
  klauspost/compress), root `v1.4.0` (`drivers/utls/go.mod`).

---

## Param matrix

Encoding key: **hex** = `hex.DecodeString`; **str** = raw string;
**uint16** = `ParseUint(…,10,16)`. R = required, O = optional, ✗ = forbidden,
n/a = not applicable.

| driver   | param        | server | client | encoding | notes |
|----------|--------------|:------:|:------:|----------|-------|
| aesgcm   | key          | R      | R      | hex      | size must be 16/24/32 bytes |
| dnst     | domain       | R      | R      | str      | trailing `.` normalized in proto |
| dnst     | maxw         | O      | ✗      | uint16   | client → error "only valid for listeners" |
| dtls     | cert         | R      | O      | hex(PEM) | client cert ⇒ SPKI pin |
| dtls     | key          | R      | ✗      | hex(PEM) | client key ⇒ error |
| dtls     | servername   | O      | O*     | str      | client needs servername OR cert |
| dtlspsk  | key          | R      | R      | hex      | the PSK |
| dtlspsk  | identity     | O      | R      | str      | server uses as PSKIdentityHint |
| ssh      | key          | R      | O      | hex(PEM) | server=host key; client=priv key (auth) |
| ssh      | pub          | O      | R      | hex(authkey) | server=allowed client key; client=host key pin |
| ssh      | pass         | O      | O      | str      | server needs pub OR pass; client needs key OR pass |
| tls      | cert         | R      | O      | hex(PEM) | client cert ⇒ SPKI pin |
| tls      | key          | R      | ✗      | hex(PEM) | client key ⇒ error |
| tls      | servername   | O      | O*     | str      | client needs servername OR cert |
| tlspsk   | key          | R      | R      | hex      | the PSK |
| tlspsk   | identity     | O      | R      | str      | |
| utls     | cert         | n/a    | O      | hex(PEM) | server side rejected entirely |
| utls     | servername   | n/a    | O*     | str      | client needs servername OR cert |
| utls     | hello        | n/a    | O      | str      | default chrome |

`O*` = optional individually but at least one of {servername, cert} is required
on the client.

---

## Proto conn internals

### aesgcm — `proto/aesgcm/aesgcm_conn.go`
- **Wire framing (per datagram, packet-boundary preserving):**
  `[8-byte seq big-endian][GCM(ciphertext||tag)]` (header doc `aesgcm_conn.go:5-7`;
  Write `aesgcm_conn.go:151-180`; Read `aesgcm_conn.go:114-147`). The conn does
  **not** add its own length framing — it assumes the underlying conn preserves
  boundaries (`aesgcm_conn.go:4`; tests wrap in `netx.NewFrameConn`,
  `aesgcm_conn_test.go:21-22`).
- **Nonce derivation:** 12-byte IV; per-packet nonce = IV with its **last 8
  bytes XORed** with the 8-byte seq. Read XORs at `nonce[4+i]` (`aesgcm_conn.go:132-133`),
  Write at `nonce[4+i]` (`aesgcm_conn.go:164-165`). The seq is also the GCM AAD
  (`buf[:8]` passed as additional data, `aesgcm_conn.go:136,168`). seq is an
  atomic counter, `seq.Add(1)-1` starts at 0 (`aesgcm_conn.go:159`).
- **Passive IV handshake (on creation):** write IV is `crypto/rand` (`aesgcm_conn.go:70`);
  duplex exchange under a **5s deadline** (`aesgcm_conn.go:75-77`): a goroutine
  `io.ReadFull`s the peer's 12-byte IV (`aesgcm_conn.go:81-85`) while the
  current side writes its own (`aesgcm_conn.go:88-98`); deadline cleared after
  (`aesgcm_conn.go:77`). Both ends must construct simultaneously or the handshake
  blocks (tests build the pair concurrently, `aesgcm_conn_test.go:32-34`).
- **Size limits:** total packet must fit `netx.MaxPacketSize` (= 65535,
  root `packet.go:4`). Write rejects `len(p)+8+Overhead > MaxPacketSize`
  (`aesgcm_conn.go:152-154`); Read rejects `n == MaxPacketSize`
  (`aesgcm_conn.go:123-125`) and `n < 8+Overhead` (`aesgcm_conn.go:126-128`).
  Short read buffer → `io.ErrShortBuffer`, packet dropped (`aesgcm_conn.go:141-143`,
  test `aesgcm_conn_test.go:111-135`).
- **MaxWrite chaining:** if the *underlying* conn exposes `MaxWrite() uint16`
  (e.g. dnst), aesgcm subtracts its own `8+Overhead` header and re-exposes the
  remainder via its own `MaxWrite()` (`aesgcm_conn.go:64-69, 108-110`); too-small
  underlying MaxWrite → constructor error.

### dnst — `proto/dnst/dnst_conn.go`
- **Direction split:** client→server data rides in the DNS query **QNAME**;
  server→client data rides in the response **TXT** record (header doc
  `dnst_conn.go:3-9`). Single request/response per exchange; cannot distinguish
  clients without payload help (`dnst_conn.go:8-9`).
- **Encoding:** base32 **StdEncoding, no padding** (`dnst_conn.go:73,204,321`).
- **Client Write (`dnst_conn.go:370-391`):** base32-encode → `splitString63`
  into ≤63-byte DNS labels joined by `.` → append `domain + "."` → reject if
  full QNAME > 253 (`dnst_conn.go:373-376`) → build TXT question with random
  `dns.Id()`, RecursionDesired (`dnst_conn.go:378-381`).
- **Client Read (`dnst_conn.go:337-368`):** unpack DNS; empty Answer → `(0,nil)`;
  first answer must be `*dns.TXT` else `invalid dns response type`; join TXT
  strings, base32-decode.
- **Server ReadTagged (`dnst_conn.go:96-138`):** **silently skips** invalid DNS
  packets, no-question messages, wrong-domain queries, and bad-encoding labels
  (continues the loop, logs Debug) so port-53 noise doesn't kill the conn
  (`dnst_conn.go:93-95`). Domain match is suffix + case-insensitive
  (`dnst_conn.go:122`); label-separator dots are stripped before decode
  (`dnst_conn.go:128`). **The parsed `*dns.Msg` is returned as the tag**
  (`dnst_conn.go:115`).
- **Server WriteTagged (`dnst_conn.go:141-168`):** tag MUST be the `*dns.Msg`
  from the matching Read (`SetReply`); else `invalid context for dnst write`.
  Encoded payload is `splitString`-chunked to 255-byte TXT segments
  (`dnst_conn.go:152-157`).
- **TaggedConn variant (`NewTaggedServerConn`, `dnst_conn.go:199-305`):** wraps
  an underlying `netx.TaggedConn` (e.g. a Mux) instead of `net.Conn`. Its tag is
  a `serverConnTagged{dnsMsg, connTag}` (`dnst_conn.go:181-184, 248`) so the
  underlying routing tag (e.g. which TCP conn in a Mux) is carried end-to-end
  alongside the DNS message; WriteTagged forwards `ct.connTag` to the underlying
  `WriteTagged` (`dnst_conn.go:272-298`).
- **MaxWrite contract (consumed by `split` and tagged-demux):**
  - **Server `maxWrite` default = 765** (`dnst_conn.go:74,206`; doc 765 reasoning
    `dnst_conn.go:49-50`), overridable via `WithMaxWrite` / driver `maxw`
    (`dnst_conn.go:51-55`). Exposed by `serverConn.MaxWrite()` /
    `taggedServerConn.MaxWrite()` (`dnst_conn.go:90,221`).
  - **Client `maxWrite`** is **computed from domain length** by `maxQNAMEPayload`
    (`dnst_conn.go:323,393-411`) accounting for base32 expansion (5 raw → 8 chars)
    and 63-byte label splitting within the 253-char QNAME budget; exposed by
    `clientConn.MaxWrite()` (`dnst_conn.go:335`).
  - Both `serverConn`/`taggedServerConn` are `netx.TaggedConn`s (return type of
    the constructors, `dnst_conn.go:67,199`) → feed `NewTaggedDemux`, whose
    MaxWrite math subtracts the session id mask (`demux_tagged.go:44-48`).
    The client `net.Conn`'s MaxWrite is consumed by `split` (`split_conn.go:47-53`)
    and by `DemuxClient` (`demux_client.go:29-33`).
- **DNS constants:** `serverMaxRead = 512` read buffer (`dnst_conn.go:32`);
  TXT chunks 255 (`dnst_conn.go:156,286`); QNAME labels 63 (`dnst_conn.go:427`),
  QNAME total ≤253 (`dnst_conn.go:374,400`).
- **Address/deadline passthrough** to the underlying conn for both server
  variants (`dnst_conn.go:170-175, 300-305`).

### ssh — `proto/ssh/ssh_conn.go`
- **Channel model:** after the SSH handshake, exactly one **`direct-tcpip`**
  channel carries the byte stream; all SSH global/channel requests are discarded.
- **Server (`ssh_conn.go:23-47`):** `ssh.NewServerConn`; `go DiscardRequests`;
  iterate incoming channels — accept the first `direct-tcpip` (returns the
  `ssh.Channel` as the `net.Conn` body), reject any other channel type with
  `UnknownChannelType` and error out (`ssh_conn.go:30-42`); if the channel range
  ends with none opened → error (`ssh_conn.go:45-46`).
- **Client (`ssh_conn.go:49-62`):** `ssh.NewClientConn` then
  `OpenChannel("direct-tcpip", nil)` (extra data **nil** — no SOCKS-style
  host/port payload).
- **Conn shape:** embeds `ssh.Channel`; `Close` joins channel+SSH conn close
  (`ssh_conn.go:72-74`); `CloseWrite` forwards to channel and to the base conn if
  it supports CloseWrite (`ssh_conn.go:64-70`); `LocalAddr`/`RemoteAddr` from the
  SSH conn (`ssh_conn.go:76-77`); deadlines delegate to the **base** conn `bc`
  (`ssh_conn.go:78-80`) — the SSH channel itself has no deadline support.

---

## Security-critical patterns

These behaviors are load-bearing for confidentiality/integrity. Changing them
silently weakens security.

1. **SPKI pinning (tls / dtls / utls clients).** Identical `spkiVerifier` is
   copy-pasted in all three (`tls.go:93-115`, `dtls.go:92-114`, `utls.go:93-115`).
   When a client `cert` is supplied: `InsecureSkipVerify = true` is set **and**
   `VerifyPeerCertificate` pins on `sha256` of `RawSubjectPublicKeyInfo`
   (`tls.go:67-69` etc.). The pin compares
   `sha256.New().Sum(c.RawSubjectPublicKeyInfo)` for each peer cert
   (`tls.go:108`). **The two — InsecureSkipVerify + the verifier — are a unit;
   removing the verifier while keeping InsecureSkipVerify = no auth at all.**
   - Flag (potential bug, unchanged per instructions): the code uses
     `sha256.New().Sum(data)` which **appends** `data` to the (empty) running
     hash and returns `data || H("")`-style bytes rather than `Sum256(data)`.
     Because the **same** construction is used on both the pin and the peer
     comparison, pinning still works *self-consistently*, but the stored value is
     not a plain SHA-256 of the SPKI. Any "fix" to `sha256.Sum256` must change
     both sides together or pinning breaks.
2. **PSK cipher suites are pinned to a single suite each — interop-critical:**
   - tlspsk: `tlspks.TLS_PSK_WITH_AES_256_CBC_SHA`, TLS **1.2 only**
     (`tlspsk.go:38-46`).
   - dtlspsk: `dtls.TLS_PSK_WITH_AES_128_GCM_SHA256` (`dtlspsk.go:42`).
   Both set `InsecureSkipVerify = true` (no cert chain; auth is the PSK).
   Changing the suite or version breaks all existing peers.
3. **TLS version pinning:** tls/utls are **TLS 1.3 only** (Min==Max==1.3,
   `tls.go:20-21`, `utls.go:26-27`); tlspsk is **TLS 1.2 only**
   (`tlspsk.go:39-40`). dtls leaves version to pion defaults (not set in
   `dtls.go`).
4. **SSH host-key & peer pinning is mandatory and exact-match:**
   client `HostKeyCallback` compares `key.Marshal()` to the pinned `pub`
   (`ssh.go:87-92`); server `PublicKeyCallback` compares `key.Marshal()` to `pub`
   (`ssh.go:52-57`). Returning `nil` = accept. No CA/known_hosts fallback.
5. **aesgcm nonce uniqueness depends on (a) random per-conn IV and (b) the
   monotonic seq counter.** Reusing an (IV, seq) pair with the same key is a
   catastrophic GCM nonce-reuse. The seq is `atomic.Uint64` and never wraps in
   practice; the IV is freshly random per connection (`aesgcm_conn.go:70`).
   Sending the IV in cleartext during the handshake is intentional (only the key
   is secret).
6. **utls fingerprinting:** the client mimics a real browser/OS TLS ClientHello
   via uTLS `ClientHelloID` (default Chrome, `utls.go:29`). The handshake is
   forced eagerly (`utls.go:83,88`). Changing the default `hello` changes the
   network fingerprint (anti-censorship behavior).
7. **dnst server tolerates hostile noise by design** — it silently drops
   malformed/foreign DNS rather than erroring (`dnst_conn.go:93-95`). Do not
   convert these `continue`s into returns; doing so makes the server trivially
   DoS-able by any stray port-53 packet.
8. **tlspsk dummy cert** is a fixed, public, embedded ed25519 self-signed cert
   (`tlspsk.go:84-99`). It is NOT a secret and provides NO authentication —
   auth is the PSK. Do not treat it as a real server certificate.

---

## Cross-module dependencies

This repo is a **multi-module workspace** (`go.work` at root listing root +
proto/* + drivers/*). Each module pins its own deps; version coupling matters
for releases.

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

Coupling chains:
- **proto↔root:** `proto/aesgcm` and `proto/dnst` import the root module
  (MaxPacketSize, Logger, TaggedConn) and pin **root v1.4.0**. `proto/ssh` does
  **not** depend on root at all (pure x/crypto).
- **driver↔proto↔root:** aesgcm/dnst/ssh drivers pin both root **v1.4.0** and
  their proto **v1.1.0**. So bumping a root type used by a proto requires:
  bump root → re-tag proto against new root → bump driver's proto pin. Three
  hops.
- All Go versions are `go 1.25.7`. Shared transitive pins (x/sys v0.42.0,
  x/net v0.52.0, x/crypto v0.49.0, pion/transport/v3 v3.1.1) are consistent
  across modules; a mismatch would surface at `go work sync` / `go mod tidy`.

---

## Change hazards

Ordered roughly by blast radius / security weight.

1. **All secrets are hex-encoded; decode errors must surface.** Every `key`,
   `cert`, `pub` goes through `hex.DecodeString` and returns a wrapped error on
   failure (e.g. `aesgcm.go:19-22`, `tls.go:27-30`, `ssh.go:24-27`). Never
   swallow these — a silently-empty key produces a broken or insecure conn.
2. **Weakening cert verification is a security regression.** Do not detach
   `VerifyPeerCertificate` from `InsecureSkipVerify=true` (`tls.go:67-69`,
   `dtls.go:67-68`, `utls.go:67-68`); do not change one SPKI hash construction
   without the other (`spkiVerifier` symmetry, §Security #1).
3. **Changing aesgcm IV size, nonce-XOR offset, seq width, or framing breaks
   wire interop AND can void GCM security.** The 12-byte IV, last-8-byte XOR
   (`aesgcm_conn.go:132-133,164-165`), 8-byte big-endian seq header, and
   "no extra framing" assumption (`aesgcm_conn.go:4`) are all wire contract.
4. **Changing dnst encoding breaks interop.** base32 std/no-padding
   (`dnst_conn.go:73`), the `.`-stripping on decode (`dnst_conn.go:128`),
   63-byte label split, 255-byte TXT split, and 253 QNAME cap are a fixed
   protocol. The client/server `maxQNAMEPayload` math (`dnst_conn.go:393-411`)
   must stay consistent with the splitting or writes silently truncate/overflow.
5. **PSK cipher/version changes break interop.** `tlspsk` (1.2 +
   AES_256_CBC_SHA) and `dtlspsk` (AES_128_GCM_SHA256) suites are single-element
   lists (`tlspsk.go:45`, `dtlspsk.go:42`); both peers must agree exactly.
6. **Adding a param requires updating the `default:` unknown-param branch.**
   Each driver rejects unknown params in the `switch` default (e.g.
   `aesgcm.go:26-28`, `dnst.go:29-30`, `tls.go:39-40`, `utls.go:61-62`). A new
   param added only to the parse switch but not documented/validated will be
   accepted everywhere; one added without a case is rejected as "unknown". Also
   honor side-restrictions like dnst `maxw` server-only (`dnst.go:21-23`) and
   tls/dtls client `key` forbidden (`tls.go:63-64`, `dtls.go:62-63`).
7. **MaxWrite / TaggedConn contracts feed `split` and the demuxes.** dnst's
   `MaxWrite()` (server default 765, client computed) and TaggedConn nature are
   consumed by `NewSplitConn` (`split_conn.go:47-53`), `NewTaggedDemux`
   (`demux_tagged.go:44-48`), and `DemuxClient` (`demux_client.go:29-33`).
   aesgcm both *consumes* an underlying MaxWrite and *re-exposes* a reduced one
   (`aesgcm_conn.go:64-69,108-110`). Changing header sizes or default 765 shifts
   the usable payload for every downstream layer; returning 0 disables `split`
   entirely (it errors, `split_conn.go:48-49`).
8. **Blank-import / duplicate-registration crash.** `Register` panics on
   duplicate name (`driver.go:21-23`). Two drivers must never share a registered
   name, and the same driver must not be imported twice into one binary. The
   registered name string (`aesgcm.go:13`, etc.) is the public URI identifier —
   renaming it is a breaking config change.
9. **utls is client-only.** Any attempt to use it as a listener errors
   (`utls.go:21-23`); a server must use `tls`. Do not "helpfully" add a server
   branch — uTLS has no server hello-mimicry.
10. **SSH `direct-tcpip` assumption.** Server accepts only that channel type and
    rejects others (`ssh_conn.go:30-42`); client opens it with **nil** extra
    data. Changing the channel type or adding required extra data breaks both
    ends simultaneously. SSH conn deadlines delegate to the base conn, not the
    channel (`ssh_conn.go:78-80`).
11. **Cross-module version bump is a 3-hop dance** for aesgcm/dnst/ssh
    (root → proto → driver pins; see Cross-module deps). Forgetting to re-tag a
    proto module leaves drivers on a stale root.

---

## How to add a driver+proto safely

Grounded in the existing pattern:

1. **(If heavy/extra-deps) create `proto/<name>/` as its own module.** Implement
   `net.Conn` (or `TaggedConn` if the write path needs read-time context, like
   dnst). Mirror `proto/aesgcm` or `proto/dnst`. If you touch root types, pin the
   root version in its `go.mod`.
2. **Implement `MaxWrite() uint16` if the protocol caps payload size** so
   `split` / demux layers compose (pattern: `dnst_conn.go:90`,
   `aesgcm_conn.go:108`). Return 0 if uncapped.
3. **Create `drivers/<name>/` module with an `init()` calling
   `netx.Register("<name>", factory)`** (pattern: `aesgcm.go:12-13`). The name
   becomes the public URI keyword — choose it permanently.
4. **In the factory: loop params in a `switch` with a `default:` that returns
   `unknown <name> parameter`** (e.g. `tls.go:39-40`). Hex-decode any
   keys/certs/passwords and **return wrapped errors on decode failure**
   (`tls.go:27-30`).
5. **Branch on `listener`.** Validate required-on-each-side params and reject
   forbidden ones (server cert+key required `tls.go:44-46`; client `key`
   forbidden `tls.go:63-64`; client requires servername-or-cert `tls.go:74-76`).
6. **Set exactly the Wrapper fields for the PipeTypes you support.** Use
   `ConnWrapListener` / `ConnWrapDialer` to lift a `ConnToConn` closure into
   listener/dialer forms (pattern `aesgcm.go:40-46`). Set `ConnToTagged` /
   `TaggedToTagged` only if you produce a TaggedConn (pattern `dnst.go:41-46`).
   Keep at most one field per input type (`wrap.go:117`).
7. **Always set `Name`, `Params: params`, `Listener: listener`** on the returned
   Wrapper (every driver does; used by `String()`/`MarshalText`,
   `wrap.go:257-270`).
8. **Add the driver's blank import to the cli mains** that should expose it (see
   modules-cli.md) and add its `go.mod` requires (root + proto + external).
   Verify `go work sync` succeeds.
9. **Document the cipher/version/fingerprint choices** if security-relevant and
   treat them as wire contract going forward (§Security-critical patterns).

---

## Related docs
- `docs/internals/pipeline.md` — how Wrappers are composed into a layer pipeline.
- `docs/internals/demux.md` — id-prefixed session demultiplexing (consumes MaxWrite).
- `docs/internals/poll-tagged.md` — TaggedConn + PollConn (dnst's primary consumer).
- `docs/internals/stream-transforms.md` — split / frame / packet conns.
- `docs/internals/icmp.md` — ICMP transport (sibling to dnst-style tunnels).
- `docs/internals/modules-cli.md` — blank-import activation in cli mains.
- `docs/mux-tag-poll.md` — end-user guide referenced from `dnst_conn.go:65-66`.
