/*
Package prepend provides a net.Conn decorator that emits a fixed header,
coalesced with the first Write's payload in a single underlying Write, and is
byte-transparent thereafter. It is the core Track-A primitive for proxy carriers
whose request header is written once at the start of the stream then followed by
the raw tunnelled payload (e.g. Trojan).

Coalescing matters: sending the header in its own Write would create a distinct
small packet on the wire (a fingerprint) and an extra syscall. By merging the
header with the first payload write, the first application datagram and the
protocol header leave as one record — matching how real clients behave.

This package is original go-netx code; it implements no third-party protocol and
consults no GPL source.
*/
package prepend

import (
	"io"
	"net"
	"sync"
)

// Conn writes a header before the first payload, then is transparent.
type Conn struct {
	net.Conn
	mu     sync.Mutex
	header []byte
	sent   bool
}

// New returns a *Conn that, on its first Write, emits header coalesced with the
// payload in a single underlying Write. header is retained by reference; callers
// must not mutate it after the call.
func New(c net.Conn, header []byte) *Conn {
	return &Conn{Conn: c, header: header}
}

// Write emits header||p in one underlying Write on the first call, then writes p
// transparently. The returned count is always in terms of the caller's payload p
// and never includes the header bytes, so callers (e.g. the WireGuard relay) get
// correct accounting even on a short underlying write.
func (c *Conn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.sent {
		return c.Conn.Write(p)
	}
	c.sent = true

	buf := make([]byte, 0, len(c.header)+len(p))
	buf = append(buf, c.header...)
	buf = append(buf, p...)
	hlen := len(c.header)
	c.header = nil

	written, err := c.Conn.Write(buf)
	// Translate the underlying byte count back to caller-payload accounting.
	if written <= hlen {
		// No payload bytes reached the wire.
		if err == nil {
			err = io.ErrShortWrite
		}
		return 0, err
	}
	return written - hlen, err
}

// FlushHeader forces the header out before any payload, for protocols whose peer
// must read the header before the first payload byte exists (read-first servers).
// After FlushHeader, Write is fully transparent.
func (c *Conn) FlushHeader() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.sent {
		return nil
	}
	c.sent = true
	h := c.header
	c.header = nil
	for len(h) > 0 {
		n, err := c.Conn.Write(h)
		if err != nil {
			return err
		}
		h = h[n:]
	}
	return nil
}
