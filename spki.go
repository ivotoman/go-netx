package netx

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/pem"
	"fmt"
)

// SPKIPinVerifier builds a VerifyPeerCertificate function (the signature is shared
// by crypto/tls.Config and pion/dtls.Config) that pins the peer's LEAF certificate
// to the SubjectPublicKeyInfo (SPKI) of the given PEM certificate. The tls, utls,
// and dtls client drivers use it together with InsecureSkipVerify: default chain
// validation is replaced by this exact public-key pin.
//
// Security invariants:
//   - The pin is the real SHA-256 of the SPKI (sha256.Sum256), not a derived value.
//   - Only the LEAF (rawCerts[0]) is matched. Matching any chain position would let
//     an active MITM present [attacker_leaf, pinned_cert]: the handshake proves only
//     the attacker's leaf key, yet a non-leaf match would still pass the pin.
//   - An empty peer chain is rejected (fail closed).
//
// The comparison is constant-time; the pinned SPKI is public material, so this is
// hygiene rather than a strict requirement.
func SPKIPinVerifier(certPEM []byte) (func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("uri: invalid PEM certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("uri: parse x509 certificate: %w", err)
	}
	want := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return fmt.Errorf("no peer certificate presented")
		}
		leaf, err := x509.ParseCertificate(rawCerts[0])
		if err != nil {
			return fmt.Errorf("parse peer cert: %w", err)
		}
		got := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
		if subtle.ConstantTimeCompare(got[:], want[:]) == 1 {
			return nil
		}
		return fmt.Errorf("no matching SPKI found")
	}, nil
}
