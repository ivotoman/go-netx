# PROVENANCE — proto/socksaddr

**Clean-room attestation:** This package was implemented solely from the public
specification below. **No GPL/AGPL/MPL source (mihomo, sing-box, xray-core,
v2ray-core, or any proxy implementation) was read, fetched, quoted, paraphrased,
or AI-ported.**

## Specification sources
- RFC 1928 — *SOCKS Protocol Version 5*, §4 "Requests" (the ATYP / address /
  port wire encoding). https://www.rfc-editor.org/rfc/rfc1928

## What this implements
The SOCKS5 address triple `ATYP(1) || ADDR(var) || PORT(2, big-endian)` with
ATYP ∈ {0x01 IPv4(4B), 0x03 domain(1 len byte + N), 0x04 IPv6(16B)}. This is the
shared target-address codec reused by the Trojan and Shadowsocks-2022 request
headers (both define their target field by reference to the SOCKS5 address
format).

## Dependencies
Go standard library only (`encoding/binary`, `errors`, `io`, `net`, `strconv`).
No external modules. License of this package: BSD-3-Clause (inherits go-netx).

## Validation
Unit tests pin exact wire bytes (KAT) for IPv4/domain/IPv6 and round-trip
Encode→Decode→Read. Interop is validated transitively by the Trojan and
Shadowsocks-2022 round-trip e2e tests against a reference server we control.
