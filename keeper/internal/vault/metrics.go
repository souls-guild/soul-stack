package vault

import (
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/souls-guild/soul-stack/shared/obs"
)

// VaultMetrics is a set of Prometheus collectors for the keeper-side Vault
// client (KV v2 reads, ADR-017). Registered by a separate helper on top of
// the component-agnostic [obs.Registry] — the same pattern as
// [grpc.RegisterGRPCMetrics] / [scenario.RegisterScenarioMetrics] (ADR-024
// §4.0): registry-core doesn't know about specific metrics, and
// keeper_vault_*-metrics are a keeper-side Vault wrapper detail.
//
// The metrics live here (keeper/internal/vault) rather than in shared/obs
// because they're tied to Keeper's server-side Vault operations (ADR-011:
// shared/vault is client-only; server-side is not exported through shared/).
//
// SECURITY (ADR-024 §2.2 + "security first"): labels MUST NOT carry the
// secret value or the logical KV path (the path often carries the secret's
// name and has high cardinality). The only cut is `mount` (closed enum,
// 1-2 values per keeper: `secret`-default) and error `kind` (closed enum
// notfound/error). Names follow Prometheus convention (snake_case, _total
// for counters, _seconds for histogram latency; ADR-024 §2.1).
type VaultMetrics struct {
	// readDuration is the latency of a single [Client.ReadKV] in seconds
	// (round-trip to Vault), cut by mount. This is the hot path of secret
	// resolution: CEL vault(), vault:-ref, core.vault.kv-read, JWT-signing-key
	// reads.
	readDuration *prometheus.HistogramVec

	// readErrorsTotal is a counter of failed [Client.ReadKV] calls, cut by
	// mount and error kind (`notfound` — ErrVaultKVNotFound, path missing/
	// deleted; `error` — transport/other read error). The detail of the
	// cause (the path itself) goes in the caller's log/trace, not the metric.
	readErrorsTotal *prometheus.CounterVec

	// writeDuration is the latency of a single [Client.WriteKV] in seconds,
	// cut by mount. Writes are a rare path (Sigil signing-key entry, R3-S7),
	// but are measured with the same cut as reads for alerting consistency.
	writeDuration *prometheus.HistogramVec

	// writeErrorsTotal is a counter of failed [Client.WriteKV] calls by mount.
	// Writes have no notfound outcome, so there's no kind cut (one class — `error`).
	writeErrorsTotal *prometheus.CounterVec

	// listDuration is the latency of a single [Client.ListKV] in seconds, cut
	// by mount. LIST is a rare path (the Reaper rule reap_orphan_vault_keys'
	// orphan-reconcile), but is measured with the same cut for consistency.
	listDuration *prometheus.HistogramVec

	// listErrorsTotal is a counter of failed [Client.ListKV] calls by mount
	// and kind. `notfound` is split from `error` symmetrically with reads:
	// for LIST, a missing subfolder is NOT an error (Client returns a nil
	// result without err), so notfound rarely occurs here, but the cut is
	// kept consistent.
	listErrorsTotal *prometheus.CounterVec
}

// Read error kinds for keeper_vault_read_errors_total. Closed enum with 2
// values: `notfound` is split from the rest because it's a normal outcome
// (no key), not a transport failure — they need different alerting.
const (
	readErrorNotFound = "notfound"
	readErrorOther    = "error"
)

// RegisterVaultMetrics creates the keeper_vault_*-collectors and registers
// them in [obs.Registry]. Returns a handle for wire-up via [Client.SetMetrics].
//
// MustRegister: a duplicate registration is a programmer error (registered
// twice on the same Registry); failing fast is more convenient than carrying
// lazy initialization (identical pattern to [grpc.RegisterGRPCMetrics]).
func RegisterVaultMetrics(reg *obs.Registry) *VaultMetrics {
	m := &VaultMetrics{
		readDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "keeper_vault_read_duration_seconds",
				Help:    "Латентность чтения Vault KV (ReadKV) в секундах, разрезанная по mount.",
				Buckets: prometheus.DefBuckets,
			},
			[]string{"mount"},
		),
		readErrorsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "keeper_vault_read_errors_total",
				Help: "Количество неуспешных чтений Vault KV, разрезанное по mount и kind (notfound/error).",
			},
			[]string{"mount", "kind"},
		),
		writeDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "keeper_vault_write_duration_seconds",
				Help:    "Латентность записи Vault KV (WriteKV) в секундах, разрезанная по mount.",
				Buckets: prometheus.DefBuckets,
			},
			[]string{"mount"},
		),
		writeErrorsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "keeper_vault_write_errors_total",
				Help: "Количество неуспешных записей Vault KV, разрезанное по mount.",
			},
			[]string{"mount"},
		),
		listDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "keeper_vault_list_duration_seconds",
				Help:    "Латентность перечисления Vault KV (ListKV) в секундах, разрезанная по mount.",
				Buckets: prometheus.DefBuckets,
			},
			[]string{"mount"},
		),
		listErrorsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "keeper_vault_list_errors_total",
				Help: "Количество неуспешных перечислений Vault KV, разрезанное по mount и kind (notfound/error).",
			},
			[]string{"mount", "kind"},
		),
	}
	reg.Registerer().MustRegister(
		m.readDuration, m.readErrorsTotal,
		m.writeDuration, m.writeErrorsTotal,
		m.listDuration, m.listErrorsTotal,
	)
	return m
}

// ObserveRead records the completion of a single [Client.ReadKV]: observes
// latency by mount and, when err != nil, increments the error counter with
// notfound/error split. A nil receiver is a no-op: Client can come up
// without observability (keeper init bootstrap path without a registry,
// unit tests).
func (m *VaultMetrics) ObserveRead(mount string, dur time.Duration, err error) {
	if m == nil {
		return
	}
	m.readDuration.WithLabelValues(mount).Observe(dur.Seconds())
	if err != nil {
		kind := readErrorOther
		if errors.Is(err, ErrVaultKVNotFound) {
			kind = readErrorNotFound
		}
		m.readErrorsTotal.WithLabelValues(mount, kind).Inc()
	}
}

// ObserveWrite records the completion of a single [Client.WriteKV]: observes
// latency by mount and, when err != nil, increments the error counter. A
// nil receiver is a no-op (Client can come up without observability).
func (m *VaultMetrics) ObserveWrite(mount string, dur time.Duration, err error) {
	if m == nil {
		return
	}
	m.writeDuration.WithLabelValues(mount).Observe(dur.Seconds())
	if err != nil {
		m.writeErrorsTotal.WithLabelValues(mount).Inc()
	}
}

// ObserveList records the completion of a single [Client.ListKV]: observes
// latency by mount and, when err != nil, increments the error counter with
// notfound/error split (the same mapping as ObserveRead). A nil receiver is
// a no-op.
func (m *VaultMetrics) ObserveList(mount string, dur time.Duration, err error) {
	if m == nil {
		return
	}
	m.listDuration.WithLabelValues(mount).Observe(dur.Seconds())
	if err != nil {
		kind := readErrorOther
		if errors.Is(err, ErrVaultKVNotFound) {
			kind = readErrorNotFound
		}
		m.listErrorsTotal.WithLabelValues(mount, kind).Inc()
	}
}
