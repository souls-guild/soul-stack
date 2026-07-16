package sigil

import (
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	"github.com/souls-guild/soul-stack/shared/obs"
)

// tracer is the OTel tracer for in-process spans of the Sigil subsystem. Gets the global
// TracerProvider raised by [obs.SetupOTel] in main; when OTel is disabled the provider
// is no-op — spans are free and code does not branch (ADR-024 §1.2). Used
// by the daemon around runtime rotation of trust-anchor signing keys via [Tracer].
var tracer = otel.Tracer("keeper/sigil")

// SpanRotation — name of the in-process span for runtime rotation of trust-anchor signing keys
// (re-build Signer + update verify sets + re-broadcast). Extracted as a constant:
// daemon starts the span with this name, test validates it.
const SpanRotation = "sigil.anchors_reload"

// Tracer returns the OTel tracer for the Sigil subsystem for callers in other packages
// (cmd/keeper daemon: reloadAnchors orchestrates multiple subsystems, but
// the span is scoped to Sigil rotation), ensuring it is bound to a single instrumentation
// scope `keeper/sigil`.
func Tracer() trace.Tracer { return tracer }

// KeyMetrics — Prometheus metrics for the Sigil signing key registry (ADR-026(h),
// R3-S7). Registered via helper over the component-agnostic [obs.Registry]
// (pattern [vault.RegisterVaultMetrics] / [grpc.RegisterGRPCMetrics], ADR-024 §4.0).
//
// MVP is a single gauge for the count of active trust-anchor keys: an operational signal
// of set health (how many anchors currently validate signatures). Full accounting for
// "N permissions signed by retiring-key" is expensive without commit-time key metadata in plugin_sigils
// (no way to match precisely) — deferred; gauge + warn-log on Retire (KeyService)
// provide minimal safety visibility (decisions.md R3-S7 item 6).
type KeyMetrics struct {
	// activeKeys — current count of active signing keys (status='active'). No
	// label cardinality: closed set (single-digit keys per cluster). Updated
	// after each registry mutation ([KeyService.afterMutation]).
	activeKeys prometheus.Gauge

	// anchorsRebroadcastTotal — counter for re-broadcast passes of the anchor set
	// to connected Souls (ADR-026(h), R3-S6). Incremented on EVERY
	// `reloadAnchors` (both pub/sub signal and TTL-fallback tick), regardless of
	// how many Souls actually received the set — this signals "node re-read and
	// re-broadcast". No labels: closed operation (single-digit passes).
	anchorsRebroadcastTotal prometheus.Counter

	// anchorsLastDelivered — count of Souls to which the last re-broadcast of the anchor set
	// was successfully delivered ([Outbound.RebroadcastTrustAnchors] delivered).
	// Operational signal "new set has propagated to connected Souls, BEFORE
	// retiring old key" (Retire invariant, ADR-026(h), R3-S7). Gauge
	// (snapshot of last broadcast state), no labels.
	anchorsLastDelivered prometheus.Gauge
}

// RegisterKeyMetrics creates keeper_sigil_* collectors and registers them in
// [obs.Registry]. MustRegister: duplicate registration is a programmer error.
func RegisterKeyMetrics(reg *obs.Registry) *KeyMetrics {
	m := &KeyMetrics{
		activeKeys: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "keeper_sigil_signing_keys_active",
			Help: "Current count of active Sigil signing trust-anchor keys (status='active').",
		}),
		anchorsRebroadcastTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "keeper_sigil_anchors_rebroadcast_total",
			Help: "Re-broadcast passes of signing trust-anchor key set to connected Souls (for each reloadAnchors: pub/sub signal + TTL-fallback tick).",
		}),
		anchorsLastDelivered: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "keeper_sigil_anchors_last_delivered",
			Help: "Count of Souls to which the last re-broadcast of signing trust-anchor key set was successfully delivered.",
		}),
	}
	reg.Registerer().MustRegister(m.activeKeys, m.anchorsRebroadcastTotal, m.anchorsLastDelivered)
	return m
}

// SetActive sets the gauge for count of active keys. nil receiver — no-op
// (KeyService can work without observability in unit tests).
func (m *KeyMetrics) SetActive(n int) {
	if m == nil {
		return
	}
	m.activeKeys.Set(float64(n))
}

// ObserveAnchorsRebroadcast records one pass of the anchor set re-broadcast:
// increment the pass counter + set the gauge for last delivered count.
// Called by daemon from `reloadAnchors` (both pub/sub path and TTL-fallback tick).
// nil receiver — no-op (daemon may call before registry wire-up / in tests).
func (m *KeyMetrics) ObserveAnchorsRebroadcast(delivered int) {
	if m == nil {
		return
	}
	m.anchorsRebroadcastTotal.Inc()
	m.anchorsLastDelivered.Set(float64(delivered))
}
