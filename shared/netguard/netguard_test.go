package netguard_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	stdurl "net/url"
	"testing"

	"github.com/souls-guild/soul-stack/shared/netguard"
)

// --- SSRF guard: address classifier ---

func TestIsBlockedIP(t *testing.T) {
	blocked := []string{
		"169.254.169.254", // cloud metadata IAM (link-local)
		"169.254.1.1",     // link-local v4
		"169.254.0.10",    // link-local v4
		"127.0.0.1",       // loopback v4
		"127.10.20.30",    // loopback v4 (all of 127/8)
		"::1",             // loopback v6
		"10.0.0.5",        // RFC1918
		"172.16.0.1",      // RFC1918 lower bound
		"172.16.31.9",     // RFC1918
		"172.31.255.255",  // RFC1918 upper bound
		"192.168.1.1",     // RFC1918
		"0.0.0.0",         // unspecified v4
		"::",              // unspecified v6
		"fc00::1",         // ULA (private v6)
		"fe80::1",         // link-local v6
		// CGNAT / Shared Address Space (RFC 6598), /10 bounds.
		"100.64.0.0",      // CGNAT lower bound
		"100.64.0.1",      // CGNAT
		"100.100.50.50",   // CGNAT middle
		"100.127.255.255", // CGNAT upper bound
		// Legacy IPv6 site-local (RFC 3879), /10 bounds.
		"fec0::",  // site-local lower bound
		"fec0::1", // site-local
		"feff:ffff:ffff:ffff:ffff:ffff:ffff:ffff", // site-local upper bound
		// IPv4-mapped IPv6: a blocked v4 must not be bypassed via the v6 form.
		"::ffff:127.0.0.1",   // loopback via v6-mapped
		"::ffff:10.0.0.5",    // RFC1918 via v6-mapped
		"::ffff:10.0.0.1",    // RFC1918 via v6-mapped
		"::ffff:169.254.1.1", // link-local via v6-mapped
		"::ffff:100.64.0.1",  // CGNAT via v6-mapped
	}
	for _, s := range blocked {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("invalid test IP %q", s)
		}
		if !netguard.IsBlockedIP(ip) {
			t.Errorf("IsBlockedIP(%q)=false, expected a block", s)
		}
	}

	allowed := []string{
		"8.8.8.8",              // public v4
		"1.1.1.1",              // public v4
		"172.32.0.1",           // outside RFC1918 (172.16–172.31)
		"172.15.0.1",           // outside RFC1918
		"203.0.113.10",         // public (TEST-NET-3, but not private/loopback)
		"2606:4700:4700::1111", // public v6
		"100.63.255.255",       // one below CGNAT 100.64.0.0/10 — not CGNAT
		"100.128.0.0",          // one above CGNAT 100.64.0.0/10 — not CGNAT
		"::ffff:8.8.8.8",       // public v4 via v6-mapped — must not be blocked
		"2001:4860:4860::8888", // public v6 (outside fec0::/10 site-local)
	}
	for _, s := range allowed {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("invalid test IP %q", s)
		}
		if netguard.IsBlockedIP(ip) {
			t.Errorf("IsBlockedIP(%q)=true, expected a pass", s)
		}
	}
}

// --- https-only validation ---

func TestValidateHTTPSURL(t *testing.T) {
	t.Run("https ok", func(t *testing.T) {
		for _, raw := range []string{
			"https://ok.example/x",
			"HTTPS://ok.example/x",
		} {
			if err := netguard.ValidateHTTPSURL(raw); err != nil {
				t.Errorf("ValidateHTTPSURL rejected a valid https %q: %v", raw, err)
			}
		}
	})

	t.Run("non-https reject", func(t *testing.T) {
		for _, raw := range []string{
			"http://evil.example/x",
			"ftp://evil.example/x",
			"file:///etc/passwd",
		} {
			if err := netguard.ValidateHTTPSURL(raw); err == nil {
				t.Errorf("ValidateHTTPSURL let through a non-https %q", raw)
			}
		}
	})

	t.Run("malformed url reject", func(t *testing.T) {
		// A control character in the URL makes url.Parse fail.
		if err := netguard.ValidateHTTPSURL("https://ok.example/\x7f"); err == nil {
			t.Fatal("ValidateHTTPSURL let through a malformed URL")
		}
	})
}

