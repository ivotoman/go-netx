package ss2022_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"testing"
	"time"

	"github.com/pedramktb/go-netx"
	_ "github.com/pedramktb/go-netx/drivers/ss2022"
)

func b64psk(keySize int) string {
	return base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, keySize))
}

func TestRegistered(t *testing.T) {
	if _, err := netx.GetDriver("ss2022"); err != nil {
		t.Fatalf("ss2022 not registered: %v", err)
	}
}

func TestParamGrammar(t *testing.T) {
	psk := b64psk(16)
	var ds netx.DialerScheme
	good := "tcp+ss2022{method=2022-blake3-aes-128-gcm,psk=" + psk + ",target=1.2.3.4:443}+frame"
	if err := ds.UnmarshalText([]byte(good)); err != nil {
		t.Fatalf("valid scheme rejected: %v", err)
	}
	bad := map[string]string{
		"missing method":   "tcp+ss2022{psk=" + psk + ",target=1.2.3.4:443}",
		"unknown method":   "tcp+ss2022{method=bogus,psk=" + psk + ",target=1.2.3.4:443}",
		"missing psk":      "tcp+ss2022{method=2022-blake3-aes-128-gcm,target=1.2.3.4:443}",
		"wrong psk size":   "tcp+ss2022{method=2022-blake3-aes-256-gcm,psk=" + psk + ",target=1.2.3.4:443}",
		"client no target": "tcp+ss2022{method=2022-blake3-aes-128-gcm,psk=" + psk + "}",
	}
	for name, s := range bad {
		var d netx.DialerScheme
		if err := d.UnmarshalText([]byte(s)); err == nil {
			t.Fatalf("%s: expected rejection for %q", name, s)
		}
	}
}

func roundTrip(t *testing.T, method string, keySize int) {
	t.Helper()
	psk := b64psk(keySize)
	srvScheme := "tcp+ss2022{method=" + method + ",psk=" + psk + "}+frame"
	cliScheme := "tcp+ss2022{method=" + method + ",psk=" + psk + ",target=1.2.3.4:443}+frame"

	var ls netx.ListenerScheme
	if err := ls.UnmarshalText([]byte(srvScheme)); err != nil {
		t.Fatalf("listener parse: %v", err)
	}
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
		_, _ = c.Write([]byte("pong"))
		srvCh <- res{data: got}
	}()

	var ds netx.DialerScheme
	if err := ds.UnmarshalText([]byte(cliScheme)); err != nil {
		t.Fatalf("dialer parse: %v", err)
	}
	conn, err := ds.Dial(context.Background(), ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	datagrams := [][]byte{
		bytes.Repeat([]byte{0x01}, 3),
		bytes.Repeat([]byte{0x02}, 1300),
		bytes.Repeat([]byte{0x03}, 200),
	}
	for i, d := range datagrams {
		if _, err := conn.Write(d); err != nil {
			t.Fatalf("write datagram %d: %v", i, err)
		}
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	pong := make([]byte, 8)
	n, err := conn.Read(pong)
	if err != nil {
		t.Fatalf("read pong: %v", err)
	}
	if string(pong[:n]) != "pong" {
		t.Fatalf("pong = %q", pong[:n])
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
			t.Fatalf("datagram %d corrupted: %d bytes want %d", i, len(r.data[i]), len(datagrams[i]))
		}
	}
}

func TestSS2022E2E_AES128(t *testing.T) { roundTrip(t, "2022-blake3-aes-128-gcm", 16) }
func TestSS2022E2E_AES256(t *testing.T) { roundTrip(t, "2022-blake3-aes-256-gcm", 32) }
