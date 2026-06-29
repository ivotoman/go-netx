/*
Package ss2022proto implements the Shadowsocks-2022 (SIP022) TCP connection codec.

Wire format (TCP), per direction, after a plaintext per-session salt:

	request:  AEAD(type=0x00 | ts:u64be | varHdrLen:u16be)            (+16 tag)
	          AEAD(ATYP·ADDR·PORT | padLen:u16be | padding | payload) (+16 tag)
	          then chunks: AEAD(len:u16be)+tag  AEAD(payload ≤0xFFFF)+tag
	response: AEAD(type=0x01 | ts:u64be | requestSalt | firstLen:u16be)(+16 tag)
	          AEAD(first payload)+tag  then chunks.

The per-session AEAD key is derived with BLAKE3 derive_key over PSK||salt. Nonces
are 12-byte little-endian counters from zero, advanced once per AEAD operation.

Clean-room: implemented from the public SIP022 specification, RFC 5116/8439, and
the BLAKE3 reference. No GPL source consulted. See PROVENANCE.md.
*/
package ss2022proto

import (
	"crypto/aes"
	"crypto/cipher"
	"fmt"

	"lukechampine.com/blake3"
)

const (
	tagSize       = 16
	nonceSize     = 12
	maxChunkSize  = 0xFFFF
	maxPadding    = 900
	subkeyContext = "shadowsocks 2022 session subkey"

	hdrTypeRequest  byte = 0x00
	hdrTypeResponse byte = 0x01

	// fixed request header: type(1) + timestamp(8) + varHdrLen(2)
	reqFixedLen = 1 + 8 + 2
)

// Method names a SIP022 AEAD-2022 cipher.
type Method struct {
	Name    string
	KeySize int
	newAEAD func(key []byte) (cipher.AEAD, error)
}

// Methods is the supported AEAD-2022 set. Stage 1 ships the AES-GCM variants;
// ChaCha and EIH multi-user are deferred.
var Methods = map[string]Method{
	"2022-blake3-aes-128-gcm": {"2022-blake3-aes-128-gcm", 16, newAESGCM},
	"2022-blake3-aes-256-gcm": {"2022-blake3-aes-256-gcm", 32, newAESGCM},
}

func newAESGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// ValidatePSK checks that psk matches the method's key size.
func ValidatePSK(m Method, psk []byte) error {
	if len(psk) != m.KeySize {
		return fmt.Errorf("ss2022: psk size %d != method key size %d", len(psk), m.KeySize)
	}
	return nil
}

// deriveSubkey computes the per-session AEAD key via BLAKE3 derive_key with the
// SIP022 context string over PSK||salt. Pinned API: lukechampine.com/blake3 v1.4.1
// DeriveKey(subKey, ctx, srcKey) fills subKey with len(subKey) bytes.
func deriveSubkey(psk, salt []byte, keySize int) []byte {
	material := make([]byte, 0, len(psk)+len(salt))
	material = append(material, psk...)
	material = append(material, salt...)
	out := make([]byte, keySize)
	blake3.DeriveKey(out, subkeyContext, material)
	return out
}

// DeriveSubkeyForTest exposes deriveSubkey for the derive_key KAT.
func DeriveSubkeyForTest(psk, salt []byte, keySize int) []byte {
	return deriveSubkey(psk, salt, keySize)
}

// incNonce increments a little-endian counter by one (carry from the low byte).
func incNonce(n []byte) {
	for i := range n {
		n[i]++
		if n[i] != 0 {
			return
		}
	}
}
