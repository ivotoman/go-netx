/*
Package socksaddr implements the SOCKS5 address encoding (RFC 1928 §4):

	ATYP(1) || ADDR(variable) || PORT(2, big-endian)

where ATYP is 0x01 (IPv4, 4 bytes), 0x03 (domain, 1 length byte + N bytes), or
0x04 (IPv6, 16 bytes). This target-address triple is the shared building block of
the Trojan and Shadowsocks-2022 request headers, so it lives in one leaf module
that both proxy codecs (and the future routing inbound) depend on.

This package is implemented clean-room from RFC 1928 alone (see PROVENANCE.md);
it consults no GPL-licensed proxy implementation.
*/
package socksaddr

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strconv"
)

// SOCKS5 address types (RFC 1928 §4).
const (
	ATYPv4     byte = 0x01
	ATYPDomain byte = 0x03
	ATYPv6     byte = 0x04
)

var (
	ErrShort         = errors.New("socksaddr: short buffer")
	ErrBadATYP       = errors.New("socksaddr: unknown ATYP")
	ErrDomainTooLong = errors.New("socksaddr: domain length exceeds 255")
	ErrEmptyDomain   = errors.New("socksaddr: empty domain")
)

// Encode appends ATYP||ADDR||PORT for host:port to dst and returns the extended
// slice. host may be an IPv4 or IPv6 literal or a domain name (1..255 bytes).
func Encode(dst []byte, host string, port uint16) ([]byte, error) {
	if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			dst = append(dst, ATYPv4)
			dst = append(dst, v4...)
		} else {
			dst = append(dst, ATYPv6)
			dst = append(dst, ip.To16()...)
		}
	} else {
		if len(host) == 0 {
			return nil, ErrEmptyDomain
		}
		if len(host) > 255 {
			return nil, ErrDomainTooLong
		}
		dst = append(dst, ATYPDomain, byte(len(host)))
		dst = append(dst, host...)
	}
	return binary.BigEndian.AppendUint16(dst, port), nil
}

// Decode parses one address block from the front of b and returns the host
// (IP literal or domain), the port, and the number of bytes consumed.
func Decode(b []byte) (host string, port uint16, n int, err error) {
	if len(b) < 1 {
		return "", 0, 0, ErrShort
	}
	switch b[0] {
	case ATYPv4:
		if len(b) < 1+net.IPv4len+2 {
			return "", 0, 0, ErrShort
		}
		host = net.IP(b[1 : 1+net.IPv4len]).String()
		n = 1 + net.IPv4len
	case ATYPv6:
		if len(b) < 1+net.IPv6len+2 {
			return "", 0, 0, ErrShort
		}
		host = net.IP(b[1 : 1+net.IPv6len]).String()
		n = 1 + net.IPv6len
	case ATYPDomain:
		if len(b) < 2 {
			return "", 0, 0, ErrShort
		}
		l := int(b[1])
		if l == 0 {
			return "", 0, 0, ErrEmptyDomain
		}
		if len(b) < 2+l+2 {
			return "", 0, 0, ErrShort
		}
		host = string(b[2 : 2+l])
		n = 2 + l
	default:
		return "", 0, 0, ErrBadATYP
	}
	port = binary.BigEndian.Uint16(b[n : n+2])
	return host, port, n + 2, nil
}

// Read consumes exactly one address block off a stream and returns the recovered
// host and port. It is the server-side counterpart to Encode.
func Read(r io.Reader) (host string, port uint16, err error) {
	var atyp [1]byte
	if _, err = io.ReadFull(r, atyp[:]); err != nil {
		return "", 0, err
	}
	switch atyp[0] {
	case ATYPv4:
		var b [net.IPv4len]byte
		if _, err = io.ReadFull(r, b[:]); err != nil {
			return "", 0, err
		}
		host = net.IP(b[:]).String()
	case ATYPv6:
		var b [net.IPv6len]byte
		if _, err = io.ReadFull(r, b[:]); err != nil {
			return "", 0, err
		}
		host = net.IP(b[:]).String()
	case ATYPDomain:
		var l [1]byte
		if _, err = io.ReadFull(r, l[:]); err != nil {
			return "", 0, err
		}
		if l[0] == 0 {
			return "", 0, ErrEmptyDomain
		}
		b := make([]byte, int(l[0]))
		if _, err = io.ReadFull(r, b); err != nil {
			return "", 0, err
		}
		host = string(b)
	default:
		return "", 0, ErrBadATYP
	}
	var p [2]byte
	if _, err = io.ReadFull(r, p[:]); err != nil {
		return "", 0, err
	}
	return host, binary.BigEndian.Uint16(p[:]), nil
}

// SplitHostPort splits a "host:port" target into (host, port). The port must be
// numeric (unlike net.LookupPort, service names are rejected).
func SplitHostPort(target string) (host string, port uint16, err error) {
	h, p, err := net.SplitHostPort(target)
	if err != nil {
		return "", 0, err
	}
	pn, err := strconv.ParseUint(p, 10, 16)
	if err != nil {
		return "", 0, err
	}
	return h, uint16(pn), nil
}
