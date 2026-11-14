// Package ssrf keeps Bowler from being used to reach machines it should not.
//
// Bowler calls URLs that its customers choose. Left alone, a customer could register
// "http://169.254.169.254/latest/meta-data/" and have Bowler fetch the cloud credentials of the machine it runs on,
// or point it at a database on the private network. Two checks stop that:
//
//   - ValidateURL rejects addresses that are wrong on their face, so the person registering gets a clear error.
//   - Control runs when a connection is actually made, on the IP address the name resolved to. This is the check
//     that counts. A name can resolve to a public address when it is registered and to a private one later (DNS
//     rebinding), so only the address being dialed can be trusted.
package ssrf

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"syscall"
)

// ErrBlocked is returned when an address is not allowed.
var ErrBlocked = errors.New("ssrf: address is not allowed")

// MaxURLLength is the longest URL accepted.
const MaxURLLength = 2048

// Policy says what may be called. The zero value is the safe one: https only, public addresses only.
type Policy struct {
	// AllowPrivate permits loopback, private and link-local addresses. It is for local development, tests and
	// demos, where the receiver runs on the same machine. Never turn it on for a service that takes URLs from
	// strangers.
	AllowPrivate bool
	// AllowHTTP permits plain http URLs. Webhook bodies often carry personal data, so https is the default.
	AllowHTTP bool
	// AllowCIDRs are private ranges that may be called although AllowPrivate is off: the operator's own network, for
	// instance, and nothing else. Make the list with ParseAllowList, which refuses what would be dangerous. It is the
	// operator's setting and not a customer's, because a customer who could widen it would remove the protection.
	AllowCIDRs []netip.Prefix
}

// blocked lists every range that is not a normal public internet address.
var blocked = mustPrefixes(
	// IPv4
	"0.0.0.0/8",       // "this network"
	"10.0.0.0/8",      // private
	"100.64.0.0/10",   // carrier-grade NAT, often used for internal services
	"127.0.0.0/8",     // loopback
	"169.254.0.0/16",  // link-local, includes the cloud metadata address 169.254.169.254
	"172.16.0.0/12",   // private
	"192.0.0.0/24",    // IETF protocol assignments
	"192.0.2.0/24",    // documentation
	"192.88.99.0/24",  // 6to4 relay
	"192.168.0.0/16",  // private
	"198.18.0.0/15",   // benchmarking
	"198.51.100.0/24", // documentation
	"203.0.113.0/24",  // documentation
	"224.0.0.0/4",     // multicast
	"240.0.0.0/4",     // reserved, includes the broadcast address
	// IPv6
	"::/128",        // unspecified
	"::1/128",       // loopback
	"64:ff9b::/96",  // NAT64, can translate to a private IPv4 address
	"100::/64",      // discard
	"2001:db8::/32", // documentation
	"2002::/16",     // 6to4, embeds an IPv4 address
	"fc00::/7",      // unique local
	"fe80::/10",     // link-local
	"ff00::/8",      // multicast
)

func mustPrefixes(cidrs ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(cidrs))
	for i, c := range cidrs {
		out[i] = netip.MustParsePrefix(c)
	}
	return out
}

