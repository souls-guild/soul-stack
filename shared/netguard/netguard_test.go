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

// --- SSRF-guard: классификатор адресов ---

func TestIsBlockedIP(t *testing.T) {
	blocked := []string{
		"169.254.169.254", // cloud metadata IAM (link-local)
		"169.254.1.1",     // link-local v4
		"169.254.0.10",    // link-local v4
		"127.0.0.1",       // loopback v4
		"127.10.20.30",    // loopback v4 (весь 127/8)
		"::1",             // loopback v6
		"10.0.0.5",        // RFC1918
		"172.16.0.1",      // RFC1918 нижняя граница
		"172.16.31.9",     // RFC1918
		"172.31.255.255",  // RFC1918 верхняя граница
		"192.168.1.1",     // RFC1918
		"0.0.0.0",         // unspecified v4
		"::",              // unspecified v6
		"fc00::1",         // ULA (private v6)
		"fe80::1",         // link-local v6
		// CGNAT / Shared Address Space (RFC 6598), границы /10.
		"100.64.0.0",      // CGNAT нижняя граница
		"100.64.0.1",      // CGNAT
		"100.100.50.50",   // CGNAT середина
		"100.127.255.255", // CGNAT верхняя граница
		// Устаревший IPv6 site-local (RFC 3879), границы /10.
		"fec0::",  // site-local нижняя граница
		"fec0::1", // site-local
		"feff:ffff:ffff:ffff:ffff:ffff:ffff:ffff", // site-local верхняя граница
		// IPv4-mapped IPv6: заблокированный v4 не должен обходиться через v6-форму.
		"::ffff:127.0.0.1",   // loopback через v6-mapped
		"::ffff:10.0.0.5",    // RFC1918 через v6-mapped
		"::ffff:10.0.0.1",    // RFC1918 через v6-mapped
		"::ffff:169.254.1.1", // link-local через v6-mapped
		"::ffff:100.64.0.1",  // CGNAT через v6-mapped
	}
	for _, s := range blocked {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("кривой тестовый IP %q", s)
		}
		if !netguard.IsBlockedIP(ip) {
			t.Errorf("IsBlockedIP(%q)=false, ожидался блок", s)
		}
	}

	allowed := []string{
		"8.8.8.8",              // публичный v4
		"1.1.1.1",              // публичный v4
		"172.32.0.1",           // вне RFC1918 (172.16–172.31)
		"172.15.0.1",           // вне RFC1918
		"203.0.113.10",         // публичный (TEST-NET-3, но не private/loopback)
		"2606:4700:4700::1111", // публичный v6
		"100.63.255.255",       // на 1 ниже CGNAT 100.64.0.0/10 — не CGNAT
		"100.128.0.0",          // на 1 выше CGNAT 100.64.0.0/10 — не CGNAT
		"::ffff:8.8.8.8",       // публичный v4 через v6-mapped — не должен блокироваться
		"2001:4860:4860::8888", // публичный v6 (вне fec0::/10 site-local)
	}
	for _, s := range allowed {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("кривой тестовый IP %q", s)
		}
		if netguard.IsBlockedIP(ip) {
			t.Errorf("IsBlockedIP(%q)=true, ожидался пропуск", s)
		}
	}
}

// --- https-only валидация ---

func TestValidateHTTPSURL(t *testing.T) {
	t.Run("https ok", func(t *testing.T) {
		for _, raw := range []string{
			"https://ok.example/x",
			"HTTPS://ok.example/x",
		} {
			if err := netguard.ValidateHTTPSURL(raw); err != nil {
				t.Errorf("ValidateHTTPSURL отверг валидный https %q: %v", raw, err)
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
				t.Errorf("ValidateHTTPSURL пропустил не-https %q", raw)
			}
		}
	})

	t.Run("malformed url reject", func(t *testing.T) {
		// Управляющий символ в URL делает url.Parse ошибкой.
		if err := netguard.ValidateHTTPSURL("https://ok.example/\x7f"); err == nil {
			t.Fatal("ValidateHTTPSURL пропустил кривой URL")
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

// --- редиректы: downgrade + лимит ---

// mkRedirReq собирает минимальный *http.Request только с URL — CheckRedirect
// смотрит лишь на req.URL.Scheme/Host.
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
				t.Errorf("CheckRedirect отверг валидный https %q: %v", raw, err)
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
				t.Errorf("CheckRedirect пропустил downgrade на %q", raw)
			}
		}
	})

	t.Run("redirect limit reject", func(t *testing.T) {
		via := make([]*http.Request, maxRedirects)
		if err := check(mkRedirReq(t, "https://ok.example/x"), via); err == nil {
			t.Fatalf("CheckRedirect не остановил цепочку на лимите %d", maxRedirects)
		}
		// На один hop меньше лимита — ещё разрешено.
		via = make([]*http.Request, maxRedirects-1)
		if err := check(mkRedirReq(t, "https://ok.example/x"), via); err != nil {
			t.Fatalf("CheckRedirect отверг цепочку короче лимита: %v", err)
		}
	})
}

// --- SSRF-guard: dial с resolve-then-check-then-dial ---

// fakeConn — пустышка net.Conn: dial-успех не должен реально открывать сокет.
type fakeConn struct{ net.Conn }

// recordingDial фиксирует addr, по которому реально пошёл бы dial (проверка
// rebind-safety: коннект идёт по проверенному IP, а не по имени).
func recordingDial(got *string) netguard.DialFunc {
	return func(_ context.Context, _, addr string) (net.Conn, error) {
		*got = addr
		return &fakeConn{}, nil
	}
}

