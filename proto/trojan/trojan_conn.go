/*
Package trojanproto implements the Trojan proxy protocol's connection codec
(trojan-gfw). Trojan carries a SOCKS5-style target request over an
already-established TLS stream:

	hex(SHA224(password))[56] || CRLF || CMD(1) || ATYP/ADDR/PORT || CRLF || payload

The client prepends this request (coalesced with the first payload) once, then the
stream is byte-transparent. The server validates the password hash in constant
time and, on any failure, splices the connection to a plaintext fallback origin so
an active prober observes a real site rather than an abnormal reset — Trojan's core
censorship-resistance property.

Clean-room: implemented from the public trojan-gfw protocol page, RFC 1928 (SOCKS5
address), and FIPS 180-4 (SHA-224). No GPL source consulted. See PROVENANCE.md.
*/
package trojanproto

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"strconv"
	"time"

	"github.com/pedramktb/go-netx/proto/prepend"
	"github.com/pedramktb/go-netx/proto/socksaddr"
)

const (
	cmdConnect      byte = 0x01
	cmdUDPAssociate byte = 0x03
	hashHexLen           = 56 // hex(SHA224) = 28 bytes -> 56 ASCII chars

	// DefaultHandshakeTimeout bounds the server-side request read so a client that
	// completes TLS then stalls cannot pin an accept slot (slowloris).
	DefaultHandshakeTimeout = 10 * time.Second
)

var crlf = [2]byte{0x0D, 0x0A}

var (
	ErrAuth      = errors.New("trojan: authentication failed")
	ErrBadFormat = errors.New("trojan: malformed request")
)

// HashPassword returns the 56-byte lowercase-hex SHA-224 of password — the Trojan
// per-connection authenticator.
func HashPassword(password string) []byte {
	sum := sha256.Sum224([]byte(password))
	out := make([]byte, hex.EncodedLen(len(sum)))
	hex.Encode(out, sum[:])
	return out
}

// NewClientConn wraps an established (TLS) conn so the Trojan CONNECT request for
// the fixed target ("host:port") is emitted, coalesced with the first payload, on
// the first Write. The conn is byte-transparent thereafter.
func NewClientConn(c net.Conn, password, target string) (net.Conn, error) {
	host, port, err := socksaddr.SplitHostPort(target)
	if err != nil {
		return nil, err
	}
	hdr := make([]byte, 0, hashHexLen+2+1+1+len(host)+2+2)
	hdr = append(hdr, HashPassword(password)...)
	hdr = append(hdr, crlf[:]...)
	hdr = append(hdr, cmdConnect)
	if hdr, err = socksaddr.Encode(hdr, host, port); err != nil {
		return nil, err
	}
	hdr = append(hdr, crlf[:]...)
	return prepend.New(c, hdr), nil
}

// recordReader records every byte read so a rejected request can be replayed to
// the fallback origin verbatim.
type recordReader struct {
	r   io.Reader
	buf bytes.Buffer
}

func (rr *recordReader) Read(p []byte) (int, error) {
	n, err := rr.r.Read(p)
	if n > 0 {
		rr.buf.Write(p[:n])
	}
	return n, err
}

// ReadRequest validates the Trojan request header on c (which must already be past
// TLS) and returns the recovered "host:port" target. On failure it returns an error
// together with every byte consumed so far, so the caller can splice the connection
// to a fallback. A read deadline (timeout) guards against slowloris and is cleared
// on success.
func ReadRequest(c net.Conn, valid [][]byte, timeout time.Duration) (target string, recorded []byte, err error) {
	if timeout > 0 {
		_ = c.SetReadDeadline(time.Now().Add(timeout))
	}
	rr := &recordReader{r: c}
	fail := func(e error) (string, []byte, error) { return "", rr.buf.Bytes(), e }

	hash := make([]byte, hashHexLen)
	if _, err = io.ReadFull(rr, hash); err != nil {
		return fail(err)
	}
	if !matchHash(valid, hash) {
		return fail(ErrAuth)
	}
	var sep [2]byte
	if _, err = io.ReadFull(rr, sep[:]); err != nil {
		return fail(err)
	}
	if sep != crlf {
		return fail(ErrBadFormat)
	}
	var cmd [1]byte
	if _, err = io.ReadFull(rr, cmd[:]); err != nil {
		return fail(err)
	}
	if cmd[0] != cmdConnect && cmd[0] != cmdUDPAssociate {
		return fail(ErrBadFormat)
	}
	host, port, err := socksaddr.Read(rr)
	if err != nil {
		return fail(err)
	}
	if _, err = io.ReadFull(rr, sep[:]); err != nil {
		return fail(err)
	}
	if sep != crlf {
		return fail(ErrBadFormat)
	}
	if timeout > 0 {
		_ = c.SetReadDeadline(time.Time{})
	}
	return net.JoinHostPort(host, strconv.Itoa(int(port))), nil, nil
}

// matchHash reports whether got matches any entry in valid, in time independent of
// WHICH entry matches (no early return) — only the set size, which is not secret,
// affects timing.
func matchHash(valid [][]byte, got []byte) bool {
	var ok int
	for _, want := range valid {
		if len(want) == len(got) {
			ok |= subtle.ConstantTimeCompare(want, got)
		}
	}
	return ok == 1
}

// SpliceFallback proxies a rejected connection to a plaintext fallback origin
// (e.g. 127.0.0.1:80). recorded (the bytes already consumed during validation) is
// replayed first; then the connection is bridged bidirectionally. It takes
// ownership of c and closes both ends when either direction finishes. This makes a
// probing connection indistinguishable from one to a real origin behind the TLS.
func SpliceFallback(c net.Conn, fallback string, recorded []byte) {
	_ = c.SetReadDeadline(time.Time{})
	up, err := net.DialTimeout("tcp", fallback, 5*time.Second)
	if err != nil {
		_ = c.Close()
		return
	}
	if len(recorded) > 0 {
		if _, err := up.Write(recorded); err != nil {
			_ = c.Close()
			_ = up.Close()
			return
		}
	}
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, c); done <- struct{}{} }()
	go func() { _, _ = io.Copy(c, up); done <- struct{}{} }()
	<-done
	// Closing both unblocks the still-running copy direction; then drain it.
	_ = c.Close()
	_ = up.Close()
	<-done
}

// serverConn is the de-Trojaned server-side connection. The request header has
// already been stripped; Read/Write are transparent (the Trojan response has no
// header). Target reports the recovered destination for the routing/relay layer.
type serverConn struct {
	net.Conn
	target string
}

// Target returns the recovered "host:port" destination from the Trojan request.
func (c *serverConn) Target() string { return c.target }
