package countrycheck

import (
	"net/netip"
	"testing"
)

func TestIsNonRoutable_v4(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		// Routable public IPs.
		{"8.8.8.8", false},
		{"81.2.69.142", false},
		// Private RFC1918, with edges just outside each block.
		{"10.0.0.1", true},
		{"172.16.0.1", true},
		{"172.31.255.255", true},
		{"172.32.0.1", false},
		{"192.168.0.1", true},
		// Shared / CGN.
		{"100.64.0.1", true},
		{"100.127.255.255", true},
		{"100.128.0.1", false},
		// Loopback, "this network", link-local.
		{"127.0.0.1", true},
		{"0.0.0.0", true},
		{"169.254.1.1", true},
		// Multicast, reserved, broadcast.
		{"224.0.0.1", true},
		{"240.0.0.1", true},
		{"255.255.255.255", true},
		// Documentation and benchmarking.
		{"192.0.2.1", true},
		{"198.51.100.1", true},
		{"203.0.113.1", true},
		{"198.18.0.1", true},
		// 6to4 relay anycast.
		{"192.88.99.1", true},
		// IETF protocol block with its globally reachable carve-outs.
		{"192.0.0.1", true},
		{"192.0.0.8", true},
		{"192.0.0.9", false},  // PCP anycast
		{"192.0.0.10", false}, // TURN anycast
		{"192.0.0.11", true},
	}
	for _, c := range cases {
		if got := isNonRoutable(netip.MustParseAddr(c.ip)); got != c.want {
			t.Errorf("isNonRoutable(%q) = %v, want %v", c.ip, got, c.want)
		}
	}
}

func TestIsNonRoutable_v6(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		// Routable global unicast.
		{"2606:4700:4700::1111", false},
		{"2001:4860:4860::8888", false},
		// ::/96: unspecified, loopback, IPv4-compatible.
		{"::", true},
		{"::1", true},
		{"::a00:1", true},
		{"::808:808", true},
		// ULA, link-local, multicast.
		{"fd12:3456::1", true},
		{"fe80::1", true},
		{"ff02::1", true},
		// Documentation.
		{"2001:db8::1", true},
		{"3fff::1", true},
		// NAT64, discard, dummy, SRv6.
		{"64:ff9b::1", true},
		{"64:ff9b:1::1", true},
		{"100::1", true},
		{"100:0:0:1::1", true},
		{"5f00::1", true},
		// Teredo: MaxMind would geolocate the relay server, not the user.
		{"2001:0:4136:e378:8000:63bf:3fff:fdd2", true},
		// IETF protocol block and its globally reachable carve-outs, with
		// addresses just outside each carve-out.
		{"2001:2::1", true},
		{"2001:1::1", false}, // PCP anycast
		{"2001:1::2", false}, // TURN anycast
		{"2001:1::3", false}, // DNS-SD SRP anycast
		{"2001:1::4", true},
		{"2001:3::1", false},     // AMT
		{"2001:4:112::1", false}, // AS112-v6
		{"2001:4:113::1", true},
		{"2001:20::1", false}, // ORCHIDv2
		{"2001:30::1", false}, // DRIP
		{"2001:40::1", true},
	}
	for _, c := range cases {
		if got := isNonRoutable(netip.MustParseAddr(c.ip)); got != c.want {
			t.Errorf("isNonRoutable(%q) = %v, want %v", c.ip, got, c.want)
		}
	}
}

func TestIsNonRoutable_zeroAddrIsNonRoutable(t *testing.T) {
	if !isNonRoutable(netip.Addr{}) {
		t.Fatal("zero-value netip.Addr should classify as non-routable")
	}
}
