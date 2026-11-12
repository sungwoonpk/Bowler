package ssrf

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
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
