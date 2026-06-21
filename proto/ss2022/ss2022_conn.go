package ss2022proto

import (
	"bufio"
	"crypto/cipher"
	"crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pedramktb/go-netx/proto/socksaddr"
)

var (
	ErrBadHeader = errors.New("ss2022: bad header")
	ErrReplay    = errors.New("ss2022: replay or stale timestamp")
	ErrSaltEcho  = errors.New("ss2022: response salt does not match request")
)

// poolBufSize holds the largest record we seal in one Write: a sealed length
// chunk (2+tag) plus a sealed max payload chunk (maxChunkSize+tag).
const poolBufSize = maxChunkSize + 2 + 2*tagSize

type ssConn struct {
	net.Conn
	method   Method
	psk      []byte
	isClient bool
	replay   *ReplayGuard // server only
	now      func() time.Time

	targetHost string // client only
	targetPort uint16

	// write side (single writer goroutine)
	wMu    sync.Mutex
	wAEAD  cipher.AEAD
	wNonce []byte
	wInit  bool

	// read side (single reader goroutine)
	rMu    sync.Mutex
	br     *bufio.Reader
	rAEAD  cipher.AEAD
	rNonce []byte
	rInit  bool
	carry  []byte // decrypted payload not yet returned
	lenBuf []byte // reused 2-byte length-chunk plaintext

	// cross-goroutine salt coupling (visibility via atomic)
	reqSalt   atomic.Pointer[[]byte] // client: own req salt; server: peer req salt
	recovered atomic.Pointer[string] // server: recovered target

	pool *sync.Pool
}

func newConn(c net.Conn, m Method, psk []byte, isClient bool, replay *ReplayGuard) *ssConn {
	return &ssConn{
		Conn: c, method: m, psk: psk, isClient: isClient, replay: replay,
		now:    time.Now,
		br:     bufio.NewReader(c),
		lenBuf: make([]byte, 2),
		pool: &sync.Pool{New: func() any {
			b := make([]byte, poolBufSize)
			return &b
		}},
	}
}

// NewClientConn wraps c as a SIP022 client carrying a fixed target.
func NewClientConn(c net.Conn, m Method, psk []byte, targetHost string, targetPort uint16) (net.Conn, error) {
	if err := ValidatePSK(m, psk); err != nil {
		return nil, err
	}
	sc := newConn(c, m, psk, true, nil)
	sc.targetHost, sc.targetPort = targetHost, targetPort
	return sc, nil
}

// NewServerConn wraps c as a SIP022 server. replay is the shared replay guard.
func NewServerConn(c net.Conn, m Method, psk []byte, replay *ReplayGuard) (net.Conn, error) {
	if err := ValidatePSK(m, psk); err != nil {
		return nil, err
	}
	return newConn(c, m, psk, false, replay), nil
}

// Target returns the recovered destination (server side) once the request header
// has been read, else "".
func (c *ssConn) Target() string {
	if p := c.recovered.Load(); p != nil {
		return *p
	}
	return ""
}

func (c *ssConn) seal(dst, plaintext []byte) []byte {
	dst = c.wAEAD.Seal(dst, c.wNonce, plaintext, nil)
	incNonce(c.wNonce)
	return dst
}

func (c *ssConn) openInto(dst, ciphertext []byte) ([]byte, error) {
	out, err := c.rAEAD.Open(dst, c.rNonce, ciphertext, nil)
	if err != nil {
		return nil, err
	}
	incNonce(c.rNonce)
	return out, nil
}

func randInt(max int) int {
	if max <= 0 {
		return 0
	}
	var b [2]byte
	_, _ = io.ReadFull(rand.Reader, b[:])
	return int(binary.BigEndian.Uint16(b[:])) % max
}

// ---- write path ----

