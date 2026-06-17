package util

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/souls-guild/soul-stack/shared/netguard"
)

// SSRF egress-guard core-HTTP-модулей (core.url / core.http). Общая guard-логика
// (resolve-then-check-then-dial по фактическому IP, rebind-safe; redirect-
// downgrade-защита; https-only; классификатор заблокированных IP) вынесена в
// shared/netguard и переиспользуется Augur-брокерами Keeper-а. Здесь — тонкие
// обёртки, сохраняющие core-side API (MaxRedirects, CheckRedirect, ValidateURL,
// IsBlockedIP, NewHTTPClient, HTTPDoer) для core.url / core.http.

// MaxRedirects — жёсткий лимит редиректов для HTTP-фетча core-модулей. Каждый
// hop проверяется на https (см. CheckRedirect); лимит защищает от бесконечной
// цепочки.
//
// Единая точка для core-модулей, ходящих по HTTP (core.url, core.http);
// не дублировать локальными копиями.
const MaxRedirects = 10

// dialTimeout — таймаут установления TCP-соединения для core-HTTP-клиента.
// Совпадает с http.DefaultTransport, чтобы кастомный DialContext не менял
// поведение по таймаутам (защита от SSRF, а не источник новых stall-ов).
const dialTimeout = 30 * time.Second

// HTTPDoer — минимальный интерфейс HTTP-клиента, нужный core-модулям. Вынесен в
// поле модуля для тестабельности: unit-тесты подменяют на fake без выхода в
// сеть. В проде — *http.Client с дефолтным (системным) TLS trust store
// (см. NewHTTPClient).
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// CheckRedirect — http.Client.CheckRedirect: отвергает любой редирект на
// не-https и цепочку длиннее MaxRedirects. Возврат ошибки прерывает запрос
// (downgrade/MITM-защита неотключаема): 302 https→http не должен скачать
// payload по незащищённому каналу и утечь sensitive-заголовки.
//
// SSRF-защита редиректов делается НЕ здесь, а в DialContext клиента (см.
// NewHTTPClient): hop на хост, резолвящийся в metadata/loopback/RFC1918, будет
// отвергнут на dial-фазе по фактически резолвнутому IP. Делегирует в
// shared/netguard (единая supply-chain-защита для core.url / core.http и
// Augur-брокеров Keeper-а).
func CheckRedirect(req *http.Request, via []*http.Request) error {
	return netguard.NewCheckRedirect(MaxRedirects)(req, via)
}

// IsBlockedIP классифицирует IP как недопустимый для исходящего core-HTTP
// (loopback/RFC1918/ULA/link-local/CGNAT/site-local/unspecified). Делегирует в
// shared/netguard. Экспортирована для unit-тестирования классификатора в
// изоляции; используется SSRF-guard через DialContext.
func IsBlockedIP(ip net.IP) bool {
	return netguard.IsBlockedIP(ip)
}

// ValidateURL принимает только https://. http:// — downgrade-риск, file:// —
// чтение локальной ФС в обход модели доступа; всё, кроме https, отвергается.
// Тонкая обёртка над ValidateFetchURL(rawURL, false) — поведение идентично
// прежнему делегированию в netguard.ValidateHTTPSURL.
//
// Единая точка для core-модулей, ходящих по HTTP; не дублировать локальными
// копиями.
func ValidateURL(rawURL string) error {
	return ValidateFetchURL(rawURL, false)
}

// ValidateFetchURL проверяет URL фетча с учётом opt-in на http://.
//
// allowHTTP=false — максимально безопасный режим (default по всем потребителям):
// делегирует БУКВАЛЬНО в netguard.ValidateHTTPSURL (тот же код, что у Augur —
// единая supply-chain-проверка), пропускается только https://.
//
// allowHTTP=true — явный opt-out оператора (param allow_http): допускаются http
// и https, всё прочее (file://, ftp:// и т.п.) отвергается. Схема сверяется
// строго через url.Parse + strings.EqualFold (а не наивный HasPrefix: строка
// "https://\nhttp://evil" не должна пролезть).
//
// Снятие https-only НЕ открывает SSRF: dial-guard живёт отдельно в NewHTTPClient
// (HTTPClientOpts.AllowPrivate) — http по-прежнему не дойдёт до metadata/loopback.
func ValidateFetchURL(rawURL string, allowHTTP bool) error {
	if !allowHTTP {
		return netguard.ValidateHTTPSURL(rawURL)
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("util: invalid url %q", rawURL)
	}
	if !strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https") {
		return fmt.Errorf("util: only http(s):// is allowed, got scheme %q", u.Scheme)
	}
	return nil
}

// checkRedirectAllowingHTTP — http.Client.CheckRedirect для режима allow_http:
// допускает hop как на https, так и на http (downgrade-redirect ожидаем при
// явном opt-out оператора), но отвергает любую не-http(s) схему и цепочку длиннее
// maxRedirects. Схема сверяется регистронезависимо (EqualFold).
//
// SSRF-защита редиректов делается НЕ здесь, а в DialContext клиента
// (netguard.GuardedDialContext при AllowPrivate=false): hop на хост,
// резолвящийся в metadata/loopback/RFC1918, отвергается на dial-фазе по
// фактически резолвнутому IP. allow_http ослабляет ТОЛЬКО downgrade-проверку
// схемы, не SSRF-guard.
func checkRedirectAllowingHTTP(maxRedirects int) func(req *http.Request, via []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if !strings.EqualFold(req.URL.Scheme, "http") && !strings.EqualFold(req.URL.Scheme, "https") {
			return fmt.Errorf("util: redirect to non-http(s) blocked: %s://%s", req.URL.Scheme, req.URL.Host)
		}
		if len(via) >= maxRedirects {
			return fmt.Errorf("util: stopped after %d redirects", maxRedirects)
		}
		return nil
	}
}

