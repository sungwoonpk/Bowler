package ssrf

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestIsBlocked(t *testing.T) {
	blockedAddrs := []string{
		"127.0.0.1", "127.255.255.254", "10.0.0.1", "172.16.0.1", "172.31.255.255", "192.168.1.1",
		"169.254.169.254", // cloud metadata
		"169.254.0.1", "0.0.0.0", "100.64.0.1", "198.18.0.1", "224.0.0.1", "255.255.255.255", "240.0.0.1",
		"::1", "::", "fe80::1", "fc00::1", "fd12:3456::1", "ff02::1", "2001:db8::1",
		"::ffff:10.0.0.1",  // IPv4 private, written as IPv6
		"::ffff:127.0.0.1", // IPv4 loopback, written as IPv6
		"::ffff:169.254.169.254",
		"64:ff9b::a00:1", // NAT64 of 10.0.0.1
	}
	for _, s := range blockedAddrs {
		if !IsBlocked(netip.MustParseAddr(s)) {
			t.Errorf("%s should be blocked", s)
		}
	}
	allowed := []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "172.15.255.255", "172.32.0.1", "100.63.255.255", "2606:4700:4700::1111", "::ffff:8.8.8.8"}
	for _, s := range allowed {
		if IsBlocked(netip.MustParseAddr(s)) {
			t.Errorf("%s should be allowed", s)
		}
	}
	if !IsBlocked(netip.Addr{}) {
		t.Error("an invalid address must be treated as blocked")
	}
}

func TestControl(t *testing.T) {
	strict := Policy{}
	if err := strict.Control("tcp", "127.0.0.1:80", nil); !errors.Is(err, ErrBlocked) {
		t.Errorf("loopback: got %v", err)
	}
	if err := strict.Control("tcp6", "[::1]:443", nil); !errors.Is(err, ErrBlocked) {
		t.Errorf("IPv6 loopback: got %v", err)
	}
	if err := strict.Control("tcp", "8.8.8.8:443", nil); err != nil {
		t.Errorf("a public address: got %v", err)
	}
	if err := strict.Control("tcp", "not an address", nil); !errors.Is(err, ErrBlocked) {
		t.Errorf("an unreadable address must be refused, got %v", err)
	}
	if err := (Policy{AllowPrivate: true}).Control("tcp", "10.0.0.1:80", nil); err != nil {
		t.Errorf("AllowPrivate should let it through: %v", err)
	}
}

func TestValidateURL(t *testing.T) {
	strict := Policy{}
	good := []string{"https://example.com/hooks", "https://api.example.com:8443/x?y=1", "https://93.184.216.34/"}
	for _, u := range good {
		if _, err := strict.ValidateURL(u); err != nil {
			t.Errorf("%q should be accepted: %v", u, err)
		}
	}
	bad := map[string]string{
		"":                                     "empty",
		"http://example.com/":                  "plain http",
		"ftp://example.com/":                   "other scheme",
		"file:///etc/passwd":                   "file scheme",
		"javascript:alert(1)":                  "javascript scheme",
		"https://":                             "no host",
		"https://user:pass@example.com/":       "credentials",
		"https://example.com/#frag":            "fragment",
		"https://example.com:0/":               "port zero",
		"https://example.com:99999/":           "port too big",
		"https://localhost/":                   "localhost",
		"https://LOCALHOST:8443/":              "localhost in capitals",
		"https://api.localhost/":               "a localhost subdomain",
		"https://127.0.0.1/":                   "loopback literal",
		"https://169.254.169.254/latest":       "metadata literal",
		"https://[::1]/":                       "IPv6 loopback literal",
		"https://[::ffff:10.0.0.1]/":           "mapped private literal",
		"https://10.1.2.3/":                    "private literal",
		"https://" + strings.Repeat("a", 2100): "too long",
		"https://exa mple.com/":                "a space in the host",
	}
	for u, why := range bad {
		if _, err := strict.ValidateURL(u); err == nil {
			t.Errorf("%s (%q) should be rejected", why, u)
		}
	}
}

