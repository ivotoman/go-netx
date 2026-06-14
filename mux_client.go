/*
MuxClient adapts a dial function into a net.Conn by treating every connection obtained
from the dialer as part of a single abstract connection. When a read or write encounters
an error on the current underlying connection, the adapter seamlessly dials a new one and
retries. This provides the illusion of a single persistent connection over a transport
that may use short-lived connections (e.g. UDP associations or individual DNS round-trips).

This is the client-side counterpart to Mux. Where Mux wraps a
net.Listener into a net.Conn by accepting incoming connections on demand, MuxClient
wraps a dial function into a net.Conn by dialing outgoing connections on demand.
*/

package netx

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Dialer is the function signature accepted by NewMuxClient.
// It should return a new net.Conn each time it is called.
type Dialer = func() (net.Conn, error)

type muxClient struct {
	logger Logger
	dial   Dialer
	closed atomic.Bool

	rMu sync.Mutex // serialises reads and redial on read path
	wMu sync.Mutex // serialises writes and redial on write path

	connMu  sync.RWMutex // guards current
	current net.Conn

	deadlineMu    sync.Mutex
	readDeadline  time.Time
	writeDeadline time.Time

	// Self-heal mode (opt-in; see WithMuxClientSelfHeal). OFF by default so the
	// default MuxClient — used by the `mux` wrapper — keeps its EOF-only redial +
	// error-propagation contract (a read-deadline timeout PROPAGATES, it does not
	// redial; see TestMuxClient_Deadlines).
	selfHeal    bool
	readTimeout time.Duration
}

type MuxClientOption func(*muxClient)

// WithMuxClientLogger sets a logger for the MuxClient to use for internal logging.
func WithMuxClientLogger(logger Logger) MuxClientOption {
	return func(c *muxClient) {
		c.logger = logger
	}
}

// WithMuxClientConn seeds the MuxClient with an already-dialled connection as its
// current one, so the first Read/Write uses it instead of dialling again. Lets a
// caller dial eagerly (to validate the upstream / fast-fail) and still hand the
// live connection to a self-healing MuxClient without paying a second handshake.
func WithMuxClientConn(initial net.Conn) MuxClientOption {
	return func(c *muxClient) {
		c.current = initial
	}
}

// WithMuxClientSelfHeal makes the MuxClient transparently re-dial on ANY read or
// write error (not just io.EOF) and, when readTimeout > 0, set that deadline on
// each read. This detects and recovers a silently half-open connection — e.g. a
// UDP association whose bound physical interface vanished on a network change,
// where reads block forever and writes succeed into the void, with no error to
// trip the EOF-only path. A re-dial re-runs the dial function from scratch, which
// for the obfuscation tunnel peer (cli/internal/tun.go) re-runs dialControl and
// re-binds the socket to the CURRENT physical interface — restoring rx without an
// external stop→start.
//
// OFF by default: only the tunnel peer dial opts in. A dial error still
// propagates (it is not retried in a tight loop); the readTimeout paces the
// re-dial loop so it can never busy-spin. Pick readTimeout comfortably above the
// tunnel's keepalive cadence so a merely-idle (healthy) link is not re-dialled
// needlessly — the re-dial is transparent but not free.
func WithMuxClientSelfHeal(readTimeout time.Duration) MuxClientOption {
	return func(c *muxClient) {
		c.selfHeal = true
		c.readTimeout = readTimeout
	}
}

// NewMuxClient wraps a dial function as a net.Conn.
// A new connection is obtained by calling dial on the first Read/Write and whenever
// the current connection reaches EOF or encounters an error.
// Closing the returned conn closes the current underlying connection (if any) and
// prevents further dialling.
func NewMuxClient(dial Dialer, opts ...MuxClientOption) net.Conn {
	dc := &muxClient{
		logger: slog.Default(),
		dial:   dial,
	}
	for _, o := range opts {
		o(dc)
	}
	return dc
}

// ensureConn returns the current connection or dials a new one.
// Caller must NOT hold connMu.
func (c *muxClient) ensureConn() (net.Conn, error) {
	c.connMu.RLock()
	conn := c.current
	c.connMu.RUnlock()
	if conn != nil {
		return conn, nil
	}

	c.connMu.Lock()
	defer c.connMu.Unlock()

	// Double-check after acquiring write lock.
	if c.current != nil {
		return c.current, nil
	}

	newConn, err := c.dial()
	if err != nil {
		c.logger.WarnContext(context.Background(), "muxClient: error dialing new connection", "error", err)
		return nil, err
	}
	c.logger.DebugContext(context.Background(), "muxClient: dialing new connection", "localAddr", newConn.LocalAddr().Network()+"://"+newConn.LocalAddr().String())

	c.deadlineMu.Lock()
	rd, wd := c.readDeadline, c.writeDeadline
	c.deadlineMu.Unlock()
	if !rd.IsZero() {
		_ = newConn.SetReadDeadline(rd)
	}
	if !wd.IsZero() {
		_ = newConn.SetWriteDeadline(wd)
	}

	c.current = newConn
	return newConn, nil
}

// replaceCurrent closes the given connection if it is still current and clears
// the slot so the next operation will redial.
func (c *muxClient) replaceCurrent(old net.Conn) {
	c.connMu.Lock()
	if c.current == old {
		c.logger.DebugContext(context.Background(), "muxClient: closing current connection", "localAddr", c.current.LocalAddr().Network()+"://"+c.current.LocalAddr().String())
		_ = c.current.Close()
		c.current = nil
	}
	c.connMu.Unlock()
}