// HTTPClientOpts — opt-out-флаги для NewHTTPClient. Нулевое значение =
// максимально безопасный клиент (прежний NewHTTPClient(false)): SSRF-guard на
// dial-фазе, системный TLS trust store, downgrade-защита редиректов. Каждый флаг
// ослабляет отдельный контур и взводится только по явному запросу оператора.
type HTTPClientOpts struct {
	// AllowPrivate — true: SSRF dial-guard выключен (легитимный internal
	// endpoint, напр. health-check 127.0.0.1:8080/health). false (default):
	// netguard.GuardedDialContext блокирует dial в metadata/loopback/RFC1918.
	AllowPrivate bool
	// InsecureSkipVerify — true: transport.TLSClientConfig.InsecureSkipVerify
	// (self-signed / internal CA). MITM-риск, взводится только явным opt-out.
	InsecureSkipVerify bool
	// AllowHTTPRedirect — true: CheckRedirect допускает downgrade-hop https→http
	// (парный с allow_http на уровне модуля). false (default): downgrade на
	// не-https отвергается (netguard).
	AllowHTTPRedirect bool
}

// GuardWarnings строит список warning-строк об ослабленных security-контурах
// HTTP-фетча (core.url / core.http). Единый источник правды формулировок и
// host-only-маскинга для обоих модулей: при поднятом opt-out-флаге оператор
// видит факт снятия контура в output ApplyEvent (конвенция core.repo/core.url —
// warnings в output доходят до оператора в RunResult).
//
// Возвращает по одной строке на каждый взведённый флаг, детерминированным
// порядком (insecure_skip_verify → allow_http → allow_private). Нулевые флаги →
// nil. В строку попадает ТОЛЬКО host (параметр host — заранее извлечённый
// WarnHost(rawURL)): полный URL может нести query/path с sensitive-данными,
// headers — sensitive-by-construction ([ADR-010] §7.4), ни то ни другое в
// warning не светится.
//
// [ADR-010]: docs/adr/0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов
func GuardWarnings(host string, opts HTTPClientOpts) []string {
	if !opts.InsecureSkipVerify && !opts.AllowHTTPRedirect && !opts.AllowPrivate {
		return nil
	}
	var w []string
	if opts.InsecureSkipVerify {
		w = append(w, fmt.Sprintf("TLS verification disabled (insecure_skip_verify) for %s", host))
	}
	if opts.AllowHTTPRedirect {
		w = append(w, fmt.Sprintf("plaintext http allowed (allow_http) for %s", host))
	}
	if opts.AllowPrivate {
		w = append(w, fmt.Sprintf("SSRF-guard disabled (allow_private) for %s", host))
	}
	return w
}

// WarnHost извлекает host из URL для guard-warning-а (без схемы/path/query — они
// могут нести sensitive-данные). Вызывается уже после ValidateFetchURL, поэтому
// невалидный URL сюда не доходит штатно; на страховку возвращает "?", не роняя
// Apply. Единая точка для core.url / core.http.
func WarnHost(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return "?"
	}
	return u.Host
}

// NewHTTPClient возвращает *http.Client для core-модулей, сконфигурированный
// набором opt-out-флагов HTTPClientOpts. Нулевое значение opts — дефолтный
// максимально-безопасный клиент: системный TLS trust store, downgrade-защита
// редиректов (CheckRedirect + лимит) и SSRF-guard на dial-фазе (shared/netguard).
//
// SSRF-guard (AllowPrivate=false): кастомный DialContext резолвит хост через
// net.Resolver и отвергает соединение, если ХОТЯ БЫ ОДИН резолвнутый IP попадает
// под IsBlockedIP. Проверка ПОСЛЕ резолва и ДО dial закрывает сразу два вектора:
//   - прямой SSRF (https://169.254.169.254 — cloud metadata IAM-креды,
//     https://127.0.0.1, RFC1918, [::1], link-local);
//   - DNS-rebind (хост, чей DNS резолвится в metadata/loopback, не дойдёт —
//     резолв реальный, dial идёт по уже проверенному конкретному IP, не по
//     имени, поэтому повторный «rebind»-резолв исключён).
//
// AllowPrivate=true — opt-in для легитимного internal health-check: guard
// выключается, dial идёт штатно. AllowHTTPRedirect=true — допускает
// downgrade-hop (парно с allow_http). InsecureSkipVerify=true — отключает
// проверку TLS-цепочки (self-signed). Каждый флаг ослабляет независимый контур.
//
// Единая точка для core-модулей, ходящих по HTTP; не дублировать локальными
// копиями.
func NewHTTPClient(opts HTTPClientOpts) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if !opts.AllowPrivate {
		dialer := &net.Dialer{Timeout: dialTimeout}
		transport.DialContext = netguard.GuardedDialContext(netguard.DefaultResolver, dialer.DialContext)
	}
	if opts.InsecureSkipVerify {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	checkRedirect := CheckRedirect
	if opts.AllowHTTPRedirect {
		checkRedirect = checkRedirectAllowingHTTP(MaxRedirects)
	}
	return &http.Client{
		CheckRedirect: checkRedirect,
		Transport:     transport,
	}
}
