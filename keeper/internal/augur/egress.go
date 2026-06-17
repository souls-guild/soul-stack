package augur

// SSRF egress-guard для брокера prom/elk (augur.md §4.1 / §7 / §9 — endpoint из
// БД-записи Omen НЕдоверенный ввод, Keeper делает по нему исходящий HTTP).
//
// Общая SSRF-guard-логика (resolve-then-check-then-dial по фактическому IP,
// rebind-safe; CheckRedirect-downgrade-защита; https-only; классификатор
// заблокированных IP) вынесена в shared/netguard и переиспользуется core-HTTP-
// модулями Soul-а (core.url / core.http). Здесь остаётся augur-специфика:
// таймауты брокерного запроса, body-limit и конструктор клиента поверх netguard.

import (
	"net"
	"net/http"
	"time"

	"github.com/souls-guild/soul-stack/shared/netguard"
)

// maxEgressRedirects — жёсткий лимит редиректов для брокерного HTTP-фетча. Каждый
// hop проверяется на https и по фактическому IP (см. netguard); лимит защищает
// от бесконечной цепочки.
const maxEgressRedirects = 10

// egressDialTimeout — таймаут установления TCP-соединения. Совпадает с
// http.DefaultTransport.
const egressDialTimeout = 30 * time.Second

// egressRequestTimeout — общий таймаут одного брокерного HTTP-запроса (dial +
// TLS + чтение лимитированного тела). endpoint НЕдоверен — медленный/висящий
// внешний хост не должен держать горутину обработки бесконечно.
const egressRequestTimeout = 15 * time.Second

// maxResponseBytes — лимит размера тела ответа внешней системы (10 MiB). Защита
// от raw-DoS: НЕдоверенный endpoint не должен заставить Keeper аллоцировать
// произвольный объём. Тело читается через io.LimitReader на эту границу;
// превышение → ошибка (см. broker_http.go).
const maxResponseBytes = 10 << 20

// validateEndpoint — проверка endpoint-а Omen-а перед HTTP-запросом (https-only +
// непустой host + литеральный IP в block-list, быстрый отказ до сборки клиента).
// Делегирует в shared/netguard; обёртка сохраняет augur-имя для брокеров
// (broker_prom/broker_elk).
func validateEndpoint(rawURL string) error {
	return netguard.ValidateEndpoint(rawURL)
}

// newEgressClient возвращает *http.Client для брокерного HTTP к НЕдоверенному
// endpoint-у: системный TLS trust store (никакого InsecureSkipVerify), общий
// таймаут запроса, redirect-downgrade-защита + лимит и SSRF-guard на dial-фазе
// по фактическому IP (rebind-safe, deny metadata/loopback/private) — всё из
// shared/netguard.
//
// resolver инжектируется для тестируемости guard-а; в проде передаётся
// netguard.DefaultResolver (см. NewEgressClient).
func newEgressClient(resolver netguard.Resolver) *http.Client {
	dialer := &net.Dialer{Timeout: egressDialTimeout}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = netguard.GuardedDialContext(resolver, dialer.DialContext)
	return &http.Client{
		Timeout:       egressRequestTimeout,
		CheckRedirect: netguard.NewCheckRedirect(maxEgressRedirects),
		Transport:     transport,
	}
}

// NewEgressClient — production SSRF-guarded *http.Client для брокерного HTTP к
// НЕдоверенному endpoint-у Omen-а (prom/elk). Системный DNS-резолвер; реализует
// [HTTPDoer]. grpc-handler создаёт один экземпляр и передаёт в брокеры.
func NewEgressClient() *http.Client {
	return newEgressClient(netguard.DefaultResolver)
}
