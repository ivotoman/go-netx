/*
Package trojan registers the "trojan" go-netx driver: a Track-A proxy carrier that
tunnels the payload (e.g. the WireGuard transport) to a fixed target through the
Trojan protocol over an established TLS carrier.

Scheme composition (Trojan MUST be carried over TLS in production):

	client: tcp+tls{...}+trojan{target=relay:443,password=...}+frame
	server: tcp+tls{...}+trojan{password=...}+frame

The driver sees only a net.Conn and cannot detect TLS at runtime, so the
"compose over TLS" requirement is a documented operational contract, not a
runtime check.

Params:
  - password (required): the Trojan password (UTF-8; hashed with SHA-224).
  - target   (client-required): the fixed "host:port" destination.
  - fallback (server-only, default 127.0.0.1:80): plaintext origin that
    rejected/probing connections are spliced to (anti-probe).
*/
package trojan

import (
	"fmt"
	"net"

	"github.com/pedramktb/go-netx"
	trojanproto "github.com/pedramktb/go-netx/proto/trojan"
)

func init() {
	netx.Register("trojan", func(params map[string]string, listener bool) (netx.Wrapper, error) {
		var password, target, fallback string
		for key, value := range params {
			switch key {
			case "password":
				password = value
			case "target":
				target = value
			case "fallback":
				fallback = value
			default:
				return netx.Wrapper{}, fmt.Errorf("uri: unknown trojan parameter %q", key)
			}
		}
		if password == "" {
			return netx.Wrapper{}, fmt.Errorf("uri: missing trojan password parameter")
		}

		if listener {
			if fallback == "" {
				fallback = "127.0.0.1:80"
			}
			valid := [][]byte{trojanproto.HashPassword(password)}
			return netx.Wrapper{
				Name:     "trojan",
				Params:   params,
				Listener: listener,
				ListenerToListener: func(l net.Listener) (net.Listener, error) {
					return trojanproto.NewServerListener(l, valid, fallback, 0), nil
				},
			}, nil
		}

		if target == "" {
			return netx.Wrapper{}, fmt.Errorf("uri: trojan client requires target parameter")
		}
		if _, _, err := net.SplitHostPort(target); err != nil {
			return netx.Wrapper{}, fmt.Errorf("uri: invalid trojan target %q: %w", target, err)
		}
		connToConn := func(c net.Conn) (net.Conn, error) {
			return trojanproto.NewClientConn(c, password, target)
		}
		return netx.Wrapper{
			Name:     "trojan",
			Params:   params,
			Listener: listener,
			DialerToDialer: func(d netx.Dialer) (netx.Dialer, error) {
				return netx.ConnWrapDialer(d, connToConn)
			},
			ConnToConn: connToConn,
		}, nil
	})
}
