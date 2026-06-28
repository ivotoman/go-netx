package netx_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"

	netx "github.com/pedramktb/go-netx"
)

// genCert returns a fresh self-signed certificate as (PEM, DER).
func genCert(t *testing.T) (pemBytes, der []byte) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test"}}
	der, err = x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), der
}

// TestSPKIPinVerifier locks in the pin semantics, especially the leaf-only match
// that closes the [attacker_leaf, pinned_cert] MITM bypass.
func TestSPKIPinVerifier(t *testing.T) {
	serverPEM, serverDER := genCert(t)
	_, attackerDER := genCert(t)

	verify, err := netx.SPKIPinVerifier(serverPEM)
	if err != nil {
		t.Fatalf("SPKIPinVerifier: %v", err)
	}

	// The pinned cert presented as the leaf is accepted.
	if err := verify([][]byte{serverDER}, nil); err != nil {
		t.Errorf("pinned leaf should be accepted, got %v", err)
	}
	// The pinned cert as the leaf with trailing certs is still accepted: leaf-only
	// ignores the rest of the chain, it does not over-reject a legitimate leaf.
	if err := verify([][]byte{serverDER, attackerDER}, nil); err != nil {
		t.Errorf("pinned leaf with trailing certs should be accepted, got %v", err)
	}
	// MITM: attacker leaf with the pinned cert smuggled at a non-leaf position must
	// be rejected (the handshake only proves the attacker's leaf key).
	if err := verify([][]byte{attackerDER, serverDER}, nil); err == nil {
		t.Error("attacker leaf + pinned cert at index 1 must be rejected (leaf-only pin)")
	}
	// A non-matching leaf is rejected.
	if err := verify([][]byte{attackerDER}, nil); err == nil {
		t.Error("non-matching leaf must be rejected")
	}
	// An empty peer chain is rejected (fail closed).
	if err := verify(nil, nil); err == nil {
		t.Error("empty peer chain must be rejected")
	}

	// Invalid PEM is rejected at construction.
	if _, err := netx.SPKIPinVerifier([]byte("not a pem")); err == nil {
		t.Error("invalid PEM must error")
	}
}
