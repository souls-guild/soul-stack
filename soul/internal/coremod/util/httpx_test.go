package util_test

import (
	"context"
	"net"
	"net/http"
	stdurl "net/url"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"
)

// netguardBlocked recognizes an SSRF-guard denial (netguard.blockedErr) by
// text: a guarded dial to metadata is rejected synchronously with this
// signature, whereas with the guard lifted, dial fails with a network
// error/timeout without it.
func netguardBlocked(err error) bool {
	return err != nil && strings.Contains(err.Error(), "ssrf-guard blocked")
}

// Exhaustive coverage of the SSRF-guard logic (IP classifier,
// guardedDialContext with rebind / multi-IP cases, redirect-downgrade,
// https-only) lives in shared/netguard. Here — util's public wrappers for
// core.url / core.http: delegation is wired up and working, guard plumbing
// in NewHTTPClient is in place.

// mkRedirReq builds a minimal *http.Request with just a URL — CheckRedirect
// only looks at req.URL.Scheme/Host.
func mkRedirReq(t *testing.T, raw string) *http.Request {
	t.Helper()
	u, err := stdurl.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return &http.Request{URL: u}
}

func TestCheckRedirect(t *testing.T) {
	t.Run("https -> https ok", func(t *testing.T) {
		if err := util.CheckRedirect(mkRedirReq(t, "https://ok.example/x"), nil); err != nil {
			t.Errorf("CheckRedirect отверг валидный https: %v", err)
		}
	})

	t.Run("https -> non-https reject", func(t *testing.T) {
		for _, raw := range []string{
			"http://evil.example/x",
			"ftp://evil.example/x",
			"file:///etc/passwd",
		} {
			if err := util.CheckRedirect(mkRedirReq(t, raw), nil); err == nil {
				t.Errorf("CheckRedirect пропустил downgrade на %q", raw)
			}
		}
	})

	t.Run("redirect limit reject", func(t *testing.T) {
		via := make([]*http.Request, util.MaxRedirects)
		if err := util.CheckRedirect(mkRedirReq(t, "https://ok.example/x"), via); err == nil {
			t.Fatalf("CheckRedirect не остановил цепочку на лимите %d", util.MaxRedirects)
		}
		via = make([]*http.Request, util.MaxRedirects-1)
		if err := util.CheckRedirect(mkRedirReq(t, "https://ok.example/x"), via); err != nil {
			t.Fatalf("CheckRedirect отверг цепочку короче лимита: %v", err)
		}
	})
}

func TestValidateURL(t *testing.T) {
	t.Run("https ok", func(t *testing.T) {
		for _, raw := range []string{"https://ok.example/x", "HTTPS://ok.example/x"} {
			if err := util.ValidateURL(raw); err != nil {
				t.Errorf("ValidateURL отверг валидный https %q: %v", raw, err)
			}
		}
	})

	t.Run("non-https reject", func(t *testing.T) {
		for _, raw := range []string{"http://evil.example/x", "ftp://evil.example/x", "file:///etc/passwd"} {
			if err := util.ValidateURL(raw); err == nil {
				t.Errorf("ValidateURL пропустил не-https %q", raw)
			}
		}
	})

	t.Run("malformed url reject", func(t *testing.T) {
		if err := util.ValidateURL("https://ok.example/\x7f"); err == nil {
			t.Fatal("ValidateURL пропустил кривой URL")
		}
	})
}

