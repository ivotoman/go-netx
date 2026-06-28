package tls

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"net"

	"github.com/pedramktb/go-netx"
)

func init() {
	netx.Register("tls", func(params map[string]string, listener bool) (netx.Wrapper, error) {
		var certKey, cert []byte
		cfg := &tls.Config{
			MinVersion: tls.VersionTLS13,
			MaxVersion: tls.VersionTLS13,
		}
		for key, value := range params {
			switch key {
			case "key":
				var err error
				certKey, err = hex.DecodeString(value)
				if err != nil {
					return netx.Wrapper{}, fmt.Errorf("uri: invalid tls key parameter: %w", err)
				}
			case "cert":
				var err error
				cert, err = hex.DecodeString(value)
				if err != nil {
					return netx.Wrapper{}, fmt.Errorf("uri: invalid tls cert parameter: %w", err)
				}
			case "servername":
				cfg.ServerName = value
			default:
				return netx.Wrapper{}, fmt.Errorf("uri: unknown tls parameter %q", key)
			}
		}
		if listener {
			if cert == nil || certKey == nil {
				return netx.Wrapper{}, fmt.Errorf("uri: tls server requires cert and key parameters")
			}
			certificate, err := tls.X509KeyPair(cert, certKey)
			if err != nil {
				return netx.Wrapper{}, fmt.Errorf("uri: invalid tls certificate: %w", err)
			}
			cfg.Certificates = []tls.Certificate{certificate}
			certSum := sha256.Sum256(certificate.Certificate[0])
			return netx.Wrapper{
				Name:     "tls",
				Params:   params,
				Listener: listener,
				SecretParams: []netx.SecretParam{{
					Name: "key",
					// Fingerprint of the paired cert (SHA-256 of DER) — matches
					// `openssl x509 -fingerprint -sha256` and browser cert dialogs.
					// A matching key on the peer must pair with this cert.
					Fingerprint: "sha256=" + colonHex(certSum[:8]),
				}},
				ListenerToListener: func(l net.Listener) (net.Listener, error) {
					return tls.NewListener(l, cfg), nil
				},
				ConnToConn: func(c net.Conn) (net.Conn, error) {
					return tls.Server(c, cfg), nil
				}}, nil
		} else {
			if certKey != nil {
				return netx.Wrapper{}, fmt.Errorf("uri: tls client does not support key parameter")
			}
			if cert != nil {
				var err error
				cfg.InsecureSkipVerify = true
				cfg.VerifyPeerCertificate, err = netx.SPKIPinVerifier(cert)
				if err != nil {
					return netx.Wrapper{}, fmt.Errorf("uri: invalid tls cert parameter: %w", err)
				}
			}
			if cfg.ServerName == "" && cert == nil {
				return netx.Wrapper{}, fmt.Errorf("uri: tls client requires servername or cert parameter")
			}
			return netx.Wrapper{
				Name:     "tls",
				Params:   params,
				Listener: listener,
				DialerToDialer: func(f netx.Dialer) (netx.Dialer, error) {
					return netx.ConnWrapDialer(f, func(c net.Conn) (net.Conn, error) {
						return tls.Client(c, cfg), nil
					})
				},
				ConnToConn: func(c net.Conn) (net.Conn, error) {
					return tls.Client(c, cfg), nil
				}}, nil
		}
	})
}

// colonHex formats b as colon-separated hex pairs (e.g. "ab:cd:ef").
func colonHex(b []byte) string {
	const hexdigits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*3-1)
	for i, x := range b {
		if i > 0 {
			out = append(out, ':')
		}
		out = append(out, hexdigits[x>>4], hexdigits[x&0x0f])
	}
	return string(out)
}