func TestValidateEndpoint(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{"https public host", "https://prom.example.com:9090", false},
		{"http denied", "http://prom.example.com:9090", true},
		{"http metadata denied", "http://169.254.169.254/latest", true},
		{"https literal metadata IP", "https://169.254.169.254/", true},
		{"https loopback literal", "https://127.0.0.1:9090", true},
		{"https rfc1918 literal", "https://10.1.2.3:9090", true},
		{"file scheme denied", "file:///etc/passwd", true},
		{"no host", "https://", true},
		{"newline smuggle", "https://\nhttp://evil", true},
		{"https public literal IP", "https://8.8.8.8:9090", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := netguard.ValidateEndpoint(c.url)
			if (err != nil) != c.wantErr {
				t.Errorf("ValidateEndpoint(%q) err=%v, wantErr=%v", c.url, err, c.wantErr)
			}
		})
	}
}

// --- redirects: downgrade + limit ---

// mkRedirReq builds a minimal *http.Request with just a URL — CheckRedirect
// looks only at req.URL.Scheme/Host.
func mkRedirReq(t *testing.T, raw string) *http.Request {
	t.Helper()
	u, err := stdurl.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return &http.Request{URL: u}
}

func TestNewCheckRedirect(t *testing.T) {
	const maxRedirects = 10
	check := netguard.NewCheckRedirect(maxRedirects)

	t.Run("https -> https ok", func(t *testing.T) {
		for _, raw := range []string{
			"https://ok.example/x",
			"HTTPS://ok.example/x",
		} {
			if err := check(mkRedirReq(t, raw), nil); err != nil {
				t.Errorf("CheckRedirect rejected a valid https %q: %v", raw, err)
			}
		}
	})

	t.Run("https -> non-https reject", func(t *testing.T) {
		for _, raw := range []string{
			"http://evil.example/x",
			"HTTP://evil.example/x",
			"ftp://evil.example/x",
			"file:///etc/passwd",
		} {
			if err := check(mkRedirReq(t, raw), nil); err == nil {
				t.Errorf("CheckRedirect let through a downgrade to %q", raw)
			}
		}
	})

	t.Run("redirect limit reject", func(t *testing.T) {
		via := make([]*http.Request, maxRedirects)
		if err := check(mkRedirReq(t, "https://ok.example/x"), via); err == nil {
			t.Fatalf("CheckRedirect did not stop the chain at the limit %d", maxRedirects)
		}
		// One hop below the limit — still allowed.
		via = make([]*http.Request, maxRedirects-1)
		if err := check(mkRedirReq(t, "https://ok.example/x"), via); err != nil {
			t.Fatalf("CheckRedirect rejected a chain shorter than the limit: %v", err)
		}
	})
}

// --- SSRF guard: dial with resolve-then-check-then-dial ---

// fakeConn is a dummy net.Conn: a successful dial must not actually open a socket.
type fakeConn struct{ net.Conn }

// recordingDial records the addr the dial would actually use (rebind-safety
// check: the connect goes to the verified IP, not the name).
func recordingDial(got *string) netguard.DialFunc {
	return func(_ context.Context, _, addr string) (net.Conn, error) {
		*got = addr
		return &fakeConn{}, nil
	}
}

// staticResolver returns a preset set of IPs for any host.
type staticResolver struct {
	addrs []string
	err   error
}

func (s staticResolver) LookupIPAddr(_ context.Context, _ string) ([]net.IPAddr, error) {
	if s.err != nil {
		return nil, s.err
	}
	out := make([]net.IPAddr, len(s.addrs))
	for i, a := range s.addrs {
		out[i] = net.IPAddr{IP: net.ParseIP(a)}
	}
	return out, nil
}

// byHostResolver resolves via a host→IP map (rebind / multi-IP cases).
type byHostResolver struct {
	byHost map[string][]net.IPAddr
	err    error
}

func (r byHostResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.byHost[host], nil
}

func ipAddrs(ips ...string) []net.IPAddr {
	out := make([]net.IPAddr, 0, len(ips))
	for _, s := range ips {
		out = append(out, net.IPAddr{IP: net.ParseIP(s)})
	}
	return out
}

