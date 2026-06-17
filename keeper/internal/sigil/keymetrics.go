package sigil

import (
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	"github.com/souls-guild/soul-stack/shared/obs"
)

// tracer для in-process span-ов Sigil-подсистемы. Берёт глобальный
// TracerProvider, поднятый [obs.SetupOTel] в main; при OTel disabled провайдер
// no-op — span-ы бесплатны и код не ветвится (ADR-024 §1.2). Используется
// daemon-ом вокруг runtime-ротации trust-anchor-ключей подписи через [Tracer].
var tracer = otel.Tracer("keeper/sigil")

// SpanRotation — имя in-process span-а runtime-ротации trust-anchor-ключей
// подписи (re-build Signer + обновление verify-наборов + re-broadcast). Вынесено
// константой: daemon стартует span этим именем, тест сверяет его.
const SpanRotation = "sigil.anchors_reload"

// Tracer отдаёт пакетный OTel-tracer Sigil-подсистемы для caller-а в другом
// пакете (cmd/keeper daemon: reloadAnchors оркеструет несколько подсистем, но
// span — про Sigil-ротацию), чтобы он был привязан к одному instrumentation
// scope `keeper/sigil`.
func Tracer() trace.Tracer { return tracer }

// KeyMetrics — Prometheus-метрики реестра ключей подписи Sigil (ADR-026(h),
// R3-S7). Регистрируется helper-ом поверх компонент-агностичного [obs.Registry]
// (паттерн [vault.RegisterVaultMetrics] / [grpc.RegisterGRPCMetrics], ADR-024 §4.0).
//
// MVP — один gauge числа active trust-anchor-ключей: операционный сигнал состояния
// набора (сколько якорей сейчас валидируют подписи). Полный учёт «N допусков
// подписаны retiring-ключом» дорог без commit-time-метки ключа в plugin_sigils
// (нечем точно сопоставить) — отложен; gauge + warn-лог при Retire (KeyService)
// дают минимальную safety-видимость (decisions.md R3-S7 item 6).
type KeyMetrics struct {
	// activeKeys — текущее число active-ключей подписи (status='active'). Без
	// разреза по label-ам: closed-набор (единицы ключей на кластер). Обновляется
	// после каждой мутации реестра ([KeyService.afterMutation]).
	activeKeys prometheus.Gauge

	// anchorsRebroadcastTotal — счётчик проходов re-broadcast-а набора якорей
	// подключённым Soul-ам (ADR-026(h), R3-S6). Инкремент на КАЖДЫЙ
	// `reloadAnchors` (и pub/sub-сигнал, и TTL-fallback-тик), независимо от того,
	// скольким Soul-ам набор реально доехал — это сигнал «нода перечитала и
	// разослала». Без label-ов: closed-операция (единицы проходов).
	anchorsRebroadcastTotal prometheus.Counter

	// anchorsLastDelivered — число Soul-ов, которым последний re-broadcast набора
	// якорей ушёл успешно ([Outbound.RebroadcastTrustAnchors] delivered).
	// Операционный сигнал «новый набор разошёлся подключённым Soul-ам, ПЕРЕД
	// Retire старого ключа» (Retire-инвариант, ADR-026(h), R3-S7). Gauge
	// (мгновенное состояние последней раздачи), без label-ов.
	anchorsLastDelivered prometheus.Gauge
}

// RegisterKeyMetrics создаёт keeper_sigil_*-collectors и регистрирует их в
// [obs.Registry]. MustRegister: дубликат-регистрация — programmer error.
func RegisterKeyMetrics(reg *obs.Registry) *KeyMetrics {
	m := &KeyMetrics{
		activeKeys: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "keeper_sigil_signing_keys_active",
			Help: "Текущее число active trust-anchor-ключей подписи Sigil (status='active').",
		}),
		anchorsRebroadcastTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "keeper_sigil_anchors_rebroadcast_total",
			Help: "Проходы re-broadcast-а набора trust-anchor-ключей подписи подключённым Soul-ам (на каждый reloadAnchors: pub/sub-сигнал + TTL-fallback-тик).",
		}),
		anchorsLastDelivered: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "keeper_sigil_anchors_last_delivered",
			Help: "Число Soul-ов, которым последний re-broadcast набора trust-anchor-ключей подписи ушёл успешно (delivered).",
		}),
	}
	reg.Registerer().MustRegister(m.activeKeys, m.anchorsRebroadcastTotal, m.anchorsLastDelivered)
	return m
}

// SetActive проставляет gauge числа active-ключей. nil-получатель — no-op
// (KeyService может работать без observability в unit-тестах).
func (m *KeyMetrics) SetActive(n int) {
	if m == nil {
		return
	}
	m.activeKeys.Set(float64(n))
}

// ObserveAnchorsRebroadcast фиксирует один проход re-broadcast-а набора якорей:
// инкремент счётчика проходов + проставление gauge последнего delivered.
// Вызывается daemon-ом из `reloadAnchors` (и pub/sub-путь, и TTL-fallback-тик).
// nil-получатель — no-op (daemon может вызывать до wire-up registry / в тестах).
func (m *KeyMetrics) ObserveAnchorsRebroadcast(delivered int) {
	if m == nil {
		return
	}
	m.anchorsRebroadcastTotal.Inc()
	m.anchorsLastDelivered.Set(float64(delivered))
}
