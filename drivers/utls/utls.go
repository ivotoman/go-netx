package utls

import (
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/pedramktb/go-netx"
	utls "github.com/refraction-networking/utls"
)

func init() {
	netx.Register("utls", func(params map[string]string, listener bool) (netx.Wrapper, error) {
		if listener {
			return netx.Wrapper{}, errors.New("uri: utls is exclusive to clients, use tls for servers instead")
		}
		var cert []byte
		cfg := &utls.Config{
			MinVersion: tls.VersionTLS13,
			MaxVersion: tls.VersionTLS13,
		}
		id := utls.HelloChrome_Auto
		for key, value := range params {
			switch key {
			case "cert":
				var err error
				cert, err = hex.DecodeString(value)
				if err != nil {
					return netx.Wrapper{}, fmt.Errorf("uri: invalid utls cert parameter: %w", err)
				}
			case "servername":
				cfg.ServerName = value
			case "hello":
				switch strings.ToLower(value) {
				case "chrome":
					id = utls.HelloChrome_Auto
				case "firefox":
					id = utls.HelloFirefox_Auto
				case "ios":
					id = utls.HelloIOS_Auto
				case "android":
					id = utls.HelloAndroid_11_OkHttp
				case "safari":
					id = utls.HelloSafari_Auto
				case "edge":
					id = utls.HelloEdge_Auto
				case "randomized":
					id = utls.HelloRandomizedALPN
				case "randomizednoalpn":
					id = utls.HelloRandomized
				default:
					return netx.Wrapper{}, fmt.Errorf("unknown utls hello profile %q", value)
				}
			default:
				return netx.Wrapper{}, fmt.Errorf("uri: unknown utls parameter %q", key)
			}
		}
		if cert != nil {
			var err error
			cfg.InsecureSkipVerify = true
			cfg.VerifyPeerCertificate, err = netx.SPKIPinVerifier(cert)
			if err != nil {
				return netx.Wrapper{}, fmt.Errorf("uri: invalid utls cert parameter: %w", err)
			}
		}
		if cfg.ServerName == "" && cert == nil {
			return netx.Wrapper{}, fmt.Errorf("uri: utls client requires servername or cert parameter")
		}
		return netx.Wrapper{
			Name:     "utls",
			Params:   params,
			Listener: listener,
			DialerToDialer: func(f netx.Dialer) (netx.Dialer, error) {
				return netx.ConnWrapDialer(f, func(c net.Conn) (net.Conn, error) {
					uc := utls.UClient(c, cfg, id)
					return uc, uc.Handshake()
				})
			},
			ConnToConn: func(c net.Conn) (net.Conn, error) {
				uc := utls.UClient(c, cfg, id)
				return uc, uc.Handshake()
			}}, nil
	})
}
