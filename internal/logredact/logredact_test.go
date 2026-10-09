package logredact

import (
	"net/netip"
	"testing"
)

func TestPrefixString(t *testing.T) {
	cases := []struct{ in, want string }{
		{"81.2.69.142", "81.2.69.0/24"},
		{"2a02:d500:1234:5678::1", "2a02:d500:1234::/48"},
		{"::ffff:81.2.69.142", "81.2.69.0/24"},
		{"fe80::1%eth0", "fe80::/48"},
		{"not-an-ip", ""},
		{"81.2.69.142:443", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := PrefixString(c.in); got != c.want {
			t.Errorf("PrefixString(%q)=%q, want %q", c.in, got, c.want)
		}
	}
}

func TestPrefix_invalidAddr(t *testing.T) {
	if got := Prefix(netip.Addr{}); got != "" {
		t.Errorf("Prefix(zero)=%q, want empty", got)
	}
}
