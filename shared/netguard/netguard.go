// Package netguard — общий SSRF egress-guard для исходящего HTTP к НЕдоверенным
// endpoint-ам. Единая точка для обеих сторон Soul Stack: Augur-брокеры Keeper-а
// (prom/elk, keeper/internal/augur) и core-HTTP-модули Soul-а (core.url /
// core.http, soul/internal/coremod). Два независимо эволюционирующих SSRF-guard-а
// в security-критичном коде — риск расхождения; этот пакет его устраняет.
//
// Защитная модель (одна для всех потребителей):
//   - только https:// (downgrade-защита; SSRF к metadata через
//     http://169.254.169.254 — частый вектор);
//   - resolve-then-check-then-dial по фактически резолвнутому IP (rebind-safe):
//     ВСЕ резолвнутые IP проверяются на [IsBlockedIP], dial идёт по уже
//     проверенному конкретному IP, а не по имени — между проверкой и
//     соединением нет второго резолва;
//   - блок редиректов на не-https и цепочек длиннее лимита ([NewCheckRedirect]).
//
// Soul-safe ([ADR-011]): только net/http stdlib, без server-only зависимостей и
// без импорта keeper-/soul-internal. Назван netguard по образцу shared/tlsx —
// инфра-утилита, суффикс описывает домен (сетевой guard), не конфликтует со
// stdlib net.
//
// [ADR-011]: docs/adr/0011-go-layout.md#adr-011-раскладка-go-кода-gowork-с-модулями-по-сторонам
package netguard

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// extraBlockedNets — CIDR-диапазоны вне классификаторов stdlib
// (net.IP.IsPrivate/IsLoopback/…), блокируемые симметрично как
// «маршрутизируемые во внутренние сети»:
//
//   - 100.64.0.0/10 — CGNAT / Shared Address Space (RFC 6598): carrier-grade NAT,
//     часто видимо во внутренних сетях и не отлавливается IsPrivate.
//   - fec0::/10 — устаревший IPv6 site-local (RFC 3879): deprecated, но всё ещё
//     может резолвиться/маршрутизироваться в legacy-сетях; не входит в
//     IsLinkLocalUnicast (fe80::/10).
//
// net.IPNet.Contains нормализует IPv4-mapped IPv6 (::ffff:100.64.0.1) к v4-форме,
// поэтому CGNAT-проверка автоматически закрывает и v6-mapped обход. Остальные
// уже заблокированные классы (loopback/RFC1918/ULA/link-local) тоже отрабатывают
// в v4-mapped-форме, так как stdlib-классификаторы используют To4().
var extraBlockedNets = []*net.IPNet{
	mustCIDR("100.64.0.0/10"),
	mustCIDR("fec0::/10"),
}

func mustCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(fmt.Sprintf("netguard: неверный CIDR %q в блок-листе SSRF-guard: %v", s, err))
	}
	return n
}

// IsBlockedIP классифицирует IP как недопустимый для исходящего HTTP к
// НЕдоверенному endpoint-у: loopback (127.0.0.0/8, ::1), приватные RFC1918/ULA
// (10/172.16/192.168, fc00::/7), link-local (169.254.0.0/16 — включает
// cloud-metadata 169.254.169.254 — и fe80::/10), CGNAT (100.64.0.0/10, RFC 6598),
// устаревший IPv6 site-local (fec0::/10, RFC 3879) и unspecified (0.0.0.0, ::).
// Возврат true означает «соединение запрещено».
//
// IPv4-mapped IPv6 (::ffff:0:0/96) обхода не даёт: stdlib-классификаторы и
// net.IPNet.Contains нормализуют такие адреса к v4-форме, поэтому заблокированный
// IPv4 остаётся заблокированным и в v6-форме.
//
// Экспортирована для unit-тестирования классификатора в изоляции (без выхода в
// сеть и без сборки клиента).
func IsBlockedIP(ip net.IP) bool {
	if ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() {
		return true
	}
	for _, n := range extraBlockedNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// ValidateHTTPSURL принимает только https://. http:// — downgrade-риск, file:// —
// чтение локальной ФС в обход модели доступа; всё, кроме https, отвергается.
//
// Разбор через url.Parse (не строковый префикс): схема сверяется
// регистронезависимо (валидный `HTTPS://` проходит) и по-настоящему
// (`https://\nhttp://evil` не пролезает через наивный HasPrefix).
//
// Литеральный IP в host НЕ проверяется здесь — это делает [ValidateEndpoint]
// (быстрый отказ до сборки клиента) и в любом случае dial-фаза по резолвнутому
// IP ([GuardedDialContext]).
func ValidateHTTPSURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("netguard: invalid url %q", rawURL)
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return fmt.Errorf("netguard: only https:// is allowed, got scheme %q", u.Scheme)
	}
	return nil
}

