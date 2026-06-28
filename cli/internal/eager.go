package internal

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
	"time"
)

// stubAddr is a placeholder net.Addr returned by a deferredConn before its
// upstream dial has settled, so the relay can log the peer address synchronously
// the moment the route handler matches (TunMaster.SetRoute dereferences
// Peer.RemoteAddr() immediately).
type stubAddr struct{ network, addr string }

func (a stubAddr) Network() string { return a.network }
func (a stubAddr) String() string  { return a.addr }

// deferredConn is a net.Conn wrapping an upstream dial that may still be in
// flight. Read/Write block until the dial settles (then proxy to the real conn,
// or return the dial error). This lets the tun route handler return immediately
// — and the obfuscation handshake to the upstream run/complete in the
// background — so the upstream can be pre-warmed (dialed at relay start) before
// the first inbound packet arrives. The first inbound datagram is naturally
// held by the blocked Write until the upstream is ready (no separate buffer:
// io.CopyBuffer reads the next datagram only after the prior Write returns, and
// extra datagrams queue in the inbound conn's read buffer).
type deferredConn struct {
	ready  chan struct{}      // closed once conn/err is set
	cancel context.CancelFunc // aborts the in-flight dial on Close
	remote net.Addr           // stub addr for synchronous logging before ready

	mu     sync.Mutex
	conn   net.Conn
	err    error
	closed bool
}

// newDeferredConn starts dialing immediately (with bounded-backoff retry so a
// transient failure on a lossy link doesn't abandon the warm upstream) and
// returns a net.Conn that blocks I/O until the dial settles.
func newDeferredConn(parent context.Context, dial func(context.Context) (net.Conn, error), remote net.Addr) *deferredConn {
	dctx, cancel := context.WithCancel(parent)
	d := &deferredConn{
		ready:  make(chan struct{}),
		cancel: cancel,
		remote: remote,
	}
	go func() {
		var (
			conn    net.Conn
			err     error
			backoff = 200 * time.Millisecond
		)
		for attempt := 0; attempt < 5; attempt++ {
			conn, err = dial(dctx)
			if err == nil || dctx.Err() != nil {
				break
			}
			if attempt == 4 {
				break // no sleep after the final attempt
			}
			select {
			case <-dctx.Done():
			case <-time.After(backoff):
			}
			if dctx.Err() != nil {
				break
			}
			if backoff < 2*time.Second {
				backoff *= 2
			}
		}
		d.mu.Lock()
		d.conn, d.err = conn, err
		// Lost a race with Close: drop the freshly-dialed conn.
		if d.closed && conn != nil {
			_ = conn.Close()
			d.conn, d.err = nil, net.ErrClosed
		}
		finalConn, finalErr := d.conn, d.err
		d.mu.Unlock()
		// Pre-warm outcome (once per warmed upstream, in the background dial
		// goroutine — never on the per-datagram path). The peer address is an
		// IP:port, not secret material. ErrClosed is a normal abandoned-warm
		// teardown, not a failure worth warning about.
		switch {
		case finalErr != nil && !errors.Is(finalErr, net.ErrClosed):
			slog.Warn("netx tun upstream pre-dial failed after retries", "err", finalErr)
		case finalConn != nil:
			slog.Info("netx tun upstream pre-warmed", "peer", finalConn.RemoteAddr().String())
		}
		close(d.ready)
	}()
	return d
}

func (d *deferredConn) await() (net.Conn, error) {
	<-d.ready
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.err != nil {
		return nil, d.err
	}
	return d.conn, nil
}

func (d *deferredConn) Read(p []byte) (int, error) {
	c, err := d.await()
	if err != nil {
		return 0, err
	}
	return c.Read(p)
}

func (d *deferredConn) Write(p []byte) (int, error) {
	c, err := d.await()
	if err != nil {
		return 0, err
	}
	return c.Write(p)
}

func (d *deferredConn) Close() error {
	d.mu.Lock()
	d.closed = true
	c := d.conn
	d.mu.Unlock()
	d.cancel() // abort an in-flight dial
	if c != nil {
		return c.Close()
	}
	return nil
}

func (d *deferredConn) liveConn() net.Conn {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.conn
}

func (d *deferredConn) RemoteAddr() net.Addr {
	if c := d.liveConn(); c != nil {
		return c.RemoteAddr()
	}
	return d.remote
}

func (d *deferredConn) LocalAddr() net.Addr {
	if c := d.liveConn(); c != nil {
		return c.LocalAddr()
	}
	return d.remote
}

func (d *deferredConn) SetDeadline(t time.Time) error      { return d.setDeadline(t, true, true) }
func (d *deferredConn) SetReadDeadline(t time.Time) error  { return d.setDeadline(t, true, false) }
func (d *deferredConn) SetWriteDeadline(t time.Time) error { return d.setDeadline(t, false, true) }

// setDeadline honors the net.Conn contract best-effort: io.CopyBuffer never sets
// deadlines, so applying only once the real conn exists is sufficient.
func (d *deferredConn) setDeadline(t time.Time, read, write bool) error {
	c := d.liveConn()
	if c == nil {
		return nil
	}
	switch {
	case read && write:
		return c.SetDeadline(t)
	case read:
		return c.SetReadDeadline(t)
	default:
		return c.SetWriteDeadline(t)
	}
}
