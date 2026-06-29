package prepend

import (
	"bytes"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
)

func TestPrependCoalesced(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	pc := New(c1, []byte("HDR"))

	got := make(chan []byte, 2)
	go func() {
		buf := make([]byte, 64)
		n, _ := c2.Read(buf)
		got <- append([]byte(nil), buf[:n]...)
		n, _ = c2.Read(buf)
		got <- append([]byte(nil), buf[:n]...)
	}()

	if n, err := pc.Write([]byte("AB")); err != nil || n != 2 {
		t.Fatalf("first Write = %d,%v want 2,nil", n, err)
	}
	if first := <-got; !bytes.Equal(first, []byte("HDRAB")) {
		t.Fatalf("peer first read = %q want %q", first, "HDRAB")
	}
	if n, err := pc.Write([]byte("CD")); err != nil || n != 2 {
		t.Fatalf("second Write = %d,%v want 2,nil", n, err)
	}
	if second := <-got; !bytes.Equal(second, []byte("CD")) {
		t.Fatalf("peer second read = %q want %q", second, "CD")
	}
}

// shortConn writes at most `cap` bytes on the first Write, recording what it saw.
type shortConn struct {
	net.Conn
	cap   int
	first bool
	mu    sync.Mutex
	seen  bytes.Buffer
}

func (s *shortConn) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(p)
	if !s.first {
		s.first = true
		if n > s.cap {
			n = s.cap
		}
	}
	s.seen.Write(p[:n])
	return n, nil
}

func TestShortWriteAccounting(t *testing.T) {
	// header len 3, payload "AB" len 2, coalesced buf len 5.
	// Case A: underlying writes 4 bytes (header + 1 payload byte) → caller sees 1.
	t.Run("partial-payload", func(t *testing.T) {
		sc := &shortConn{Conn: discardConn{}, cap: 4}
		pc := New(sc, []byte("HDR"))
		n, err := pc.Write([]byte("AB"))
		if n != 1 {
			t.Fatalf("Write returned %d, want 1 (one payload byte landed)", n)
		}
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if got := sc.seen.String(); got != "HDRA" {
			t.Fatalf("underlying saw %q want %q", got, "HDRA")
		}
	})
	// Case B: underlying writes only 2 bytes (< header len 3) → caller sees 0, ErrShortWrite.
	t.Run("header-only-partial", func(t *testing.T) {
		sc := &shortConn{Conn: discardConn{}, cap: 2}
		pc := New(sc, []byte("HDR"))
		n, err := pc.Write([]byte("AB"))
		if n != 0 {
			t.Fatalf("Write returned %d, want 0 (no payload byte landed)", n)
		}
		if !errors.Is(err, io.ErrShortWrite) {
			t.Fatalf("err = %v, want io.ErrShortWrite", err)
		}
	})
}

func TestFlushHeaderThenTransparent(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	pc := New(c1, []byte("HELLO"))

	got := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 8)
		n, _ := c2.Read(buf)
		got <- append([]byte(nil), buf[:n]...)
	}()

	if err := pc.FlushHeader(); err != nil {
		t.Fatalf("FlushHeader: %v", err)
	}
	if h := <-got; !bytes.Equal(h, []byte("HELLO")) {
		t.Fatalf("peer read %q want %q", h, "HELLO")
	}
	// FlushHeader is idempotent.
	if err := pc.FlushHeader(); err != nil {
		t.Fatalf("second FlushHeader: %v", err)
	}
}

// discardConn is a net.Conn whose methods are unused except as an embedding base.
type discardConn struct{ net.Conn }
