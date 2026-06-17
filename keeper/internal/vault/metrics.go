package vault

import (
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/souls-guild/soul-stack/shared/obs"
)

// VaultMetrics — набор Prometheus-collector-ов keeper-side Vault-клиента
// (чтение KV v2, ADR-017). Регистрируется отдельным helper-ом поверх
// компонент-агностичного [obs.Registry] — тем же паттерном, что
// [grpc.RegisterGRPCMetrics] / [scenario.RegisterScenarioMetrics] (ADR-024
// §4.0): registry-core не знает про конкретные метрики, а keeper_vault_*-
// метрики — частность keeper-side Vault-обёртки.
//
// Метрики живут здесь (keeper/internal/vault), а не в shared/obs, потому что
// привязаны к server-side Vault-операциям Keeper-а (ADR-011: shared/vault — это
// только клиентская часть, server-side через shared/ не экспортируется).
//
// БЕЗОПАСНОСТЬ (ADR-024 §2.2 + «безопасность на первом месте»): в label-ы НЕ
// кладём ни значение секрета, ни логический KV-путь (путь часто несёт имя
// секрета и высокую кардинальность). Разрез — только `mount` (closed enum,
// 1-2 значения на keeper: `secret`-default) и `kind` ошибки (closed enum
// notfound/error). Имена — Prometheus convention (snake_case, _total для
// counter, _seconds для histogram латентности; ADR-024 §2.1).
type VaultMetrics struct {
	// readDuration — латентность одного [Client.ReadKV] в секундах (round-trip
	// до Vault), разрезанная по mount. Это горячий путь резолва секретов:
	// CEL vault(), vault:-ref, core.vault.kv-read, чтение JWT-signing-key.
	readDuration *prometheus.HistogramVec

	// readErrorsTotal — счётчик неуспешных [Client.ReadKV], разрезанный по mount
	// и kind ошибки (`notfound` — ErrVaultKVNotFound, путь отсутствует/удалён;
	// `error` — транспортная/прочая ошибка чтения). Деталь причины (сам путь) —
	// в log/trace caller-а, не в метрику.
	readErrorsTotal *prometheus.CounterVec

	// writeDuration — латентность одного [Client.WriteKV] в секундах, разрезанная
	// по mount. Запись — редкий путь (ввод ключа подписи Sigil, R3-S7), но
	// измеряется тем же разрезом, что чтение, для единообразия алертинга.
	writeDuration *prometheus.HistogramVec

	// writeErrorsTotal — счётчик неуспешных [Client.WriteKV] по mount. Запись не
	// имеет notfound-исхода, поэтому без kind-разреза (один класс — `error`).
	writeErrorsTotal *prometheus.CounterVec

	// listDuration — латентность одного [Client.ListKV] в секундах, разрезанная
	// по mount. LIST — редкий путь (orphan-reconcile Reaper-правила
	// reap_orphan_vault_keys), но измеряется тем же разрезом для единообразия.
	listDuration *prometheus.HistogramVec

	// listErrorsTotal — счётчик неуспешных [Client.ListKV] по mount и kind.
	// `notfound` отделён от `error` симметрично чтению: для LIST отсутствующая
	// подпапка — НЕ ошибка (Client отдаёт nil-результат без err), поэтому
	// notfound тут практически не встречается, но разрез держим единообразным.
	listErrorsTotal *prometheus.CounterVec
}

// Виды ошибок чтения для keeper_vault_read_errors_total. Closed enum в 2
// значения: `notfound` отделён от прочих, т.к. это штатный исход (нет ключа),
// а не сбой транспорта — алертить на них надо по-разному.
const (
	readErrorNotFound = "notfound"
	readErrorOther    = "error"
)

// RegisterVaultMetrics создаёт keeper_vault_*-collectors и регистрирует их в
// [obs.Registry]. Возвращает дескриптор для wire-up через [Client.SetMetrics].
//
// MustRegister: дубликат-регистрация — programmer error (вызвали дважды на
// одном Registry); падать сразу удобнее, чем носить ленивую инициализацию
// (паттерн идентичен [grpc.RegisterGRPCMetrics]).
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

// ObserveRead фиксирует завершение одного [Client.ReadKV]: наблюдает латентность
// по mount и, при err != nil, инкрементирует счётчик ошибок с разделением
// notfound/error. nil-получатель — no-op: Client может подниматься без
// observability (bootstrap-путь keeper init без registry, unit-тесты).
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

// ObserveWrite фиксирует завершение одного [Client.WriteKV]: наблюдает латентность
// по mount и, при err != nil, инкрементирует счётчик ошибок. nil-получатель —
// no-op (Client может подниматься без observability).
func (m *VaultMetrics) ObserveWrite(mount string, dur time.Duration, err error) {
	if m == nil {
		return
	}
	m.writeDuration.WithLabelValues(mount).Observe(dur.Seconds())
	if err != nil {
		m.writeErrorsTotal.WithLabelValues(mount).Inc()
	}
}

// ObserveList фиксирует завершение одного [Client.ListKV]: наблюдает латентность
// по mount и, при err != nil, инкрементирует счётчик ошибок с разделением
// notfound/error (тем же маппингом, что ObserveRead). nil-получатель — no-op.
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
