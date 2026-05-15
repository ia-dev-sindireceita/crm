package metashared

import "net"

// metaIPRanges is the static Meta-published source range list used by
// IsAllowedIP. The slice is deliberately a `var` (not a const, since
// Go consts cannot hold IPNet) so a follow-up PR can edit it the day
// Meta announces a change without touching call sites.
//
// Source: https://developers.facebook.com/docs/sharing/webhook
// and Meta's AS32934 published prefixes (last reviewed 2026-05). The
// list is intentionally conservative — we ship the well-known core
// ranges; operators add or trim entries via PR when Meta updates the
// published list. The function is described as best-effort in the
// package contract because (a) CDNs / proxies between Meta and our
// edge may rewrite the source IP and (b) Meta itself does not
// guarantee the static list is exhaustive. HMAC verification remains
// the primary defence; IsAllowedIP is defense-in-depth.
var metaIPRanges = mustParseCIDRs([]string{
	// IPv4 — AS32934 well-known webhook ranges.
	"31.13.24.0/21",
	"31.13.64.0/18",
	"66.220.144.0/20",
	"69.63.176.0/20",
	"69.171.224.0/19",
	"74.119.76.0/22",
	"103.4.96.0/22",
	"129.134.0.0/16",
	"157.240.0.0/16",
	"173.252.64.0/18",
	"179.60.192.0/22",
	"185.60.216.0/22",
	"204.15.20.0/22",
	// IPv6.
	"2401:db00::/32",
	"2620:0:1c00::/40",
	"2a03:2880::/32",
	"2a03:2881::/32",
	"2a03:2887::/32",
	"2a03:2888::/32",
})

// IsAllowedIP reports whether ip falls inside any of the Meta-published
// source ranges in metaIPRanges. Returns false for a nil ip.
//
// The function is "best effort": callers MUST NOT rely on it as the
// primary authentication signal (HMAC verification covers that). It is
// useful as a coarse pre-filter in front of the HMAC check when the
// edge can produce a trustworthy client IP — typically only when the
// terminator sits directly in front of Meta with no intermediate
// proxy. In production the proxy chain frequently rewrites the source,
// in which case operators should disable this check and rely on HMAC
// alone.
func IsAllowedIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	for _, r := range metaIPRanges {
		if r.Contains(ip) {
			return true
		}
	}
	return false
}

// mustParseCIDRs converts a string slice of CIDR notations into
// *net.IPNet. A bad CIDR is a programming error in the static list
// above and panics at package init so CI catches it instead of
// shipping a silently empty allowlist.
func mustParseCIDRs(cidrs []string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, ipnet, err := net.ParseCIDR(c)
		if err != nil {
			panic("metashared: invalid CIDR " + c + ": " + err.Error())
		}
		out = append(out, ipnet)
	}
	return out
}
