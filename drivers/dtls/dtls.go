package dtls

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/pedramktb/go-netx"
	"github.com/pion/dtls/v3"
	dtlsnet "github.com/pion/dtls/v3/pkg/net"
)

func init() {
	netx.Register("dtls", func(params map[string]string, listener bool) (netx.Wrapper, error) {
		var certKey, cert []byte
		cfg := &dtls.Config{}
		for key, value := range params {
			switch key {
			case "key":
				var err error
				certKey, err = hex.DecodeString(value)
				if err != nil {
					return netx.Wrapper{}, fmt.Errorf("uri: invalid dtls key parameter: %w", err)
				}
			case "cert":
				var err error
				cert, err = hex.DecodeString(value)
				if err != nil {
					return netx.Wrapper{}, fmt.Errorf("uri: invalid dtls cert parameter: %w", err)
				}
			case "servername":
				cfg.ServerName = value
			case "mtu":
				// Fragment handshake flights to fit the path MTU so DTLS records
				// never IP-fragment (fragmented records are disproportionately
				// dropped by middleboxes on censored paths). pion default 1200.
				n, err := strconv.ParseUint(value, 10, 32)
				if err != nil || n < 576 || n > 1500 {
					return netx.Wrapper{}, fmt.Errorf("uri: invalid dtls mtu parameter %q (want 576..1500)", value)
				}
				cfg.MTU = int(n)
			case "flightinterval":
				// Initial handshake-retransmit interval (pion default 1s). A value
				// a bit above the path RTT recovers a lost flight far faster on a
				// lossy link. Must be positive.
				d, err := time.ParseDuration(value)
				if err != nil || d <= 0 {
					return netx.Wrapper{}, fmt.Errorf("uri: invalid dtls flightinterval parameter %q: %w", value, err)
				}
				cfg.FlightInterval = d
			case "nobackoff":
				b, err := strconv.ParseBool(value)
				if err != nil {
					return netx.Wrapper{}, fmt.Errorf("uri: invalid dtls nobackoff parameter %q: %w", value, err)
				}
				cfg.DisableRetransmitBackoff = b
			case "skipcookie":
				// Server-only: skip the HelloVerifyRequest cookie exchange,
				// removing one round-trip from every fresh handshake. The cookie
				// is anti-spoofing DoS protection; for an SPKI-pinned endpoint the
				// handshake cannot complete without the pinned key, so abuse is
				// bounded — rate-limit upstream if needed.
				if !listener {
					return netx.Wrapper{}, fmt.Errorf("uri: dtls skipcookie parameter is only valid for servers")
				}
				b, err := strconv.ParseBool(value)
				if err != nil {
					return netx.Wrapper{}, fmt.Errorf("uri: invalid dtls skipcookie parameter %q: %w", value, err)
				}
				cfg.InsecureSkipVerifyHello = b
			case "resume":
				// Enable abbreviated-handshake session resumption (skips the
				// Certificate flight for a returning peer). Backed by a bounded
				// process-global store shared across listener/dialer instances.
				//
				// CAVEAT: with the certificate-based dtls driver, pion/dtls v3.1.2
				// fails the *full* handshake when SessionStore is set on both ends
				// with separate stores (the production client/server split). Prefer
				// resumption on the dtlspsk driver, where it works. The param is
				// still accepted here for completeness/forward-compat.
				b, err := strconv.ParseBool(value)
				if err != nil {
					return netx.Wrapper{}, fmt.Errorf("uri: invalid dtls resume parameter %q: %w", value, err)
				}
				if b {
					cfg.SessionStore = sharedSessionStore()
				}
			default:
				return netx.Wrapper{}, fmt.Errorf("uri: unknown dtls parameter %q", key)
			}
		}
		if listener {
			if cert == nil || certKey == nil {
				return netx.Wrapper{}, fmt.Errorf("uri: dtls server requires cert and key parameters")
			}
			certificate, err := tls.X509KeyPair(cert, certKey)
			if err != nil {
				return netx.Wrapper{}, fmt.Errorf("uri: invalid dtls certificate: %w", err)
			}
			cfg.Certificates = []tls.Certificate{certificate}
			certSum := sha256.Sum256(certificate.Certificate[0])
			return netx.Wrapper{
				Name:     "dtls",
				Params:   params,
				Listener: listener,
				SecretParams: []netx.SecretParam{{
					Name:        "key",
					Fingerprint: "sha256=" + colonHex(certSum[:8]),
				}},
				ListenerToListener: func(l net.Listener) (net.Listener, error) {
					return dtls.NewListener(dtlsnet.PacketListenerFromListener(l), cfg)
				},
				ConnToConn: func(c net.Conn) (net.Conn, error) {
					return dtls.Server(dtlsnet.PacketConnFromConn(c), c.RemoteAddr(), cfg)
				}}, nil
		} else {
			if certKey != nil {
				return netx.Wrapper{}, fmt.Errorf("uri: dtls client does not support key parameter")
			}
			if cert != nil {
				var err error
				cfg.InsecureSkipVerify = true
				cfg.VerifyPeerCertificate, err = netx.SPKIPinVerifier(cert)
				if err != nil {
					return netx.Wrapper{}, fmt.Errorf("uri: invalid dtls cert parameter: %w", err)
				}
			}
			if cfg.ServerName == "" && cert == nil {
				return netx.Wrapper{}, fmt.Errorf("uri: dtls client requires servername or cert parameter")
			}
			return netx.Wrapper{
				Name:     "dtls",
				Params:   params,
				Listener: listener,
				DialerToDialer: func(f netx.Dialer) (netx.Dialer, error) {
					return netx.ConnWrapDialer(f, func(c net.Conn) (net.Conn, error) {
						return dtls.Client(dtlsnet.PacketConnFromConn(c), c.RemoteAddr(), cfg)
					})
				},
				ConnToConn: func(c net.Conn) (net.Conn, error) {
					return dtls.Client(dtlsnet.PacketConnFromConn(c), c.RemoteAddr(), cfg)
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
