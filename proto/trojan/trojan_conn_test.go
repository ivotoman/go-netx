package trojanproto

import (
	"bytes"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func TestHashPassword(t *testing.T) {
	// FIPS 180-4 SHA-224("password"), lowercase hex (independent reference vector).
	const want = "d63dc919e201d7bc4c825630d2cf25fdc93d4b2f0d46706d29038d01"
	got := string(HashPassword("password"))
	if got != want {
		t.Fatalf("HashPassword(\"password\") = %s\n                       want %s", got, want)
	}
	if len(got) != hashHexLen {
		t.Fatalf("hash length = %d, want %d", len(got), hashHexLen)
	}
}

func TestClientHeaderBytes(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	cli, err := NewClientConn(c1, "pw", "1.2.3.4:443")
	if err != nil {
		t.Fatalf("NewClientConn: %v", err)
	}

	got := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 256)
		n, _ := c2.Read(buf)
		got <- append([]byte(nil), buf[:n]...)
	}()

	if _, err := cli.Write([]byte("WG")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	var want []byte
	want = append(want, HashPassword("pw")...)
	want = append(want, 0x0D, 0x0A, 0x01)             // CRLF, CMD=CONNECT
	want = append(want, 0x01, 1, 2, 3, 4, 0x01, 0xBB) // ATYP v4, 1.2.3.4, :443
	want = append(want, 0x0D, 0x0A)                   // CRLF
	want = append(want, 'W', 'G')                     // first payload, coalesced

	if first := <-got; !bytes.Equal(first, want) {
		t.Fatalf("client header+payload =\n % x\nwant\n % x", first, want)
	}
}

func TestServerReadRequestRoundTrip(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	cli, err := NewClientConn(c1, "pw", "example.com:8443")
	if err != nil {
		t.Fatalf("NewClientConn: %v", err)
	}
	go func() {
		_, _ = cli.Write([]byte("hello"))
	}()

	target, recorded, err := ReadRequest(c2, [][]byte{HashPassword("pw")}, time.Second)
	if err != nil {
		t.Fatalf("ReadRequest: %v", err)
	}
	if target != "example.com:8443" {
		t.Fatalf("target = %q want example.com:8443", target)
	}
	if recorded != nil {
		t.Fatalf("recorded should be nil on success, got % x", recorded)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(c2, buf); err != nil {
		t.Fatalf("payload read: %v", err)
	}
	if string(buf) != "hello" {
		t.Fatalf("payload = %q want hello", buf)
	}
}

func TestServerReadRequestBadAuth(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	// Client uses the WRONG password → server must reject with ErrAuth and return
	// the consumed bytes for the fallback splice.
	cli, _ := NewClientConn(c1, "wrong", "1.2.3.4:443")
	go func() { _, _ = cli.Write([]byte("probe")) }()

	_, recorded, err := ReadRequest(c2, [][]byte{HashPassword("right")}, time.Second)
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("err = %v want ErrAuth", err)
	}
	// Recorded must begin with the (wrong) 56-byte hash the prober sent.
	if len(recorded) < hashHexLen || !bytes.Equal(recorded[:hashHexLen], HashPassword("wrong")) {
		t.Fatalf("recorded prefix mismatch: % x", recorded)
	}
}

func TestMatchHashSet(t *testing.T) {
	set := [][]byte{HashPassword("a"), HashPassword("b"), HashPassword("c")}
	if !matchHash(set, HashPassword("b")) {
		t.Fatal("expected match for b")
	}
	if matchHash(set, HashPassword("z")) {
		t.Fatal("unexpected match for z")
	}
	if matchHash(nil, HashPassword("a")) {
		t.Fatal("empty set should not match")
	}
}
