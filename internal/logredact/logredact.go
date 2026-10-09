// Package logredact reduces client IP addresses to a network prefix before
// they are logged. A /24 or /48 is enough to spot abuse from one network
// without storing a personal identifier in log retention.
package logredact

import "net/netip"

const (
	ipv4PrefixBits = 24
	ipv6PrefixBits = 48
)

// Prefix returns the redacted network containing addr, for example
// "81.2.69.0/24" or "2a02:d500::/48". IPv4-mapped IPv6 addresses are
// treated as IPv4 and zones are dropped. It returns "" for an invalid addr.
func Prefix(addr netip.Addr) string {
	if !addr.IsValid() {
		return ""
	}
	addr = addr.Unmap().WithZone("")
	bits := ipv6PrefixBits
	if addr.Is4() {
		bits = ipv4PrefixBits
	}
	p, err := addr.Prefix(bits)
	if err != nil {
		return ""
	}
	return p.String()
}

// PrefixString is Prefix for an unparsed caller-supplied string. Input that
// does not parse as an IP returns "": it may be anything, so it is never
// echoed into logs.
func PrefixString(raw string) string {
	addr, err := netip.ParseAddr(raw)
	if err != nil {
		return ""
	}
	return Prefix(addr)
}
