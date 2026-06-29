package trojan_test

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"

	"github.com/pedramktb/go-netx"
	_ "github.com/pedramktb/go-netx/drivers/trojan"
)

func mustListener(t *testing.T, s string) netx.ListenerScheme {
	t.Helper()
	var ls netx.ListenerScheme
	if err := ls.UnmarshalText([]byte(s)); err != nil {
		t.Fatalf("parse listener %q: %v", s, err)
	}
	return ls
}

func mustDialer(t *testing.T, s string) netx.DialerScheme {
	t.Helper()
	var ds netx.DialerScheme
	if err := ds.UnmarshalText([]byte(s)); err != nil {
		t.Fatalf("parse dialer %q: %v", s, err)
	}
	return ds
}

// TestTrojanE2E_DatagramBoundaries proves the full driver chain round-trips and
// that WG-style datagram boundaries survive (frame is the outermost wrapper).
func TestTrojanE2E_DatagramBoundaries(t *testing.T) {
	ls := mustListener(t, "tcp+trojan{password=s3cret}+frame")
	ln, err := ls.Listen(context.Background(), "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	type res struct {
		data [][]byte
		err  error
	}
	srvCh := make(chan res, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			srvCh <- res{err: err}
			return
		}
		defer c.Close()
		var got [][]byte
		buf := make([]byte, 4096)
		for range 3 {
			n, err := c.Read(buf)
			if err != nil {
				srvCh <- res{err: err}
				return
			}
			got = append(got, append([]byte(nil), buf[:n]...))
		}
		_, _ = c.Write([]byte("ack"))
		srvCh <- res{data: got}
	}()

	ds := mustDialer(t, "tcp+trojan{password=s3cret,target=1.2.3.4:443}+frame")
	conn, err := ds.Dial(context.Background(), ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	datagrams := [][]byte{
		bytes.Repeat([]byte{0xA1}, 5),
		bytes.Repeat([]byte{0xB2}, 1300),
		bytes.Repeat([]byte{0xC3}, 64),
	}
	for i, d := range datagrams {
		if _, err := conn.Write(d); err != nil {
			t.Fatalf("write datagram %d: %v", i, err)
		}
	}

	ack := make([]byte, 8)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err := conn.Read(ack)
	if err != nil {
		t.Fatalf("read ack: %v", err)
	}
	if string(ack[:n]) != "ack" {
		t.Fatalf("ack = %q want ack", ack[:n])
	}

	r := <-srvCh
	if r.err != nil {
		t.Fatalf("server: %v", r.err)
	}
	if len(r.data) != 3 {
		t.Fatalf("server received %d datagrams, want 3 (boundary loss)", len(r.data))
	}
	for i := range datagrams {
		if !bytes.Equal(r.data[i], datagrams[i]) {
			t.Fatalf("datagram %d: got %d bytes, want %d (boundary not preserved)", i, len(r.data[i]), len(datagrams[i]))
		}
	}
}

// TestTrojanE2E_FallbackOnBadAuth proves a wrong-password connection is spliced to
// the fallback origin (anti-probe) rather than reset.
func TestTrojanE2E_FallbackOnBadAuth(t *testing.T) {
	fb, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer fb.Close()
	fbGot := make(chan int, 1)
	go func() {
		c, err := fb.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 512)
		n, _ := c.Read(buf)
		fbGot <- n
		_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\n\r\n"))
	}()

	ls := mustListener(t, "tcp+trojan{password=right,fallback="+fb.Addr().String()+"}")
	ln, err := ls.Listen(context.Background(), "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	// Raw probe: a wrong 56-byte hash followed by an HTTP-looking request. The
	// trojan server must splice this to the fallback (validate runs internally;
	// the bad-auth conn is never surfaced via Accept).
	raw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial raw: %v", err)
	}
	defer raw.Close()
	probe := append(bytes.Repeat([]byte("a"), 56), []byte("GET / HTTP/1.0\r\n\r\n")...)
	if _, err := raw.Write(probe); err != nil {
		t.Fatalf("probe write: %v", err)
	}

	select {
	case n := <-fbGot:
		if n < 56 {
			t.Fatalf("fallback received %d bytes, want >=56 (replayed probe)", n)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("fallback origin never received the spliced probe bytes")
	}
}
