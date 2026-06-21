package ss2022proto

import (
	"bytes"
	"testing"
	"time"
)

func TestIncNonceLittleEndian(t *testing.T) {
	n := make([]byte, nonceSize)
	incNonce(n)
	if n[0] != 1 {
		t.Fatalf("after 1 inc: %v", n)
	}
	n2 := make([]byte, nonceSize)
	n2[0] = 0xFF
	incNonce(n2)
	if n2[0] != 0 || n2[1] != 1 {
		t.Fatalf("carry failed: %v", n2)
	}
	n3 := bytes.Repeat([]byte{0xFF}, nonceSize)
	incNonce(n3)
	for i, b := range n3 {
		if b != 0 {
			t.Fatalf("wrap byte %d = %d", i, b)
		}
	}
}

func TestDeriveSubkeyDeterministicAndKeyed(t *testing.T) {
	psk := bytes.Repeat([]byte{0x11}, 16)
	salt := bytes.Repeat([]byte{0x22}, 16)
	a := deriveSubkey(psk, salt, 16)
	if len(a) != 16 {
		t.Fatalf("len %d want 16", len(a))
	}
	if !bytes.Equal(a, deriveSubkey(psk, salt, 16)) {
		t.Fatal("derive not deterministic")
	}
	if bytes.Equal(a, deriveSubkey(psk, bytes.Repeat([]byte{0x23}, 16), 16)) {
		t.Fatal("salt not mixed into the subkey")
	}
	if bytes.Equal(a, deriveSubkey(bytes.Repeat([]byte{0x12}, 16), salt, 16)) {
		t.Fatal("psk not mixed into the subkey")
	}
	// 256-bit method derives a 32-byte key.
	if len(deriveSubkey(bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32), 32)) != 32 {
		t.Fatal("aes-256 subkey size")
	}
}

func TestReplayWindow(t *testing.T) {
	g := NewReplayGuard()
	now := time.Unix(1_700_000_000, 0)
	g.now = func() time.Time { return now }
	base := uint64(now.Unix())
	if !g.checkTime(base - 29) {
		t.Fatal("-29s should be within window")
	}
	if g.checkTime(base - 31) {
		t.Fatal("-31s should be rejected")
	}
	if g.checkTime(base + 31) {
		t.Fatal("+31s should be rejected")
	}
}

func TestSaltCache(t *testing.T) {
	g := NewReplayGuard()
	now := time.Unix(1_700_000_000, 0)
	g.now = func() time.Time { return now }
	salt := []byte("0123456789abcdef")
	if !g.checkSalt(salt) {
		t.Fatal("first salt should be accepted")
	}
	if g.checkSalt(salt) {
		t.Fatal("replayed salt should be rejected")
	}
	now = now.Add(61 * time.Second)
	if !g.checkSalt(salt) {
		t.Fatal("salt should be accepted again after 60s")
	}
}
