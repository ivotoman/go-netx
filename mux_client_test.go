package netx_test

import (
	"bytes"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	netx "github.com/pedramktb/go-netx"
)

// helper: returns a dial function that connects to a TCP listener.
func tcpDialer(t *testing.T, addr string) netx.Dialer {
	t.Helper()
	return func() (net.Conn, error) {
		return net.Dial("tcp", addr)
	}
}

func TestMuxClient_SingleConnection(t *testing.T) {
	ln := tcpListener(t)
	dc := netx.NewMuxClient(tcpDialer(t, ln.Addr().String()))
	defer dc.Close()

	msg := []byte("hello mux client")

	var wg sync.WaitGroup
	wg.Go(func() {
		c, err := ln.Accept()
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		defer c.Close()
		buf := make([]byte, 256)
		n, err := c.Read(buf)
		if err != nil {
			t.Errorf("read: %v", err)
			return
		}
		// Echo back
		if _, err := c.Write(buf[:n]); err != nil {
			t.Errorf("write: %v", err)
		}
	})

	// Write through the MuxClient (triggers dial)
	if _, err := dc.Write(msg); err != nil {
		t.Fatalf("write: %v", err)
	}

	buf := make([]byte, 256)
	n, err := dc.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(buf[:n], msg) {
		t.Fatalf("got %q, want %q", buf[:n], msg)
	}

	wg.Wait()
}

func TestMuxClient_MultipleConnections(t *testing.T) {
	ln := tcpListener(t)
	dc := netx.NewMuxClient(tcpDialer(t, ln.Addr().String()))
	defer dc.Close()

	messages := []string{"first", "second", "third"}

	// Server: accept connections, send a message, and close.
	// Each accepted connection carries one message.
	go func() {
		for _, msg := range messages {
			c, err := ln.Accept()
			if err != nil {
				t.Errorf("accept: %v", err)
				return
			}
			if _, err := c.Write([]byte(msg)); err != nil {
				t.Errorf("write %q: %v", msg, err)
			}
			c.Close()
		}
	}()

	// Client: read all messages through a single MuxClient.
	// MuxClient dials on the first Read (current is nil) and redials
	// transparently whenever it encounters EOF.
	for _, want := range messages {
		buf := make([]byte, 256)
		n, err := dc.Read(buf)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if string(buf[:n]) != want {
			t.Fatalf("got %q, want %q", buf[:n], want)
		}
	}
}

func TestMuxClient_RequestResponse(t *testing.T) {
	ln := tcpListener(t)
	dc := netx.NewMuxClient(tcpDialer(t, ln.Addr().String()))
	defer dc.Close()

	rounds := 3
	var wg sync.WaitGroup

	// Server: accept one connection and handle multiple request-response
	// rounds on it (connection stays open).
	wg.Go(func() {
		c, err := ln.Accept()
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		defer c.Close()
		for i := 0; i < rounds; i++ {
			buf := make([]byte, 256)
			n, err := c.Read(buf)
			if err != nil {
				t.Errorf("read %d: %v", i, err)
				return
			}
			if _, err := c.Write(buf[:n]); err != nil {
				t.Errorf("write %d: %v", i, err)
				return
			}
		}
	})

	for i := range rounds {
		msg := []byte{byte('A' + i)}
		if _, err := dc.Write(msg); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		buf := make([]byte, 256)
		n, err := dc.Read(buf)
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if !bytes.Equal(buf[:n], msg) {
			t.Fatalf("round %d: got %q, want %q", i, buf[:n], msg)
		}
	}

	wg.Wait()
}

func TestMuxClient_Close(t *testing.T) {
	ln := tcpListener(t)
	dc := netx.NewMuxClient(tcpDialer(t, ln.Addr().String()))

	if err := dc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Read after close should return error
	buf := make([]byte, 256)
	_, err := dc.Read(buf)
	if err == nil {
		t.Fatal("expected error on read after close")
	}

	// Write after close should return error
	_, err = dc.Write([]byte("data"))
	if err == nil {
		t.Fatal("expected error on write after close")
	}

	// Double close should not panic
	if err := dc.Close(); err != nil {
		t.Fatalf("double close: %v", err)
	}
}

func TestMuxClient_WriteTriggersDialOnNoConnection(t *testing.T) {
	ln := tcpListener(t)
	dc := netx.NewMuxClient(tcpDialer(t, ln.Addr().String()))
	defer dc.Close()

	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		// drain
		_, _ = io.Copy(io.Discard, c)
	}()

	// First write should trigger a dial
	if _, err := dc.Write([]byte("trigger")); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestMuxClient_Deadlines(t *testing.T) {
	ln := tcpListener(t)
	dc := netx.NewMuxClient(tcpDialer(t, ln.Addr().String()))
	defer dc.Close()

	// Set a very short read deadline before any connection exists
	if err := dc.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}

	// Accept on server side so the client dial succeeds
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		// Hold open — never send data
		time.Sleep(2 * time.Second)
	}()

	// Trigger dial by writing
	if _, err := dc.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}

	buf := make([]byte, 256)
	_, err := dc.Read(buf)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("expected timeout, got %v", err)
	}
}