func (c *ssConn) Write(p []byte) (int, error) {
	c.wMu.Lock()
	defer c.wMu.Unlock()
	if !c.wInit {
		if err := c.writeFirst(p); err != nil {
			return 0, err
		}
		c.wInit = true
		return len(p), nil
	}
	if err := c.writeChunks(p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *ssConn) writeFirst(p []byte) error {
	salt := make([]byte, c.method.KeySize)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return err
	}
	aead, err := c.method.newAEAD(deriveSubkey(c.psk, salt, c.method.KeySize))
	if err != nil {
		return err
	}
	c.wAEAD = aead
	c.wNonce = make([]byte, nonceSize)

	out := make([]byte, 0, len(salt)+reqFixedLen+tagSize+512+len(p)+tagSize)
	out = append(out, salt...)

	if c.isClient {
		sb := append([]byte(nil), salt...)
		c.reqSalt.Store(&sb) // publish before the network write (happens-before)

		initial := p
		var rest []byte
		if len(initial) > maxChunkSize {
			initial, rest = p[:maxChunkSize], p[maxChunkSize:]
		}
		varHdr := make([]byte, 0, 1+1+len(c.targetHost)+2+2+maxPadding+len(initial))
		if varHdr, err = socksaddr.Encode(varHdr, c.targetHost, c.targetPort); err != nil {
			return err
		}
		padLen := 0
		if len(initial) == 0 {
			padLen = 1 + randInt(maxPadding) // [1,900]; required when no initial payload
		}
		varHdr = binary.BigEndian.AppendUint16(varHdr, uint16(padLen))
		if padLen > 0 {
			varHdr = append(varHdr, make([]byte, padLen)...)
		}
		varHdr = append(varHdr, initial...)

		fixed := make([]byte, 0, reqFixedLen)
		fixed = append(fixed, hdrTypeRequest)
		fixed = binary.BigEndian.AppendUint64(fixed, uint64(c.now().Unix()))
		fixed = binary.BigEndian.AppendUint16(fixed, uint16(len(varHdr)))

		out = c.seal(out, fixed)
		out = c.seal(out, varHdr)
		if _, err := c.Conn.Write(out); err != nil {
			return err
		}
		if len(rest) > 0 {
			return c.writeChunks(rest)
		}
		return nil
	}

	// server response: type | ts | requestSalt | firstLen, then first payload.
	reqSaltP := c.reqSalt.Load()
	if reqSaltP == nil {
		return ErrBadHeader // request must be read before a response is written
	}
	reqSalt := *reqSaltP
	fixed := make([]byte, 0, 1+8+len(reqSalt)+2)
	fixed = append(fixed, hdrTypeResponse)
	fixed = binary.BigEndian.AppendUint64(fixed, uint64(c.now().Unix()))
	fixed = append(fixed, reqSalt...)
	fixed = binary.BigEndian.AppendUint16(fixed, uint16(len(p)))
	out = c.seal(out, fixed)
	out = c.seal(out, p)
	_, err = c.Conn.Write(out)
	return err
}

func (c *ssConn) writeChunks(p []byte) error {
	for len(p) > 0 {
		n := len(p)
		if n > maxChunkSize {
			n = maxChunkSize
		}
		var lb [2]byte // local: lenBuf is the read goroutine's field
		binary.BigEndian.PutUint16(lb[:], uint16(n))
		bp := c.pool.Get().(*[]byte)
		buf := (*bp)[:0]
		buf = c.seal(buf, lb[:])
		buf = c.seal(buf, p[:n])
		_, err := c.Conn.Write(buf)
		c.pool.Put(bp)
		if err != nil {
			return err
		}
		p = p[n:]
	}
	return nil
}

// ---- read path ----

func (c *ssConn) Read(p []byte) (int, error) {
	c.rMu.Lock()
	defer c.rMu.Unlock()
	for len(c.carry) == 0 {
		if !c.rInit {
			if err := c.readFirst(); err != nil {
				return 0, err
			}
			c.rInit = true
		} else {
			if err := c.readChunk(); err != nil {
				return 0, err
			}
		}
	}
	n := copy(p, c.carry)
	c.carry = c.carry[n:]
	return n, nil
}