// IsBlocked reports whether ip is outside the public internet.
func IsBlocked(ip netip.Addr) bool {
	// An IPv4 address written as IPv6 ("::ffff:10.0.0.1") is still the IPv4 address.
	ip = ip.Unmap()
	if !ip.IsValid() {
		return true
	}
	for _, p := range blocked {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// metadataAddrs are the addresses where cloud providers hand out the credentials of the machine. They are the usual target
// of this kind of attack, so no allow list may include one, however it is written.
var metadataAddrs = []netip.Addr{
	netip.MustParseAddr("169.254.169.254"), // AWS, Google Cloud, Azure and most others
	netip.MustParseAddr("fd00:ec2::254"),   // AWS, over IPv6
	netip.MustParseAddr("100.100.100.200"), // Alibaba Cloud
	netip.MustParseAddr("192.0.0.192"),     // Oracle Cloud
}

// maxAllowCIDRs bounds the list, since every connection is checked against it.
const maxAllowCIDRs = 64

// allowable are the only blocked ranges that an allow list may reach into: the ones people really keep receivers in. The
// others are refused. Some cannot be judged, such as the NAT64 and 6to4 ranges, which carry an IPv4 address inside them and
// so could be used to name a metadata address another way. Others are never a place to send a webhook, such as the
// multicast and documentation ranges.
var allowable = mustPrefixes(
	"10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12", "192.168.0.0/16",
	"::1/128", "fc00::/7", "fe80::/10",
)

// ParseAllowList reads the ranges of an allow list. Each must be a network written as it is (10.20.0.0/16, not
// 10.20.0.5/16), lie wholly inside one of the allowable private ranges, and not include a cloud metadata address. A range
// that reaches public addresses would allow more than the operator can have meant, so it is refused with the reason.
func ParseAllowList(entries []string) ([]netip.Prefix, error) {
	if len(entries) > maxAllowCIDRs {
		return nil, fmt.Errorf("at most %d ranges may be allowed", maxAllowCIDRs)
	}
	out := make([]netip.Prefix, 0, len(entries))
	for _, raw := range entries {
		raw = strings.TrimSpace(raw)
		p, err := netip.ParsePrefix(raw)
		if err != nil {
			return nil, fmt.Errorf("%q is not a network such as 10.20.0.0/16", raw)
		}
		if p.Addr().Is4In6() {
			return nil, fmt.Errorf("%q: write an IPv4 range as IPv4", raw)
		}
		if p != p.Masked() {
			return nil, fmt.Errorf("%q has bits set after the network part, did you mean %s", raw, p.Masked())
		}
		if !insideAllowable(p) {
			return nil, fmt.Errorf("%q is not inside a private range that may be allowed (10.0.0.0/8, 100.64.0.0/10, 127.0.0.0/8, 169.254.0.0/16, 172.16.0.0/12, 192.168.0.0/16, ::1/128, fc00::/7, fe80::/10)", raw)
		}
		for _, m := range metadataAddrs {
			if p.Contains(m) {
				return nil, fmt.Errorf("%q includes %s, where cloud providers hand out credentials, and cannot be allowed", raw, m)
			}
		}
		out = append(out, p)
	}
	return out, nil
}

// insideAllowable reports whether every address of p is in one allowable range.
func insideAllowable(p netip.Prefix) bool {
	for _, b := range allowable {
		if b.Addr().Is4() == p.Addr().Is4() && b.Bits() <= p.Bits() && b.Contains(p.Addr()) {
			return true
		}
	}
	return false
}

// blocks reports whether the policy refuses ip: it is outside the public internet and not in the operator's allow list.
func (p Policy) blocks(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !IsBlocked(ip) {
		return false
	}
	if !ip.IsValid() {
		return true
	}
	for _, c := range p.AllowCIDRs {
		if c.Contains(ip) {
			return false
		}
	}
	return true
}

// Control is a net.Dialer Control function. It is called with the resolved address just before each connection
// attempt and refuses addresses the policy does not allow.
//
//	dialer := &net.Dialer{Control: policy.Control}
func (p Policy) Control(_, address string, _ syscall.RawConn) error {
	if p.AllowPrivate {
		return nil
	}
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("%w: cannot read %q", ErrBlocked, address)
	}
	if p.blocks(ap.Addr()) {
		return fmt.Errorf("%w: %s is not a public address", ErrBlocked, ap.Addr())
	}
	return nil
}

// ValidateURL checks a URL a customer wants Bowler to call. It cannot see what a name will resolve to, so it only
// catches addresses written out in the URL. Control does the rest.
func (p Policy) ValidateURL(raw string) (*url.URL, error) {
	if raw == "" || len(raw) > MaxURLLength {
		return nil, fmt.Errorf("url must be between 1 and %d characters", MaxURLLength)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("url is not valid")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !p.AllowHTTP {
			return nil, errors.New("url must use https")
		}
	default:
		return nil, errors.New("url must use https")
	}
	if u.User != nil {
		return nil, errors.New("url must not contain a username or password")
	}
	host := u.Hostname()
	if host == "" {
		return nil, errors.New("url has no host")
	}
	if u.Fragment != "" {
		return nil, errors.New("url must not contain a fragment")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, errors.New("url has an invalid port")
		}
	}
	if !p.AllowPrivate {
		if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
			return nil, errors.New("url must not point at this machine")
		}
		if ip, err := netip.ParseAddr(host); err == nil && p.blocks(ip) {
			return nil, errors.New("url must not point at a private or reserved address")
		}
	}
	return u, nil
}
