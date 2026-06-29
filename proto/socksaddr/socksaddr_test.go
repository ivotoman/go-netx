package socksaddr

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestEncodeKAT(t *testing.T) {
	cases := []struct {
		name string
		host string
		port uint16
		want []byte
	}{
		{"ipv4", "1.2.3.4", 443, []byte{0x01, 1, 2, 3, 4, 0x01, 0xBB}},
		{"domain", "a.com", 80, append([]byte{0x03, 0x05}, append([]byte("a.com"), 0x00, 0x50)...)},
		{"ipv6", "::1", 8443, append(append([]byte{0x04}, []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}...), 0x20, 0xFB)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Encode(nil, c.host, c.port)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			if !bytes.Equal(got, c.want) {
				t.Fatalf("Encode(%q,%d) = % x, want % x", c.host, c.port, got, c.want)
			}
		})
	}
}

func TestRoundTrip(t *testing.T) {
	cases := []struct {
		host string
		port uint16
	}{
		{"1.2.3.4", 443},
		{"example.com", 8080},
		{"2001:db8::1", 51820},
	}
	for _, c := range cases {
		enc, err := Encode(nil, c.host, c.port)
		if err != nil {
			t.Fatalf("Encode(%q): %v", c.host, err)
		}
		// Decode: also append trailing junk to ensure n is reported correctly.
		dh, dp, n, err := Decode(append(enc, 0xDE, 0xAD))
		if err != nil {
			t.Fatalf("Decode(%q): %v", c.host, err)
		}
		if dh != c.host || dp != c.port || n != len(enc) {
			t.Fatalf("Decode(%q) = %q,%d,%d want %q,%d,%d", c.host, dh, dp, n, c.host, c.port, len(enc))
		}
		// Read: same bytes off a stream.
		rh, rp, err := Read(bytes.NewReader(enc))
		if err != nil {
			t.Fatalf("Read(%q): %v", c.host, err)
		}
		if rh != c.host || rp != c.port {
			t.Fatalf("Read(%q) = %q,%d want %q,%d", c.host, rh, rp, c.host, c.port)
		}
	}
}

func TestDecodeErrors(t *testing.T) {
	if _, _, _, err := Decode([]byte{0x02, 1, 2, 3}); !errors.Is(err, ErrBadATYP) {
		t.Fatalf("bad ATYP: got %v want ErrBadATYP", err)
	}
	if _, _, _, err := Decode([]byte{0x01, 1, 2}); !errors.Is(err, ErrShort) {
		t.Fatalf("truncated ipv4: got %v want ErrShort", err)
	}
	if _, _, _, err := Decode([]byte{0x03, 0x00, 0x00, 0x50}); !errors.Is(err, ErrEmptyDomain) {
		t.Fatalf("zero-length domain: got %v want ErrEmptyDomain", err)
	}
	if _, _, _, err := Decode([]byte{0x03, 0x05, 'a', 'b'}); !errors.Is(err, ErrShort) {
		t.Fatalf("truncated domain: got %v want ErrShort", err)
	}
}

func TestEncodeDomainTooLong(t *testing.T) {
	if _, err := Encode(nil, strings.Repeat("a", 256), 80); !errors.Is(err, ErrDomainTooLong) {
		t.Fatalf("256-char domain: got %v want ErrDomainTooLong", err)
	}
}

func TestSplitHostPort(t *testing.T) {
	h, p, err := SplitHostPort("example.com:443")
	if err != nil || h != "example.com" || p != 443 {
		t.Fatalf("SplitHostPort = %q,%d,%v", h, p, err)
	}
	if _, _, err := SplitHostPort("example.com:https"); err == nil {
		t.Fatalf("service-name port should be rejected")
	}
}