func TestGuardedDialContext(t *testing.T) {
	t.Run("literal metadata IP blocked, no dial", func(t *testing.T) {
		var dialed string
		dc := netguard.GuardedDialContext(staticResolver{}, recordingDial(&dialed))
		_, err := dc(context.Background(), "tcp", "169.254.169.254:443")
		if err == nil {
			t.Fatal("metadata 169.254.169.254 not blocked")
		}
		if dialed != "" {
			t.Fatalf("dial performed to %q despite the block", dialed)
		}
	})

	t.Run("literal loopback blocked", func(t *testing.T) {
		for _, addr := range []string{"127.0.0.1:80", "[::1]:443"} {
			var dialed string
			dc := netguard.GuardedDialContext(staticResolver{}, recordingDial(&dialed))
			if _, err := dc(context.Background(), "tcp", addr); err == nil {
				t.Errorf("loopback %q not blocked", addr)
			}
			if dialed != "" {
				t.Errorf("dial performed to %q (loopback)", dialed)
			}
		}
	})

	t.Run("literal RFC1918 blocked", func(t *testing.T) {
		for _, addr := range []string{"10.0.0.5:443", "192.168.1.1:443", "172.16.0.9:443"} {
			dc := netguard.GuardedDialContext(staticResolver{}, recordingDial(new(string)))
			if _, err := dc(context.Background(), "tcp", addr); err == nil {
				t.Errorf("RFC1918 %q not blocked", addr)
			}
		}
	})

	t.Run("literal CGNAT blocked", func(t *testing.T) {
		for _, addr := range []string{"100.64.0.1:443", "[::ffff:100.64.0.1]:443"} {
			var dialed string
			dc := netguard.GuardedDialContext(staticResolver{}, recordingDial(&dialed))
			if _, err := dc(context.Background(), "tcp", addr); err == nil {
				t.Errorf("CGNAT %q not blocked", addr)
			}
			if dialed != "" {
				t.Errorf("dial performed to %q (CGNAT)", dialed)
			}
		}
	})

	t.Run("literal IPv6 site-local blocked", func(t *testing.T) {
		var dialed string
		dc := netguard.GuardedDialContext(staticResolver{}, recordingDial(&dialed))
		if _, err := dc(context.Background(), "tcp", "[fec0::1]:443"); err == nil {
			t.Error("site-local fec0::1 not blocked")
		}
		if dialed != "" {
			t.Errorf("dial performed to %q (site-local)", dialed)
		}
	})

	t.Run("literal public IP allowed, dial by that IP", func(t *testing.T) {
		var dialed string
		dc := netguard.GuardedDialContext(staticResolver{}, recordingDial(&dialed))
		if _, err := dc(context.Background(), "tcp", "8.8.8.8:443"); err != nil {
			t.Fatalf("public IP blocked: %v", err)
		}
		if dialed != "8.8.8.8:443" {
			t.Fatalf("dial to %q, expected 8.8.8.8:443", dialed)
		}
	})

	t.Run("DNS name resolving to private IP blocked (rebind)", func(t *testing.T) {
		var dialed string
		dc := netguard.GuardedDialContext(staticResolver{addrs: []string{"169.254.169.254"}}, recordingDial(&dialed))
		if _, err := dc(context.Background(), "tcp", "rebind.evil.example:443"); err == nil {
			t.Fatal("a DNS name resolving to metadata is not blocked (rebind open)")
		}
		if dialed != "" {
			t.Fatalf("dial performed to %q despite rebind to metadata", dialed)
		}
	})

	t.Run("multi-IP with one private blocked entirely", func(t *testing.T) {
		var dialed string
		// One public + one metadata: the classic bypass attempt.
		dc := netguard.GuardedDialContext(staticResolver{addrs: []string{"8.8.8.8", "169.254.169.254"}}, recordingDial(&dialed))
		if _, err := dc(context.Background(), "tcp", "mixed.evil.example:443"); err == nil {
			t.Fatal("multi-IP with a private address not blocked (bypass of 'public+metadata')")
		}
		if dialed != "" {
			t.Fatalf("dial performed to %q despite one private IP in the set", dialed)
		}
	})

	t.Run("DNS name resolving to public IP allowed, dial by resolved IP", func(t *testing.T) {
		var dialed string
		dc := netguard.GuardedDialContext(byHostResolver{byHost: map[string][]net.IPAddr{
			"good.example": ipAddrs("8.8.8.8"),
		}}, recordingDial(&dialed))
		if _, err := dc(context.Background(), "tcp", "good.example:443"); err != nil {
			t.Fatalf("public DNS name blocked: %v", err)
		}
		// Dial to the specific resolved IP, not the name (rebind-safe).
		if dialed != "8.8.8.8:443" {
			t.Fatalf("dial to %q, expected 8.8.8.8:443 (by the checked IP, not the name)", dialed)
		}
	})

	t.Run("no addresses surfaces error", func(t *testing.T) {
		dc := netguard.GuardedDialContext(byHostResolver{byHost: map[string][]net.IPAddr{"empty.example": nil}}, recordingDial(new(string)))
		if _, err := dc(context.Background(), "tcp", "empty.example:443"); err == nil {
			t.Fatal("host without A records did not produce an error")
		}
	})

	t.Run("resolver error surfaces", func(t *testing.T) {
		dc := netguard.GuardedDialContext(staticResolver{err: errors.New("nxdomain")}, recordingDial(new(string)))
		if _, err := dc(context.Background(), "tcp", "nope.example:443"); err == nil {
			t.Fatal("resolve error not propagated")
		}
	})
}