// ValidateEndpoint — строгая форма проверки endpoint-а перед HTTP-запросом:
// [ValidateHTTPSURL] (только https) + непустой host + литеральный IP в host
// проверяется на [IsBlockedIP] здесь (быстрый отказ до сборки клиента).
// DNS-имена проверяются на dial-фазе по резолвнутому IP ([GuardedDialContext]).
func ValidateEndpoint(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("netguard: invalid endpoint url %q", rawURL)
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return fmt.Errorf("netguard: only https:// endpoints are allowed, got scheme %q", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("netguard: endpoint %q has no host", rawURL)
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); ip != nil && IsBlockedIP(ip) {
		return blockedErr(host, ip)
	}
	return nil
}

// NewCheckRedirect возвращает http.Client.CheckRedirect: отвергает любой редирект
// на не-https и цепочку длиннее maxRedirects. Возврат ошибки прерывает запрос
// (downgrade/MITM-защита неотключаема): 302 https→http не должен скачать payload
// по незащищённому каналу и утечь sensitive-заголовки.
//
// SSRF-защита редиректов делается НЕ здесь, а в DialContext клиента
// ([GuardedDialContext]): hop на хост, резолвящийся в metadata/loopback/RFC1918,
// будет отвергнут на dial-фазе по фактически резолвнутому IP — это закрывает и
// прямой доступ, и DNS-rebind, чего CheckRedirect (видит только URL hop-а) дать
// не может.
func NewCheckRedirect(maxRedirects int) func(req *http.Request, via []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if !strings.EqualFold(req.URL.Scheme, "https") {
			return fmt.Errorf("netguard: redirect to non-https blocked: %s://%s", req.URL.Scheme, req.URL.Host)
		}
		if len(via) >= maxRedirects {
			return fmt.Errorf("netguard: stopped after %d redirects", maxRedirects)
		}
		return nil
	}
}

// Resolver — минимум net.Resolver, нужный SSRF-guard. Интерфейс (не конкретный
// *net.Resolver) — чтобы unit-тест guard-а инжектил фейковый резолвер
// (DNS-rebind / multi-IP кейсы) без поднятия настоящего DNS. В проде —
// [DefaultResolver].
type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// DefaultResolver — системный DNS-резолвер для production-клиентов.
var DefaultResolver Resolver = net.DefaultResolver

// DialFunc — сигнатура net.Dialer.DialContext; параметр guard-а, чтобы тест
// проверял, по какому именно адресу (IP) пошёл dial (rebind-safety), без
// реального TCP-соединения.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// GuardedDialContext строит DialContext с resolve-then-check-then-dial: адрес
// резолвится через resolver, ВСЕ полученные IP проверяются на [IsBlockedIP] (если
// хоть один заблокирован — отказ целиком, чтобы хост с парой «один публичный +
// один metadata» A-записей не обходил guard), и dial идёт по первому проверенному
// IP, а не по имени. Dial именно по IP (а не повторный resolve внутри dialer) —
// ключ к защите от DNS-rebind: между проверкой и соединением нет второго резолва,
// который мог бы вернуть другой адрес.
func GuardedDialContext(resolver Resolver, dial DialFunc) DialFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("netguard: ssrf-guard bad address %q: %w", addr, err)
		}

		// Литеральный IP в URL — проверяем без резолва.
		if ip := net.ParseIP(host); ip != nil {
			if IsBlockedIP(ip) {
				return nil, blockedErr(host, ip)
			}
			return dial(ctx, network, addr)
		}

		ips, err := resolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("netguard: ssrf-guard resolve %q: %w", host, err)
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("netguard: ssrf-guard %q resolved to no addresses", host)
		}
		for _, ipa := range ips {
			if IsBlockedIP(ipa.IP) {
				return nil, blockedErr(host, ipa.IP)
			}
		}

		// Dial по уже проверенному конкретному IP (rebind-safe): берём первый,
		// network/port сохраняем. Повторного резолва имени нет.
		return dial(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
	}
}

// blockedErr — единая формулировка отказа SSRF-guard (host для диагностики,
// класс адреса — через сам IP; ничего лишнего наружу).
func blockedErr(host string, ip net.IP) error {
	return fmt.Errorf("netguard: ssrf-guard blocked address for %q: %s (loopback/private/link-local/cgnat/site-local)", host, ip)
}
