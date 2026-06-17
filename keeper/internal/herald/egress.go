package herald

// SSRF egress-guard webhook-доставки Herald (ADR-052(e): URL канала —
// оператор-заданный → исходящий HTTP с keeper-а → SSRF-вектор).
//
// Общая SSRF-guard-логика (resolve-then-check-then-dial по фактическому IP,
// rebind-safe; CheckRedirect-downgrade-защита; https-only; классификатор
// заблокированных IP) — в shared/netguard (тот же guard, что у augur/core.url).
// Здесь — herald-специфика: per-Herald opt-out (http_allowed/allow_private),
// конфигурируемый timeout, конструктор клиента под конкретный канал.

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/souls-guild/soul-stack/shared/netguard"
)

const (
	// maxDeliveryRedirects — жёсткий лимит редиректов webhook-POST-а. Каждый hop
	// проверяется на https и по фактическому IP (netguard).
	maxDeliveryRedirects = 5

	// deliveryDialTimeout — таймаут установления TCP-соединения.
	deliveryDialTimeout = 10 * time.Second

	// DefaultDeliveryTimeout — дефолтный общий таймаут одного webhook-POST-а
	// (dial + TLS + запись + чтение ответа). Оператор-заданный endpoint
	// НЕдоверен: медленный/злой хост не должен держать worker-горутину дольше.
	// Конфигурируемый (keeper.yml::herald.delivery_timeout, ADR-052(e) «timeout»).
	DefaultDeliveryTimeout = 10 * time.Second
)

// validateDeliveryEndpoint — проверка URL канала ПЕРЕД запросом (ADR-052(e)):
// config мог измениться после create (или create прошёл по другому opt-out-у),
// поэтому валидируем на каждой доставке, а не доверяем CRUD-времени.
//
// allowPrivate / httpAllowed — per-Herald opt-out-ы (config.allow_private /
// config.http_allowed). Дефолтный контур (оба false): https-only + литеральный
// private-IP в host блокируется (netguard.ValidateEndpoint). DNS-резолв в
// приватный IP ловится на dial-фазе (guardedDeliveryClient).
//
// При allow_private dial-guard не ставится вовсе (см. guardedDeliveryClient),
// поэтому здесь литеральный private-IP при allow_private не отвергаем.
func validateDeliveryEndpoint(rawURL string, httpAllowed, allowPrivate bool) error {
	if !httpAllowed && !allowPrivate {
		return netguard.ValidateEndpoint(rawURL)
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("herald: invalid webhook url %q", rawURL)
	}
	if u.Host == "" {
		return fmt.Errorf("herald: webhook url %q has no host", rawURL)
	}
	if !httpAllowed && !strings.EqualFold(u.Scheme, "https") {
		return fmt.Errorf("herald: only https:// webhook allowed (set http_allowed), got scheme %q", u.Scheme)
	}
	if u.Scheme != "" && !strings.EqualFold(u.Scheme, "https") && !strings.EqualFold(u.Scheme, "http") {
		return fmt.Errorf("herald: unsupported webhook url scheme %q", u.Scheme)
	}
	// allow_private=false + http_allowed=true: приватку всё равно режем (dial-guard
	// в guardedDeliveryClient). Литеральный private-IP здесь не валидируем —
	// dial-фаза покроет (и литерал, и DNS-резолв).
	return nil
}

// guardedDeliveryClient собирает *http.Client под конкретный канал:
//   - системный TLS trust store (никакого InsecureSkipVerify);
//   - общий timeout запроса;
//   - redirect-downgrade-защита + лимит (netguard.NewCheckRedirect);
//   - SSRF dial-guard по фактическому IP (netguard.GuardedDialContext), ЕСЛИ
//     allowPrivate=false. При allowPrivate=true (явный opt-out оператора)
//     dial-guard НЕ ставится — приватные IP разрешены (на свой риск, ADR-052(e)
//     «allow_private — явный opt-out как core.url»).
//
// resolver инжектируется для тестируемости guard-а (DNS-rebind/multi-IP без
// настоящего DNS); в проде — netguard.DefaultResolver.
func guardedDeliveryClient(resolver netguard.Resolver, allowPrivate bool, timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = DefaultDeliveryTimeout
	}
	dialer := &net.Dialer{Timeout: deliveryDialTimeout}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if !allowPrivate {
		transport.DialContext = netguard.GuardedDialContext(resolver, dialer.DialContext)
	} else {
		transport.DialContext = dialer.DialContext
	}
	return &http.Client{
		Timeout:       timeout,
		CheckRedirect: netguard.NewCheckRedirect(maxDeliveryRedirects),
		Transport:     transport,
	}
}