// staticResolver — резолвер с заранее заданным набором IP для любого host-а.
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

// byHostResolver резолвит по карте host→IP (rebind / multi-IP кейсы).
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
			t.Fatal("metadata 169.254.169.254 не заблокирован")
		}
		if dialed != "" {
			t.Fatalf("dial выполнен по %q несмотря на блок", dialed)
		}
	})

	t.Run("literal loopback blocked", func(t *testing.T) {
		for _, addr := range []string{"127.0.0.1:80", "[::1]:443"} {
			var dialed string
			dc := netguard.GuardedDialContext(staticResolver{}, recordingDial(&dialed))
			if _, err := dc(context.Background(), "tcp", addr); err == nil {
				t.Errorf("loopback %q не заблокирован", addr)
			}
			if dialed != "" {
				t.Errorf("dial выполнен по %q (loopback)", dialed)
			}
		}
	})

	t.Run("literal RFC1918 blocked", func(t *testing.T) {
		for _, addr := range []string{"10.0.0.5:443", "192.168.1.1:443", "172.16.0.9:443"} {
			dc := netguard.GuardedDialContext(staticResolver{}, recordingDial(new(string)))
			if _, err := dc(context.Background(), "tcp", addr); err == nil {
				t.Errorf("RFC1918 %q не заблокирован", addr)
			}
		}
	})

	t.Run("literal CGNAT blocked", func(t *testing.T) {
		for _, addr := range []string{"100.64.0.1:443", "[::ffff:100.64.0.1]:443"} {
			var dialed string
			dc := netguard.GuardedDialContext(staticResolver{}, recordingDial(&dialed))
			if _, err := dc(context.Background(), "tcp", addr); err == nil {
				t.Errorf("CGNAT %q не заблокирован", addr)
			}
			if dialed != "" {
				t.Errorf("dial выполнен по %q (CGNAT)", dialed)
			}
		}
	})

	t.Run("literal IPv6 site-local blocked", func(t *testing.T) {
		var dialed string
		dc := netguard.GuardedDialContext(staticResolver{}, recordingDial(&dialed))
		if _, err := dc(context.Background(), "tcp", "[fec0::1]:443"); err == nil {
			t.Error("site-local fec0::1 не заблокирован")
		}
		if dialed != "" {
			t.Errorf("dial выполнен по %q (site-local)", dialed)
		}
	})

	t.Run("literal public IP allowed, dial by that IP", func(t *testing.T) {
		var dialed string
		dc := netguard.GuardedDialContext(staticResolver{}, recordingDial(&dialed))
		if _, err := dc(context.Background(), "tcp", "8.8.8.8:443"); err != nil {
			t.Fatalf("публичный IP заблокирован: %v", err)
		}
		if dialed != "8.8.8.8:443" {
			t.Fatalf("dial по %q, ожидался 8.8.8.8:443", dialed)
		}
	})

	t.Run("DNS name resolving to private IP blocked (rebind)", func(t *testing.T) {
		var dialed string
		dc := netguard.GuardedDialContext(staticResolver{addrs: []string{"169.254.169.254"}}, recordingDial(&dialed))
		if _, err := dc(context.Background(), "tcp", "rebind.evil.example:443"); err == nil {
			t.Fatal("DNS-имя, резолвящееся в metadata, не заблокировано (rebind открыт)")
		}
		if dialed != "" {
			t.Fatalf("dial выполнен по %q несмотря на rebind в metadata", dialed)
		}
	})

	t.Run("multi-IP with one private blocked entirely", func(t *testing.T) {
		var dialed string
		// Один публичный + один metadata: классическая попытка обхода.
		dc := netguard.GuardedDialContext(staticResolver{addrs: []string{"8.8.8.8", "169.254.169.254"}}, recordingDial(&dialed))
		if _, err := dc(context.Background(), "tcp", "mixed.evil.example:443"); err == nil {
			t.Fatal("multi-IP с приватным адресом не заблокирован (обход «public+metadata»)")
		}
		if dialed != "" {
			t.Fatalf("dial выполнен по %q несмотря на один приватный IP в наборе", dialed)
		}
	})

	t.Run("DNS name resolving to public IP allowed, dial by resolved IP", func(t *testing.T) {
		var dialed string
		dc := netguard.GuardedDialContext(byHostResolver{byHost: map[string][]net.IPAddr{
			"good.example": ipAddrs("8.8.8.8"),
		}}, recordingDial(&dialed))
		if _, err := dc(context.Background(), "tcp", "good.example:443"); err != nil {
			t.Fatalf("публичное DNS-имя заблокировано: %v", err)
		}
		// Dial по конкретному резолвнутому IP, а не по имени (rebind-safe).
		if dialed != "8.8.8.8:443" {
			t.Fatalf("dial по %q, ожидался 8.8.8.8:443 (по проверенному IP, не имени)", dialed)
		}
	})

	t.Run("no addresses surfaces error", func(t *testing.T) {
		dc := netguard.GuardedDialContext(byHostResolver{byHost: map[string][]net.IPAddr{"empty.example": nil}}, recordingDial(new(string)))
		if _, err := dc(context.Background(), "tcp", "empty.example:443"); err == nil {
			t.Fatal("host без A-записей не дал ошибку")
		}
	})

	t.Run("resolver error surfaces", func(t *testing.T) {
		dc := netguard.GuardedDialContext(staticResolver{err: errors.New("nxdomain")}, recordingDial(new(string)))
		if _, err := dc(context.Background(), "tcp", "nope.example:443"); err == nil {
			t.Fatal("ошибка резолва не проброшена")
		}
	})
}
