/*
Package ss2022 registers the "ss2022" go-netx driver: a Track-A proxy carrier that
tunnels the payload (e.g. the WireGuard transport) to a fixed target through the
Shadowsocks-2022 (SIP022) protocol.

Because ss2022 self-frames its own AEAD chunk stream, it must be carried directly
over the transport and the datagram-boundary `frame` wrapper sits OUTERMOST:

	client: tcp+ss2022{method=2022-blake3-aes-128-gcm,psk=<b64>,target=relay:8443}+frame
	server: tcp+ss2022{method=2022-blake3-aes-128-gcm,psk=<b64>}+frame

Params:
  - method (required): 2022-blake3-aes-128-gcm | 2022-blake3-aes-256-gcm
  - psk    (required): base64-encoded pre-shared key (16 or 32 bytes per method)
  - target (client-required): the fixed "host:port" destination
*/
package ss2022

import (
	"encoding/base64"
	"fmt"
	"net"

	"github.com/pedramktb/go-netx"
	"github.com/pedramktb/go-netx/proto/socksaddr"
	ss2022proto "github.com/pedramktb/go-netx/proto/ss2022"
)

// decodePSK accepts the URL-safe base64 form (the canonical, URI-safe encoding —
// the scheme grammar splits on '+' and treats '=' specially, so standard base64 is
// unsafe in a URI) and tolerates padded/standard variants when they happen to be
// '+'/'/'-free.
func decodePSK(s string) ([]byte, error) {
	for _, enc := range []*base64.Encoding{
		base64.RawURLEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.StdEncoding,
	} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, fmt.Errorf("not valid base64")
}

func init() {
	netx.Register("ss2022", func(params map[string]string, listener bool) (netx.Wrapper, error) {
		var (
			method ss2022proto.Method
			haveM  bool
			psk    []byte
			target string
		)
		for key, value := range params {
			switch key {
			case "method":
				m, ok := ss2022proto.Methods[value]
				if !ok {
					return netx.Wrapper{}, fmt.Errorf("uri: unknown ss2022 method %q", value)
				}
				method, haveM = m, true
			case "psk":
				b, err := decodePSK(value)
				if err != nil {
					return netx.Wrapper{}, fmt.Errorf("uri: invalid ss2022 psk: %w", err)
				}
				psk = b
			case "target":
				target = value
			default:
				return netx.Wrapper{}, fmt.Errorf("uri: unknown ss2022 parameter %q", key)
			}
		}
		if !haveM {
			return netx.Wrapper{}, fmt.Errorf("uri: missing ss2022 method parameter")
		}
		if err := ss2022proto.ValidatePSK(method, psk); err != nil {
			return netx.Wrapper{}, fmt.Errorf("uri: %w", err)
		}

		if listener {
			replay := ss2022proto.NewReplayGuard()
			connToConn := func(c net.Conn) (net.Conn, error) {
				return ss2022proto.NewServerConn(c, method, psk, replay)
			}
			return netx.Wrapper{
				Name:     "ss2022",
				Params:   params,
				Listener: listener,
				ListenerToListener: func(l net.Listener) (net.Listener, error) {
					return netx.ConnWrapListener(l, connToConn)
				},
				ConnToConn: connToConn,
			}, nil
		}

		if target == "" {
			return netx.Wrapper{}, fmt.Errorf("uri: ss2022 client requires target parameter")
		}
		host, port, err := socksaddr.SplitHostPort(target)
		if err != nil {
			return netx.Wrapper{}, fmt.Errorf("uri: ss2022 target: %w", err)
		}
		connToConn := func(c net.Conn) (net.Conn, error) {
			return ss2022proto.NewClientConn(c, method, psk, host, port)
		}
		return netx.Wrapper{
			Name:     "ss2022",
			Params:   params,
			Listener: listener,
			DialerToDialer: func(d netx.Dialer) (netx.Dialer, error) {
				return netx.ConnWrapDialer(d, connToConn)
			},
			ConnToConn: connToConn,
		}, nil
	})
}