func TestIsBlockedIP(t *testing.T) {
	for _, s := range []string{"169.254.169.254", "127.0.0.1", "10.0.0.5", "100.64.0.1", "::1", "fc00::1"} {
		if !util.IsBlockedIP(net.ParseIP(s)) {
			t.Errorf("IsBlockedIP(%q)=false, ожидался блок", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		if util.IsBlockedIP(net.ParseIP(s)) {
			t.Errorf("IsBlockedIP(%q)=true, ожидался пропуск", s)
		}
	}
}

func TestNewHTTPClient_GuardWiring(t *testing.T) {
	// Zero-value opts = the old NewHTTPClient(false): a maximally safe
	// client. Verifying behavioral equivalence.
	t.Run("zero opts: DialContext выставлен + downgrade-защита + TLS дефолт", func(t *testing.T) {
		c := util.NewHTTPClient(util.HTTPClientOpts{})
		tr, ok := c.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("Transport не *http.Transport: %T", c.Transport)
		}
		if tr.DialContext == nil {
			t.Fatal("AllowPrivate=false: DialContext не выставлен — SSRF-guard отключён")
		}
		if tr.TLSClientConfig != nil && tr.TLSClientConfig.InsecureSkipVerify {
			t.Fatal("zero opts: InsecureSkipVerify взведён без запроса")
		}
		if c.CheckRedirect == nil {
			t.Fatal("CheckRedirect не выставлен (downgrade-защита отключена)")
		}
		if err := c.CheckRedirect(mkRedirReq(t, "http://evil.example/x"), nil); err == nil {
			t.Fatal("NewHTTPClient.CheckRedirect пропустил downgrade https->http")
		}
		// Guard is actually wired in: a literal metadata IP never reaches dial.
		if _, err := tr.DialContext(context.Background(), "tcp", "169.254.169.254:443"); !netguardBlocked(err) {
			t.Fatalf("guard не заблокировал dial в metadata: %v", err)
		}
	})

	t.Run("AllowPrivate: guard снят, downgrade-защита сохранена", func(t *testing.T) {
		c := util.NewHTTPClient(util.HTTPClientOpts{AllowPrivate: true})
		tr, ok := c.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("Transport не *http.Transport: %T", c.Transport)
		}
		// http.DefaultTransport.Clone() carries a non-nil default DialContext;
		// with AllowPrivate=true we do NOT wrap it with netguard. Verifying
		// behaviorally: guard lifted — the metadata IP isn't rejected at
		// dial's check phase (the connection actually attempts to establish
		// and fails over the network, not via a netguard block). Without the
		// guard, the error is NOT a netguard verdict.
		if tr.DialContext == nil {
			t.Fatal("AllowPrivate=true: DialContext nil — дефолтный dialer потерян")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		_, derr := tr.DialContext(ctx, "tcp", "169.254.169.254:1")
		if derr != nil && netguardBlocked(derr) {
			t.Fatalf("AllowPrivate=true: guard НЕ снят, dial отвергнут netguard-ом: %v", derr)
		}
		if c.CheckRedirect == nil {
			t.Fatal("AllowPrivate=true: downgrade-защита редиректов потеряна")
		}
		if err := c.CheckRedirect(mkRedirReq(t, "http://evil.example/x"), nil); err == nil {
			t.Fatal("AllowPrivate=true: downgrade https->http не должен сниматься")
		}
	})

	t.Run("InsecureSkipVerify: TLSClientConfig.InsecureSkipVerify=true, прочее цело", func(t *testing.T) {
		c := util.NewHTTPClient(util.HTTPClientOpts{InsecureSkipVerify: true})
		tr, ok := c.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("Transport не *http.Transport: %T", c.Transport)
		}
		if tr.TLSClientConfig == nil || !tr.TLSClientConfig.InsecureSkipVerify {
			t.Fatal("InsecureSkipVerify=true: transport.TLSClientConfig.InsecureSkipVerify не взведён")
		}
		if tr.DialContext == nil {
			t.Fatal("InsecureSkipVerify не должен снимать SSRF-guard")
		}
		if err := c.CheckRedirect(mkRedirReq(t, "http://evil.example/x"), nil); err == nil {
			t.Fatal("InsecureSkipVerify не должен снимать downgrade-защиту редиректов")
		}
	})

	t.Run("AllowHTTPRedirect: http-hop допускается, не-http(s) отвергается", func(t *testing.T) {
		c := util.NewHTTPClient(util.HTTPClientOpts{AllowHTTPRedirect: true})
		if c.CheckRedirect == nil {
			t.Fatal("AllowHTTPRedirect: CheckRedirect не выставлен")
		}
		if err := c.CheckRedirect(mkRedirReq(t, "http://ok.example/x"), nil); err != nil {
			t.Fatalf("AllowHTTPRedirect=true: http-hop должен допускаться, got %v", err)
		}
		if err := c.CheckRedirect(mkRedirReq(t, "https://ok.example/x"), nil); err != nil {
			t.Fatalf("AllowHTTPRedirect=true: https-hop должен допускаться, got %v", err)
		}
		if err := c.CheckRedirect(mkRedirReq(t, "file:///etc/passwd"), nil); err == nil {
			t.Fatal("AllowHTTPRedirect=true: не-http(s) схема должна отвергаться")
		}
		// SSRF-guard is in place: AllowHTTPRedirect doesn't open up private.
		tr := c.Transport.(*http.Transport)
		if tr.DialContext == nil {
			t.Fatal("AllowHTTPRedirect не должен снимать SSRF-guard")
		}
	})

	t.Run("AllowHTTPRedirect off: downgrade https->http отвергается (netguard)", func(t *testing.T) {
		c := util.NewHTTPClient(util.HTTPClientOpts{})
		if err := c.CheckRedirect(mkRedirReq(t, "http://ok.example/x"), nil); err == nil {
			t.Fatal("AllowHTTPRedirect=false: downgrade https->http должен отвергаться")
		}
	})
}

