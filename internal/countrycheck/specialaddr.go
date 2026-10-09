package countrycheck

import "net/netip"

// Non-routable / special-use IP prefixes.
//
// Source: IANA IPv4 and IPv6 Special-Purpose Address Registries, checked
// 2026-10-09:
//
//	https://www.iana.org/assignments/iana-ipv4-special-registry/
//	https://www.iana.org/assignments/iana-ipv6-special-registry/
//
// For these addresses a GeoIP lookup gives no answer or a wrong one. We
// skip the lookup and return Decision=unknown, Reason=non_routable_address,
// and the gateway decides per customer (D2).
//
// Deliberate deviations from the registry's "globally reachable" column:
//   - 64:ff9b::/96 (NAT64) is marked globally reachable, but the address the
//     gateway sees is the translator, not the user. Treated as non-routable.
//   - 2001::/32 (Teredo) is N/A in the registry. MaxMind maps it onto the
//     IPv4 tree using the embedded Teredo *server* address, so a lookup
//     returns the relay's country, not the user's. Treated as non-routable.
//   - ::/96 (IPv4-compatible, deprecated by RFC 4291) is not in the
//     registry. MaxMind stores IPv4 data under ::/96, so "::a00:1" would
//     silently be looked up as 10.0.0.1. Treated as non-routable.

var (
	nonRoutableV4 = mustParsePrefixes(
		"0.0.0.0/8",       // "This network"
		"10.0.0.0/8",      // Private
		"100.64.0.0/10",   // Shared / CGN
		"127.0.0.0/8",     // Loopback
		"169.254.0.0/16",  // Link-local
		"172.16.0.0/12",   // Private
		"192.0.0.0/24",    // IETF protocol assignments (globally reachable sub-blocks carved out below)
		"192.0.2.0/24",    // Documentation (TEST-NET-1)
		"192.88.99.0/24",  // 6to4 relay anycast (deprecated)
		"192.168.0.0/16",  // Private
		"198.18.0.0/15",   // Benchmarking
		"198.51.100.0/24", // Documentation (TEST-NET-2)
		"203.0.113.0/24",  // Documentation (TEST-NET-3)
		"224.0.0.0/4",     // Multicast
		"240.0.0.0/4",     // Reserved, including 255.255.255.255
	)

	// routableV4Carveouts are globally reachable blocks nested inside a
	// nonRoutableV4 entry. They are checked first.
	routableV4Carveouts = mustParsePrefixes(
		"192.0.0.9/32",  // Port Control Protocol anycast
		"192.0.0.10/32", // TURN anycast
	)

	nonRoutableV6 = mustParsePrefixes(
		"::/96",          // Unspecified (::), loopback (::1), and deprecated IPv4-compatible
		"64:ff9b::/96",   // NAT64 well-known prefix (see deviations above)
		"64:ff9b:1::/48", // NAT64 local-use
		"100::/64",       // Discard-only
		"100:0:0:1::/64", // Dummy prefix (RFC 9780)
		"2001::/23",      // IETF protocol assignments, including Teredo (globally reachable sub-blocks carved out below)
		"2001:db8::/32",  // Documentation. Not inside 2001::/23, which ends at 2001:1ff.
		"3fff::/20",      // Documentation (RFC 9637)
		"5f00::/16",      // SRv6 SIDs (RFC 9602)
		"fc00::/7",       // Unique local
		"fe80::/10",      // Link-local
		"ff00::/8",       // Multicast
	)

	// routableV6Carveouts are globally reachable blocks nested inside a
	// nonRoutableV6 entry. They are checked first.
	routableV6Carveouts = mustParsePrefixes(
		"2001:1::1/128",   // Port Control Protocol anycast
		"2001:1::2/128",   // TURN anycast
		"2001:1::3/128",   // DNS-SD SRP anycast (RFC 9665)
		"2001:3::/32",     // AMT
		"2001:4:112::/48", // AS112-v6
		"2001:20::/28",    // ORCHIDv2
		"2001:30::/28",    // Drone Remote ID Protocol
	)
)

// isNonRoutable reports whether addr is on a special-purpose prefix that
// should not be geolocated. The caller must pass an unmapped address
// (IPv4-mapped IPv6 → IPv4); a zero Addr is non-routable.
func isNonRoutable(addr netip.Addr) bool {
	if !addr.IsValid() {
		return true
	}
	carveouts, blocked := routableV6Carveouts, nonRoutableV6
	if addr.Is4() {
		carveouts, blocked = routableV4Carveouts, nonRoutableV4
	}
	for _, p := range carveouts {
		if p.Contains(addr) {
			return false
		}
	}
	for _, p := range blocked {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

func mustParsePrefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(ss))
	for _, s := range ss {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			panic("countrycheck: bad special-address prefix " + s + ": " + err.Error())
		}
		out = append(out, p)
	}
	return out
}
