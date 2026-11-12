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
	if IsBlocked(ap.Addr()) {
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
		if ip, err := netip.ParseAddr(host); err == nil && IsBlocked(ip) {
			return nil, errors.New("url must not point at a private or reserved address")
		}
	}
	return u, nil
}