func TestValidateFetchURL(t *testing.T) {
	t.Run("allowHTTP=false: только https", func(t *testing.T) {
		for _, raw := range []string{"https://ok.example/x", "HTTPS://ok.example/x"} {
			if err := util.ValidateFetchURL(raw, false); err != nil {
				t.Errorf("ValidateFetchURL(%q, false) отверг валидный https: %v", raw, err)
			}
		}
		for _, raw := range []string{"http://evil.example/x", "ftp://evil.example/x", "file:///etc/passwd"} {
			if err := util.ValidateFetchURL(raw, false); err == nil {
				t.Errorf("ValidateFetchURL(%q, false) пропустил не-https", raw)
			}
		}
	})

	t.Run("allowHTTP=true: http и https ок, прочее отвергнуто", func(t *testing.T) {
		for _, raw := range []string{"http://ok.example/x", "HTTP://ok.example/x", "https://ok.example/x", "HTTPS://ok.example/x"} {
			if err := util.ValidateFetchURL(raw, true); err != nil {
				t.Errorf("ValidateFetchURL(%q, true) отверг валидный http(s): %v", raw, err)
			}
		}
		for _, raw := range []string{"file:///etc/passwd", "ftp://evil.example/x", "gopher://evil.example"} {
			if err := util.ValidateFetchURL(raw, true); err == nil {
				t.Errorf("ValidateFetchURL(%q, true) пропустил не-http(s)", raw)
			}
		}
	})

	t.Run("allowHTTP=true: кривой URL отвергнут", func(t *testing.T) {
		if err := util.ValidateFetchURL("http://ok.example/\x7f", true); err == nil {
			t.Fatal("ValidateFetchURL пропустил кривой URL")
		}
	})
}

func TestWarnHost(t *testing.T) {
	t.Run("host без схемы/path/query", func(t *testing.T) {
		if got := util.WarnHost("https://svc.internal:8443/secret?token=leak"); got != "svc.internal:8443" {
			t.Fatalf("WarnHost=%q want svc.internal:8443", got)
		}
	})
	t.Run("кривой URL → ?", func(t *testing.T) {
		if got := util.WarnHost("::not-a-url"); got != "?" {
			t.Fatalf("WarnHost=%q want ?", got)
		}
	})
}

func TestGuardWarnings(t *testing.T) {
	t.Run("нулевые флаги → nil", func(t *testing.T) {
		if w := util.GuardWarnings("h", util.HTTPClientOpts{}); w != nil {
			t.Fatalf("warnings без снятых guard-ов: %v", w)
		}
	})

	t.Run("каждый флаг → своя формулировка с host", func(t *testing.T) {
		cases := []struct {
			opts util.HTTPClientOpts
			want string
		}{
			{util.HTTPClientOpts{InsecureSkipVerify: true}, "TLS verification disabled (insecure_skip_verify) for host.example"},
			{util.HTTPClientOpts{AllowHTTPRedirect: true}, "plaintext http allowed (allow_http) for host.example"},
			{util.HTTPClientOpts{AllowPrivate: true}, "SSRF-guard disabled (allow_private) for host.example"},
		}
		for _, c := range cases {
			w := util.GuardWarnings("host.example", c.opts)
			if len(w) != 1 || w[0] != c.want {
				t.Fatalf("GuardWarnings=%v want [%q]", w, c.want)
			}
		}
	})

	t.Run("все три → детерминированный порядок", func(t *testing.T) {
		w := util.GuardWarnings("h", util.HTTPClientOpts{
			AllowPrivate: true, InsecureSkipVerify: true, AllowHTTPRedirect: true,
		})
		want := []string{
			"TLS verification disabled (insecure_skip_verify) for h",
			"plaintext http allowed (allow_http) for h",
			"SSRF-guard disabled (allow_private) for h",
		}
		if len(w) != len(want) {
			t.Fatalf("len=%d want %d: %v", len(w), len(want), w)
		}
		for i := range want {
			if w[i] != want[i] {
				t.Fatalf("warnings[%d]=%q want %q", i, w[i], want[i])
			}
		}
	})
}

func TestStringsToAny(t *testing.T) {
	got := util.StringsToAny([]string{"a", "b"})
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("StringsToAny=%v", got)
	}
	if got := util.StringsToAny(nil); len(got) != 0 {
		t.Fatalf("StringsToAny(nil) не пустой: %v", got)
	}
}
