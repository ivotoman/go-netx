# PROVENANCE — proto/ss2022

**Clean-room attestation:** Implemented solely from the public specifications below.
**No GPL/AGPL/MPL source (mihomo, sing-box, xray-core, go-shadowsocks2, etc.) was
read, fetched, quoted, paraphrased, or AI-ported.**

## Specification sources
- Shadowsocks-2022 (SIP022) edition spec:
  https://github.com/Shadowsocks-NET/shadowsocks-specs/blob/main/2022-1-shadowsocks-2022-edition.md
- shadowsocks.org SIP022 page.
- RFC 5116 (AEAD interface), RFC 8439 (ChaCha20-Poly1305 — for the deferred ChaCha
  method), FIPS 197 / SP 800-38D (AES-GCM).
- BLAKE3 reference (derive_key mode) — implemented via `lukechampine.com/blake3`
  v1.4.1 (MIT), `DeriveKey(subKey, context, srcKey)`.

## What this implements (Stage 1 scope)
The SIP022 **TCP** codec for `2022-blake3-aes-128-gcm` and `2022-blake3-aes-256-gcm`:
per-session salt, BLAKE3 `derive_key("shadowsocks 2022 session subkey", PSK||salt)`,
12-byte little-endian per-direction nonces advanced once per AEAD op, the request
(type/timestamp/varlen + ATYP·ADDR·PORT/padding/payload) and response
(type/timestamp/requestSalt/firstLen + payload) headers, the length+payload chunk
stream (≤0xFFFF), the ±30s timestamp window, and a 60s salt replay cache.
**Deferred:** UDP packet codec, ChaCha methods, EIH multi-user.

## Clean-room hazards reconciled / outstanding
- The BLAKE3 `derive_key` context string and `PSK||salt` material order are pinned by
  the spec and asserted by `TestDeriveSubkeyDeterministicAndKeyed`; the exact derived
  bytes must additionally be validated against a controlled reference server before
  any interop claim (the unit test proves consistency, not external conformance).
- The UDP separate-header nonce slice and ChaCha field order remain spec items to
  reconcile against a reference server when the deferred UDP/ChaCha paths land.

## Dependencies
`proto/socksaddr` (first-party, BSD-3) + `lukechampine.com/blake3` (MIT) +
`github.com/klauspost/cpuid/v2` (BSD-3, transitive). No copyleft in the graph.
