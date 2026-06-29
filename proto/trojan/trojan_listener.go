package trojanproto

import (
	"errors"
	"net"
	"sync"
	"time"
)

// serverListener performs the Trojan request validation asynchronously so a slow
// or probing client never blocks the accept loop, and so a rejected client can be
// spliced to the fallback origin without ever surfacing to the caller. Only
// successfully-authenticated, de-Trojaned connections are returned from Accept.
//
// This is a server-side construct (the relay/reference server). It is never
// registered on the iOS client (which only dials), so its background goroutines do
// not interact with the FFI callback-lifecycle invariant.
type serverListener struct {
	net.Listener
	valid     [][]byte
	fallback  string
	timeout   time.Duration
	ch        chan acceptItem
	done      chan struct{}
	closeOnce sync.Once
}

type acceptItem struct {
	conn net.Conn
	err  error
}

// NewServerListener wraps inner so its Accept yields de-Trojaned connections.
// valid is the set of accepted 56-byte password hashes; fallback is the plaintext
// origin (e.g. "127.0.0.1:80") that rejected/probing connections are spliced to.
// If timeout <= 0, DefaultHandshakeTimeout is used.
func NewServerListener(inner net.Listener, valid [][]byte, fallback string, timeout time.Duration) net.Listener {
	if timeout <= 0 {
		timeout = DefaultHandshakeTimeout
	}
	l := &serverListener{
		Listener: inner,
		valid:    valid,
		fallback: fallback,
		timeout:  timeout,
		ch:       make(chan acceptItem),
		done:     make(chan struct{}),
	}
	go l.acceptLoop()
	return l
}

func (l *serverListener) acceptLoop() {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			select {
			case l.ch <- acceptItem{err: err}:
			case <-l.done:
			}
			return
		}
		go l.validate(c)
	}
}

func (l *serverListener) validate(c net.Conn) {
	target, recorded, err := ReadRequest(c, l.valid, l.timeout)
	if err != nil {
		// Auth/format failure → splice to the fallback so a prober sees a real
		// origin. Other errors (timeout, EOF) → drop quietly.
		if errors.Is(err, ErrAuth) || errors.Is(err, ErrBadFormat) {
			l.spliceOrClose(c, recorded)
		} else {
			_ = c.Close()
		}
		return
	}
	select {
	case l.ch <- acceptItem{conn: &serverConn{Conn: c, target: target}}:
	case <-l.done:
		_ = c.Close()
	}
}

func (l *serverListener) spliceOrClose(c net.Conn, recorded []byte) {
	if l.fallback == "" {
		_ = c.Close()
		return
	}
	SpliceFallback(c, l.fallback, recorded)
}

func (l *serverListener) Accept() (net.Conn, error) {
	select {
	case item := <-l.ch:
		return item.conn, item.err
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *serverListener) Close() error {
	l.closeOnce.Do(func() { close(l.done) })
	return l.Listener.Close()
}
