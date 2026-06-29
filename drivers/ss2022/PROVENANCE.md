# PROVENANCE — drivers/ss2022

**Clean-room attestation:** Original go-netx glue registering the Shadowsocks-2022
codec (`proto/ss2022`) into the netx Driver/Wrapper registry. **No GPL/AGPL/MPL proxy
source was read, fetched, quoted, paraphrased, or AI-ported.**

## What this implements
`netx.Register("ss2022", …)` mapping URI params (`method`, `psk`, `target`) to the
client (`DialerToDialer`/`ConnToConn`) and server (`ListenerToListener` via
`ConnWrapListener`) wrapper forms. The `psk` is URL-safe base64 (the scheme grammar
reserves `+`, `=`, `{`, `}`, so standard base64 is unsafe in a URI).

## Protocol provenance
See `proto/ss2022/PROVENANCE.md` — the wire codec derives from the public SIP022 spec,
RFCs, and the BLAKE3 reference.

## Dependencies
`go-netx` core (BSD-3) + `proto/ss2022` + `proto/socksaddr` (pulls `lukechampine.com/blake3`
MIT and `klauspost/cpuid/v2` BSD-3). No copyleft in the graph.
