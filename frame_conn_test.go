package netx_test

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	netx "github.com/pedramktb/go-netx"
)

// helper to write a frame to a raw conn
func writeFrame(t *testing.T, c net.Conn, payload []byte) {
	t.Helper()
	var hdr [2]byte
	binary.BigEndian.PutUint16(hdr[:], uint16(len(payload)))
	if _, err := c.Write(hdr[:]); err != nil {
		t.Fatalf("write hdr: %v", err)
	}
	if len(payload) > 0 {
		if _, err := c.Write(payload); err != nil {
			t.Fatalf("write body: %v", err)
		}
	}
}

func TestFrameConnSimple(t *testing.T) {
	clientRaw, serverRaw := net.Pipe()
	t.Cleanup(func() { _ = clientRaw.Close(); _ = serverRaw.Close() })

	fcClient := netx.NewFrameConn(clientRaw)
	fcServer := netx.NewFrameConn(serverRaw)

	// send one frame
	msg := []byte("hello frame")
	got := make([]byte, len(msg))
	done := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(fcServer, got)
		done <- err
	}()
	time.Sleep(10 * time.Millisecond)
	if _, err := fcClient.Write(msg); err != nil {
		t.Fatalf("write: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("readfull: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout")
	}
	if !bytes.Equal(got, msg) {
		t.Fatalf("mismatch")
	}
}

func TestFrameConnPartialRead(t *testing.T) {
	clientRaw, serverRaw := net.Pipe()
	t.Cleanup(func() { _ = clientRaw.Close(); _ = serverRaw.Close() })

	fcClient := netx.NewFrameConn(clientRaw)
	fcServer := netx.NewFrameConn(serverRaw)

	data := bytes.Repeat([]byte("x"), 1024)
	// Start reader first to avoid pipe deadlock
	first := make([]byte, 100)
	type res struct {
		n   int
		err error
	}
	done1 := make(chan res, 1)
	go func() {
		n1, err := fcServer.Read(first)
		done1 <- res{n: n1, err: err}
	}()
	time.Sleep(10 * time.Millisecond)
	if _, err := fcClient.Write(data); err != nil {
		t.Fatalf("write: %v", err)
	}
	select {
	case r := <-done1:
		if r.err != nil || r.n != 100 {
			t.Fatalf("read1 n=%d err=%v", r.n, r.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout first read")
	}

	// Now read the remainder
	rest := make([]byte, len(data)-len(first))
	n2, err := io.ReadFull(fcServer, rest)
	if err != nil {
		t.Fatalf("read2: %v", err)
	}
	if n2 != len(rest) {
		t.Fatalf("n2=%d", n2)
	}
}

func TestFrameConnDeliversEmptyFrames(t *testing.T) {
	clientRaw, serverRaw := net.Pipe()
	t.Cleanup(func() { _ = clientRaw.Close(); _ = serverRaw.Close() })

	fcServer := netx.NewFrameConn(serverRaw)

	// write two empty frames and then a payload on the client side using raw writer
	payload := []byte("data")
	doneWrite := make(chan struct{})
	go func() {
		writeFrame(t, clientRaw, nil)
		writeFrame(t, clientRaw, []byte{})
		writeFrame(t, clientRaw, payload)
		close(doneWrite)
	}()

	// First empty frame should yield 0, nil
	buf := make([]byte, 8)
	n, err := fcServer.Read(buf)
	if err != nil || n != 0 {
		t.Fatalf("empty1 n=%d err=%v", n, err)
	}
	// Second empty frame should also yield 0, nil
	n, err = fcServer.Read(buf)
	if err != nil || n != 0 {
		t.Fatalf("empty2 n=%d err=%v", n, err)
	}
	// Then the payload
	n, err = fcServer.Read(buf)
	if err != nil {
		t.Fatalf("read payload: %v", err)
	}
	if n != len(payload) || !bytes.Equal(buf[:n], payload) {
		t.Fatalf("unexpected payload: %q", buf[:n])
	}
	select {
	case <-doneWrite:
	case <-time.After(2 * time.Second):
		t.Fatalf("writer blocked")
	}
}

// TestFrameConn_OversizeRejected is a regression test for the 16-bit length
// header: a payload of exactly MaxPacketSize (65535) must round-trip, while a
// larger payload must be rejected with an error and no partial frame emitted
// (pre-fix, uint16(len(p)) wrapped silently and desynced the stream).
func TestFrameConn_OversizeRejected(t *testing.T) {
	clientRaw, serverRaw := net.Pipe()
	t.Cleanup(func() { _ = clientRaw.Close(); _ = serverRaw.Close() })

	fcClient := netx.NewFrameConn(clientRaw)
	fcServer := netx.NewFrameConn(serverRaw)

	// Exactly MaxPacketSize must round-trip losslessly.
	maxPayload := bytes.Repeat([]byte("a"), 65535)
	got := make([]byte, len(maxPayload))
	done := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(fcServer, got)
		done <- err
	}()
	time.Sleep(10 * time.Millisecond)
	n, err := fcClient.Write(maxPayload)
	if err != nil {
		t.Fatalf("write 65535: %v", err)
	}
	if n != len(maxPayload) {
		t.Fatalf("write 65535 returned n=%d, want %d", n, len(maxPayload))
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("readfull 65535: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout reading 65535-byte frame")
	}
	if !bytes.Equal(got, maxPayload) {
		t.Fatalf("65535-byte payload corrupted in round-trip")
	}

	// 65536 must be rejected up front with a "too large" error and n==0, before any
	// wire write. The short write deadline makes this fail fast (instead of hanging
	// on the header write) if the guard is ever removed, and asserting the message
	// keeps the test from passing on an unrelated deadline error.
	_ = fcClient.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))
	over := bytes.Repeat([]byte("b"), 65536)
	n, err = fcClient.Write(over)
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("expected 'too large' error for 65536-byte payload, got n=%d err=%v", n, err)
	}
	if n != 0 {
		t.Fatalf("oversize Write returned n=%d, want 0", n)
	}
}