func TestPolicyFlags(t *testing.T) {
	if _, err := (Policy{AllowHTTP: true}).ValidateURL("http://example.com/"); err != nil {
		t.Errorf("AllowHTTP: %v", err)
	}
	if _, err := (Policy{AllowHTTP: true}).ValidateURL("http://127.0.0.1/"); err == nil {
		t.Error("AllowHTTP must not also allow private addresses")
	}
	dev := Policy{AllowPrivate: true, AllowHTTP: true}
	for _, u := range []string{"http://localhost:8080/", "http://127.0.0.1:9000/x", "http://10.0.0.5/"} {
		if _, err := dev.ValidateURL(u); err != nil {
			t.Errorf("development policy should accept %q: %v", u, err)
		}
	}
}

// A real connection: the guard must stop it before any bytes are sent.
func TestDialIsStoppedForRealConnections(t *testing.T) {
	hit := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit <- struct{}{}
		w.WriteHeader(204)
	}))
	defer srv.Close()

	client := func(p Policy) *http.Client {
		return &http.Client{
			Timeout:   2 * time.Second,
			Transport: &http.Transport{DialContext: (&net.Dialer{Control: p.Control}).DialContext},
		}
	}

	_, err := client(Policy{}).Get(srv.URL)
	if err == nil {
		t.Fatal("a request to a loopback server must fail under the default policy")
	}
	if !strings.Contains(err.Error(), "not allowed") && !errors.Is(err, ErrBlocked) {
		t.Errorf("the failure should name the guard, got %v", err)
	}
	select {
	case <-hit:
		t.Fatal("the server must never have been reached")
	default:
	}

	resp, err := client(Policy{AllowPrivate: true}).Get(srv.URL)
	if err != nil {
		t.Fatalf("with AllowPrivate the request should work: %v", err)
	}
	resp.Body.Close()
}

// A name that resolves to a private address is stopped at dial time, even though the URL looked fine.
func TestNameResolvingToLoopbackIsStopped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))

	// "localhost" resolves to 127.0.0.1 on every machine, so it stands in for a rebinding name here.
	u := "http://localhost:" + port + "/"
	c := &http.Client{
		Timeout:   2 * time.Second,
		Transport: &http.Transport{DialContext: (&net.Dialer{Control: Policy{}.Control}).DialContext},
	}
	if _, err := c.Get(u); err == nil {
		t.Fatal("a name that resolves to loopback must be refused at dial time")
	}
}

func TestParseAllowListAcceptsPrivateNetworks(t *testing.T) {
	good := []string{
		"10.20.0.0/16", "10.0.0.0/8", "192.168.5.0/24", "172.16.0.0/12", "127.0.0.1/32", "127.0.0.0/8", "100.64.0.0/11",
		"169.254.10.0/24", "fd12:3456::/48", "fc00::/8", "fe80::/10", "::1/128", " 10.1.0.0/16 ",
	}
	got, err := ParseAllowList(good)
	if err != nil || len(got) != len(good) {
		t.Fatalf("%v %v", got, err)
	}
	if got, err := ParseAllowList(nil); err != nil || len(got) != 0 {
		t.Fatalf("an empty list is no list: %v %v", got, err)
	}
}

func TestParseAllowListRefusesWhatWouldBeDangerous(t *testing.T) {
	bad := map[string]string{
		"":                       "empty",
		"garbage":                "not a network",
		"10.20.0.0":              "no length",
		"10.20.0.5/16":           "bits after the network part",
		"0.0.0.0/0":              "everything",
		"::/0":                   "everything, IPv6",
		"8.8.8.0/24":             "a public range",
		"10.0.0.0/7":             "reaches the public 11.0.0.0/8",
		"172.0.0.0/8":            "reaches public addresses around 172.16/12",
		"169.254.0.0/16":         "includes the metadata address",
		"169.254.169.254/32":     "is the metadata address",
		"169.254.169.0/24":       "includes the metadata address",
		"100.64.0.0/10":          "includes Alibaba's metadata address",
		"192.0.0.0/24":           "is outside the allowable ranges and includes Oracle's metadata address",
		"fc00::/6":               "reaches public IPv6",
		"fc00::/7":               "includes AWS's IPv6 metadata address",
		"fd00:ec2::254/128":      "is AWS's IPv6 metadata address",
		"fd00::/8":               "includes AWS's IPv6 metadata address",
		"64:ff9b::/96":           "NAT64 can name any IPv4 address, metadata included",
		"2002::/16":              "6to4 embeds an IPv4 address",
		"224.0.0.0/4":            "multicast",
		"::ffff:10.0.0.0/104":    "an IPv4 range written as IPv6",
		"2001:db8::/32":          "documentation range",
		"0.0.0.0/8":              "this network",
		"255.255.255.255/32":     "broadcast",
		"198.18.0.0/15":          "benchmarking range",
		"fe80::/9":               "reaches beyond link-local",
		"10.20.0.0/16 10.30.0.0": "two in one",
	}
	for entry, why := range bad {
		if _, err := ParseAllowList([]string{entry}); err == nil {
			t.Errorf("%s (%q) must be refused", why, entry)
		}
	}
	// One bad entry spoils the list, so a typo cannot quietly leave a range out.
	if _, err := ParseAllowList([]string{"10.20.0.0/16", "8.8.8.0/24"}); err == nil {
		t.Error("a list with one bad entry must be refused as a whole")
	}
	tooMany := make([]string, maxAllowCIDRs+1)
	for i := range tooMany {
		tooMany[i] = "10." + strconv.Itoa(i) + ".0.0/16"
	}
	if _, err := ParseAllowList(tooMany); err == nil {
		t.Error("the list has a size limit")
	}
	if _, err := ParseAllowList(tooMany[:maxAllowCIDRs]); err != nil {
		t.Errorf("exactly the limit is fine: %v", err)
	}
}