// readOpenAlloc reads and decrypts one record into a freshly allocated slice
// (used for the one-time handshake headers).
func (c *ssConn) readOpenAlloc(plainLen int) ([]byte, error) {
	ctLen := plainLen + tagSize
	bp := c.pool.Get().(*[]byte)
	buf := *bp
	if ctLen > cap(buf) {
		buf = make([]byte, ctLen)
	}
	if _, err := io.ReadFull(c.br, buf[:ctLen]); err != nil {
		c.pool.Put(bp)
		return nil, err
	}
	out, err := c.openInto(make([]byte, 0, plainLen), buf[:ctLen])
	c.pool.Put(bp)
	return out, err
}

// readChunkInto reads and decrypts one record into dst (reused), using a pooled
// ciphertext scratch — zero steady-state allocation.
func (c *ssConn) readChunkInto(plainLen int, dst *[]byte) error {
	ctLen := plainLen + tagSize
	bp := c.pool.Get().(*[]byte)
	buf := *bp
	if ctLen > cap(buf) {
		buf = make([]byte, ctLen)
	}
	if _, err := io.ReadFull(c.br, buf[:ctLen]); err != nil {
		c.pool.Put(bp)
		return err
	}
	out, err := c.openInto((*dst)[:0], buf[:ctLen])
	c.pool.Put(bp)
	if err != nil {
		return err
	}
	*dst = out
	return nil
}

func (c *ssConn) readFirst() error {
	salt := make([]byte, c.method.KeySize)
	if _, err := io.ReadFull(c.br, salt); err != nil {
		return err
	}
	aead, err := c.method.newAEAD(deriveSubkey(c.psk, salt, c.method.KeySize))
	if err != nil {
		return err
	}
	c.rAEAD = aead
	c.rNonce = make([]byte, nonceSize)

	if c.isClient {
		fixedLen := 1 + 8 + c.method.KeySize + 2
		hdr, err := c.readOpenAlloc(fixedLen)
		if err != nil {
			return err
		}
		if hdr[0] != hdrTypeResponse {
			return ErrBadHeader
		}
		reqSaltP := c.reqSalt.Load()
		if reqSaltP == nil {
			return ErrBadHeader
		}
		echo := hdr[1+8 : 1+8+c.method.KeySize]
		if subtle.ConstantTimeCompare(echo, *reqSaltP) != 1 {
			return ErrSaltEcho
		}
		firstLen := int(binary.BigEndian.Uint16(hdr[1+8+c.method.KeySize:]))
		if firstLen == 0 || firstLen > maxChunkSize {
			return ErrBadHeader
		}
		return c.readChunkInto(firstLen, &c.carry)
	}

	// server: fixed request header + variable header
	fixed, err := c.readOpenAlloc(reqFixedLen)
	if err != nil {
		return err
	}
	if fixed[0] != hdrTypeRequest {
		return ErrBadHeader
	}
	if !c.replay.checkTime(binary.BigEndian.Uint64(fixed[1:9])) {
		return ErrReplay
	}
	varLen := int(binary.BigEndian.Uint16(fixed[9:11]))
	varHdr, err := c.readOpenAlloc(varLen)
	if err != nil {
		return err
	}
	host, port, n, err := socksaddr.Decode(varHdr)
	if err != nil {
		return err
	}
	rest := varHdr[n:]
	if len(rest) < 2 {
		return ErrBadHeader
	}
	padLen := int(binary.BigEndian.Uint16(rest[:2]))
	rest = rest[2:]
	if padLen > maxPadding || padLen > len(rest) {
		return ErrBadHeader
	}
	initial := rest[padLen:]
	if len(initial) == 0 && padLen == 0 {
		return ErrBadHeader // SIP022: payload or padding MUST be present
	}
	if !c.replay.checkSalt(salt) {
		return ErrReplay
	}
	target := net.JoinHostPort(host, strconv.Itoa(int(port)))
	c.recovered.Store(&target)
	sb := append([]byte(nil), salt...)
	c.reqSalt.Store(&sb) // for the response salt echo
	c.carry = append(c.carry[:0], initial...)
	return nil
}

func (c *ssConn) readChunk() error {
	if err := c.readChunkInto(2, &c.lenBuf); err != nil {
		return err
	}
	n := int(binary.BigEndian.Uint16(c.lenBuf))
	if n == 0 || n > maxChunkSize {
		return ErrBadHeader
	}
	return c.readChunkInto(n, &c.carry)
}
