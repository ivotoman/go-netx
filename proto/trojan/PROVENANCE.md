# PROVENANCE — proto/trojan

**Clean-room attestation:** Implemented solely from the public specifications
below. **No GPL/AGPL/MPL source (mihomo, sing-box, xray-core, the original trojan
or trojan-go implementations, etc.) was read, fetched, quoted, paraphrased, or
AI-ported.**

## Specification sources
- Trojan protocol — *trojan-gfw* protocol documentation:
  https://trojan-gfw.github.io/trojan/protocol — the request format
  `hex(SHA224(password)) CRLF CMD ATYP·ADDR·PORT CRLF payload`, the UDP-associate
  framing, and the mandatory fallback-on-bad-auth anti-probe behavior.
- RFC 1928 (SOCKS5) — the ATYP/ADDR/PORT address triple (via proto/socksaddr).
- FIPS 180-4 — SHA-224 (the password authenticator), implemented with the Go
  standard library `crypto/sha256.Sum224`.

## What this implements
- Client request codec (prepend the request header, coalesced with the first
  payload write; byte-transparent thereafter).
- Server request validation with a constant-time password-hash match over the
  accepted set (no early return → match position not timing-leaked) and a read
  deadline (slowloris guard).
- The mandatory fallback splice: on auth/format failure the connection is bridged
  to a plaintext fallback origin, replaying the bytes already consumed, so an
  active prober observes a real site instead of an abnormal reset.

## Dependencies
Go standard library + `proto/prepend` + `proto/socksaddr` (both BSD-3-Clause,
first-party). No external modules.

## Clean-room hazards reconciled
None outstanding for Trojan: the wire format is fully specified publicly and is
byte-pinned by unit KATs (`TestHashPassword`, `TestClientHeaderBytes`) and a
round-trip e2e test against our own server codec.
