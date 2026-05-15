package metashared_test

import (
	"net"
	"testing"

	"github.com/pericles-luz/crm/internal/adapter/channels/metashared"
)

func TestIsAllowedIP_KnownMetaRanges(t *testing.T) {
	t.Parallel()
	cases := []string{
		// One IP from inside each documented IPv4 range.
		"31.13.24.1",
		"31.13.65.10",
		"66.220.144.20",
		"69.63.176.30",
		"69.171.224.40",
		"74.119.76.50",
		"103.4.96.60",
		"129.134.0.70",
		"157.240.0.80",
		"173.252.64.90",
		"179.60.192.100",
		"185.60.216.110",
		"204.15.20.120",
		// IPv6 representatives.
		"2401:db00::1",
		"2620:0:1c00::1",
		"2a03:2880::1",
		"2a03:2881::1",
		"2a03:2887::1",
		"2a03:2888::1",
	}
	for _, ip := range cases {
		ip := ip
		t.Run(ip, func(t *testing.T) {
			t.Parallel()
			parsed := net.ParseIP(ip)
			if parsed == nil {
				t.Fatalf("ParseIP(%q) returned nil", ip)
			}
			if !metashared.IsAllowedIP(parsed) {
				t.Fatalf("IsAllowedIP(%q) = false, want true", ip)
			}
		})
	}
}

func TestIsAllowedIP_OutsideRanges(t *testing.T) {
	t.Parallel()
	cases := []string{
		// Private RFC1918 / loopback.
		"10.0.0.1",
		"192.168.1.1",
		"172.16.5.7",
		"127.0.0.1",
		// Public but not Meta.
		"8.8.8.8",
		"1.1.1.1",
		"203.0.113.4",
		// IPv6 outside Meta.
		"::1",
		"2001:db8::1",
	}
	for _, ip := range cases {
		ip := ip
		t.Run(ip, func(t *testing.T) {
			t.Parallel()
			parsed := net.ParseIP(ip)
			if parsed == nil {
				t.Fatalf("ParseIP(%q) returned nil", ip)
			}
			if metashared.IsAllowedIP(parsed) {
				t.Fatalf("IsAllowedIP(%q) = true, want false", ip)
			}
		})
	}
}

func TestIsAllowedIP_NilIP(t *testing.T) {
	t.Parallel()
	if metashared.IsAllowedIP(nil) {
		t.Fatal("IsAllowedIP(nil) = true, want false")
	}
}

// TestIsAllowedIP_EdgesOfRange exercises the inclusive lower and upper
// bound of one documented range to defend the test against an
// off-by-one in the CIDR-parse path.
func TestIsAllowedIP_EdgesOfRange(t *testing.T) {
	t.Parallel()
	// 31.13.24.0/21 covers 31.13.24.0 – 31.13.31.255.
	in := []string{"31.13.24.0", "31.13.31.255"}
	out := []string{"31.13.23.255", "31.13.32.0"}
	for _, ip := range in {
		if !metashared.IsAllowedIP(net.ParseIP(ip)) {
			t.Errorf("IsAllowedIP(%q) = false at range edge", ip)
		}
	}
	for _, ip := range out {
		if metashared.IsAllowedIP(net.ParseIP(ip)) {
			t.Errorf("IsAllowedIP(%q) = true just outside range", ip)
		}
	}
}