func allowing(t *testing.T, cidrs ...string) Policy {
	t.Helper()
	list, err := ParseAllowList(cidrs)
	if err != nil {
		t.Fatal(err)
	}
	return Policy{AllowCIDRs: list}
}

func TestAnAllowListOpensOnlyItsOwnRanges(t *testing.T) {
	p := allowing(t, "10.20.0.0/16", "fd12:3456::/48")
	for _, addr := range []string{"10.20.1.2:443", "10.20.255.255:443", "[fd12:3456::7]:443", "[::ffff:10.20.1.2]:443", "8.8.8.8:443"} {
		if err := p.Control("tcp", addr, nil); err != nil {
			t.Errorf("%s should pass: %v", addr, err)
		}
	}
	for _, addr := range []string{
		"10.21.0.1:443", "10.19.255.255:443", "127.0.0.1:443", "192.168.1.1:443", "169.254.169.254:80", "[::1]:443", "[fd12:3457::1]:443",
		"[::ffff:10.21.0.1]:443", "[64:ff9b::a9fe:a9fe]:80", "not an address",
	} {
		if err := p.Control("tcp", addr, nil); !errors.Is(err, ErrBlocked) {
			t.Errorf("%s must stay blocked, got %v", addr, err)
		}
	}
	if err := (Policy{}).Control("tcp", "10.20.1.2:443", nil); !errors.Is(err, ErrBlocked) {
		t.Errorf("with no list the range is blocked: %v", err)
	}
}

func TestAnAllowListChangesWhatURLsAreAccepted(t *testing.T) {
	p := allowing(t, "10.20.0.0/16")
	for _, u := range []string{"https://10.20.1.2/hook", "https://10.20.1.2:8443/hook", "https://[::ffff:10.20.1.2]/hook", "https://example.com/hook"} {
		if _, err := p.ValidateURL(u); err != nil {
			t.Errorf("%q should be accepted: %v", u, err)
		}
	}
	for _, u := range []string{"https://10.21.0.1/", "https://127.0.0.1/", "https://169.254.169.254/latest", "https://localhost/", "http://10.20.1.2/hook"} {
		if _, err := p.ValidateURL(u); err == nil {
			t.Errorf("%q should be rejected", u)
		}
	}
}

// A real connection to a server on this machine: allowed only by a list that names its range.
func TestDialHonoursTheAllowListForRealConnections(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer srv.Close()
	get := func(p Policy) error {
		c := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DialContext: (&net.Dialer{Control: p.Control}).DialContext, DisableKeepAlives: true}}
		resp, err := c.Get(srv.URL)
		if err == nil {
			resp.Body.Close()
		}
		return err
	}
	if err := get(allowing(t, "127.0.0.0/8")); err != nil {
		t.Fatalf("the server is inside the allowed range: %v", err)
	}
	if err := get(allowing(t, "10.20.0.0/16")); err == nil {
		t.Fatal("a list that does not name loopback must not let it through")
	}
	if err := get(Policy{}); err == nil {
		t.Fatal("no list, no loopback")
	}
}