func (c *muxClient) Read(b []byte) (int, error) {
	c.rMu.Lock()
	defer c.rMu.Unlock()

	if c.selfHeal {
		return c.readSelfHeal(b)
	}

	for {
		if c.closed.Load() {
			return 0, net.ErrClosed
		}

		conn, err := c.ensureConn()
		if err != nil {
			if c.closed.Load() {
				return 0, net.ErrClosed
			}
			return 0, err
		}

		n, err := conn.Read(b)
		if n > 0 {
			if errors.Is(err, io.EOF) {
				c.replaceCurrent(conn)
			}
			return n, nil
		}
		if errors.Is(err, io.EOF) {
			c.replaceCurrent(conn)
			continue // redial on next iteration
		}
		if c.closed.Load() {
			return 0, net.ErrClosed
		}
		return 0, err
	}
}

// readSelfHeal is Read for self-heal mode (see WithMuxClientSelfHeal): re-dial on
// ANY read error, not just io.EOF, so a half-open connection after a network
// change is recovered. A dial error propagates (no tight retry loop); the
// per-read deadline paces the re-dial loop so it never busy-spins.
func (c *muxClient) readSelfHeal(b []byte) (int, error) {
	for {
		if c.closed.Load() {
			return 0, net.ErrClosed
		}

		conn, err := c.ensureConn()
		if err != nil {
			if c.closed.Load() {
				return 0, net.ErrClosed
			}
			// Dial (incl. handshake) failed — e.g. the network is genuinely
			// down. Propagate so the relay tears the route down rather than
			// re-handshaking in a loop; the next inbound packet re-establishes it.
			return 0, err
		}

		if c.readTimeout > 0 {
			_ = conn.SetReadDeadline(time.Now().Add(c.readTimeout))
		}

		n, err := conn.Read(b)
		if n > 0 {
			if errors.Is(err, io.EOF) {
				c.replaceCurrent(conn)
			}
			return n, nil
		}
		if err != nil {
			if c.closed.Load() {
				return 0, net.ErrClosed
			}
			// EOF, a read-deadline timeout, ECONNRESET, ENETUNREACH, … all mean
			// the inbound path is dead: drop this conn and re-dial (a fresh dial
			// re-binds to the current physical interface). Re-dialling here, not
			// returning, is what makes the tunnel self-heal a network change.
			c.logger.InfoContext(context.Background(), "muxClient: re-dialing peer after read error", "error", err)
			c.replaceCurrent(conn)
			continue
		}
		// n == 0 with no error: benign short read — read again on the same conn.
	}
}

func (c *muxClient) Write(b []byte) (int, error) {
	c.wMu.Lock()
	defer c.wMu.Unlock()

	if c.selfHeal {
		return c.writeSelfHeal(b)
	}

	if c.closed.Load() {
		return 0, net.ErrClosed
	}

	conn, err := c.ensureConn()
	if err != nil {
		if c.closed.Load() {
			return 0, net.ErrClosed
		}
		return 0, err
	}

	return conn.Write(b)
}

// writeSelfHeal is Write for self-heal mode: on a write error, drop the conn and
// re-dial once, then retry. Bounded (max two attempts) so a hard failure
// propagates instead of looping. Most network-change failures surface on the
// read path (a half-open socket still accepts writes), so this is the secondary
// recovery trigger.
func (c *muxClient) writeSelfHeal(b []byte) (int, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if c.closed.Load() {
			return 0, net.ErrClosed
		}
		conn, err := c.ensureConn()
		if err != nil {
			if c.closed.Load() {
				return 0, net.ErrClosed
			}
			return 0, err
		}
		n, err := conn.Write(b)
		if err == nil {
			return n, nil
		}
		lastErr = err
		c.replaceCurrent(conn)
	}
	return 0, lastErr
}

func (c *muxClient) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}

	c.connMu.Lock()
	defer c.connMu.Unlock()
	if c.current != nil {
		err := c.current.Close()
		c.current = nil
		return err
	}
	return nil
}

func (c *muxClient) SetDeadline(t time.Time) error {
	c.deadlineMu.Lock()
	c.readDeadline = t
	c.writeDeadline = t
	c.deadlineMu.Unlock()

	c.connMu.RLock()
	defer c.connMu.RUnlock()
	if c.current != nil {
		return c.current.SetDeadline(t)
	}
	return nil
}

func (c *muxClient) SetReadDeadline(t time.Time) error {
	c.deadlineMu.Lock()
	c.readDeadline = t
	c.deadlineMu.Unlock()

	c.connMu.RLock()
	defer c.connMu.RUnlock()
	if c.current != nil {
		return c.current.SetReadDeadline(t)
	}
	return nil
}

func (c *muxClient) SetWriteDeadline(t time.Time) error {
	c.deadlineMu.Lock()
	c.writeDeadline = t
	c.deadlineMu.Unlock()

	c.connMu.RLock()
	defer c.connMu.RUnlock()
	if c.current != nil {
		return c.current.SetWriteDeadline(t)
	}
	return nil
}

func (c *muxClient) LocalAddr() net.Addr  { return &muxVirtualAddr{} }
func (c *muxClient) RemoteAddr() net.Addr { return &muxVirtualAddr{} }
