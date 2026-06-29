# PROVENANCE — drivers/trojan

**Clean-room attestation:** This driver is original go-netx glue code that registers
the Trojan codec (`proto/trojan`) into the netx Driver/Wrapper registry. **No
GPL/AGPL/MPL proxy source was read, fetched, quoted, paraphrased, or AI-ported.**

## What this implements
The `netx.Register("trojan", …)` block mapping URI params (`password`, `target`,
`fallback`) to the client (`DialerToDialer`/`ConnToConn`) and server
(`ListenerToListener`) wrapper forms, exactly mirroring the existing driver idiom
(`drivers/aesgcm`, `drivers/tls`).

## Protocol provenance
See `proto/trojan/PROVENANCE.md` — the wire codec derives solely from the public
trojan-gfw protocol page, RFC 1928, and FIPS 180-4.

## Dependencies
`go-netx` core (BSD-3-Clause) + `proto/trojan` (which pulls `proto/prepend` and
`proto/socksaddr`). No external modules; no copyleft in the graph.