func TestMuxClient_DialError(t *testing.T) {
	dialErr := errors.New("dial failed")
	dc := netx.NewMuxClient(func() (net.Conn, error) {
		return nil, dialErr
	})
	defer dc.Close()

	_, err := dc.Write([]byte("data"))
	if !errors.Is(err, dialErr) {
		t.Fatalf("expected dial error, got %v", err)
	}

	_, err = dc.Read(make([]byte, 256))
	if !errors.Is(err, dialErr) {
		t.Fatalf("expected dial error, got %v", err)
	}
}

// --- Self-heal mode (WithMuxClientSelfHeal) -------------------------------------

// fakeConn is a programmable net.Conn for the self-heal tests. readFn drives the
// Read behaviour (data, errors, timeouts); writes succeed by default.
type fakeConn struct {
	readFn  func(b []byte) (int, error)
	writeFn func(b []byte) (int, error)
	mu      sync.Mutex
	closed  bool
}

func (f *fakeConn) Read(b []byte) (int, error) { return f.readFn(b) }
func (f *fakeConn) Write(b []byte) (int, error) {
	if f.writeFn != nil {
		return f.writeFn(b)
	}
	return len(b), nil
}
func (f *fakeConn) Close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	return nil
}
func (f *fakeConn) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}
func (f *fakeConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (f *fakeConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (f *fakeConn) SetDeadline(time.Time) error      { return nil }
func (f *fakeConn) SetReadDeadline(time.Time) error  { return nil }
func (f *fakeConn) SetWriteDeadline(time.Time) error { return nil }

// readOnce returns data on the first call, then io.EOF.
func readOnce(data []byte) func(b []byte) (int, error) {
	done := false
	return func(b []byte) (int, error) {
		if done {
			return 0, io.EOF
		}
		done = true
		return copy(b, data), nil
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

// seqDialer hands out the given conns in order, then errors.
func seqDialer(conns ...net.Conn) netx.Dialer {
	i := 0
	return func() (net.Conn, error) {
		if i >= len(conns) {
			return nil, errors.New("no more conns")
		}
		c := conns[i]
		i++
		return c, nil
	}
}

// In self-heal mode a non-EOF read error (here a connection reset) must re-dial
// — exactly the network-change case the default EOF-only path leaves stuck.
func TestMuxClient_SelfHeal_RedialsOnNonEOFError(t *testing.T) {
	dead := &fakeConn{readFn: func([]byte) (int, error) { return 0, errors.New("connection reset by peer") }}
	healed := &fakeConn{readFn: readOnce([]byte("healed"))}
	dc := netx.NewMuxClient(seqDialer(dead, healed), netx.WithMuxClientSelfHeal(0))
	defer dc.Close()

	buf := make([]byte, 256)
	n, err := dc.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf[:n]) != "healed" {
		t.Fatalf("got %q, want %q", buf[:n], "healed")
	}
	if !dead.isClosed() {
		t.Fatal("expected the dead conn to be closed on re-dial")
	}
}

// A read-deadline timeout is a net.Error/Timeout — in DEFAULT mode it propagates
// (see TestMuxClient_Deadlines); in self-heal mode it must re-dial instead.
func TestMuxClient_SelfHeal_RedialsOnTimeout(t *testing.T) {
	stalled := &fakeConn{readFn: func([]byte) (int, error) { return 0, timeoutErr{} }}
	healed := &fakeConn{readFn: readOnce([]byte("after-timeout"))}
	dc := netx.NewMuxClient(seqDialer(stalled, healed), netx.WithMuxClientSelfHeal(10*time.Millisecond))
	defer dc.Close()

	buf := make([]byte, 256)
	n, err := dc.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf[:n]) != "after-timeout" {
		t.Fatalf("got %q, want %q", buf[:n], "after-timeout")
	}
}

// WithMuxClientConn seeds the current conn so the first Read uses it without
// dialling (the dialer must not be called).
func TestMuxClient_SelfHeal_SeedsInitialConn(t *testing.T) {
	seed := &fakeConn{readFn: readOnce([]byte("seeded"))}
	dc := netx.NewMuxClient(
		func() (net.Conn, error) { t.Fatal("dialer must not be called"); return nil, nil },
		netx.WithMuxClientConn(seed),
		netx.WithMuxClientSelfHeal(0),
	)
	defer dc.Close()

	buf := make([]byte, 256)
	n, err := dc.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf[:n]) != "seeded" {
		t.Fatalf("got %q, want %q", buf[:n], "seeded")
	}
}

// A dial failure (no current conn) must propagate, not loop forever.
func TestMuxClient_SelfHeal_DialErrorPropagates(t *testing.T) {
	dialErr := errors.New("dial failed")
	dc := netx.NewMuxClient(func() (net.Conn, error) { return nil, dialErr }, netx.WithMuxClientSelfHeal(0))
	defer dc.Close()

	done := make(chan struct{})
	go func() {
		_, err := dc.Read(make([]byte, 256))
		if !errors.Is(err, dialErr) {
			t.Errorf("expected dial error, got %v", err)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("self-heal Read looped on dial error instead of propagating")
	}
}

// Close must break the self-heal loop.
func TestMuxClient_SelfHeal_ClosedReturns(t *testing.T) {
	dc := netx.NewMuxClient(seqDialer(), netx.WithMuxClientSelfHeal(0))
	if err := dc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := dc.Read(make([]byte, 256)); err == nil {
		t.Fatal("expected error reading after close")
	}
	if _, err := dc.Write([]byte("x")); err == nil {
		t.Fatal("expected error writing after close")
	}
}
