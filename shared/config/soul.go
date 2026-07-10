package config

import (
	"net"
	"strconv"
)

// SoulConfig — типизированное представление `soul.yml`.
// Нормативная спека блоков — [docs/soul/config.md].
//
// У Soul нет блока `auth:` (mTLS / SoulSeed вместо JWT, см. ADR-014).
type SoulConfig struct {
	SID string `yaml:"sid,omitempty"`

	Paths       SoulPaths        `yaml:"paths,omitempty"`
	Keeper      SoulKeeper       `yaml:"keeper"`
	Soulprint   *SoulSoulprint   `yaml:"soulprint,omitempty"`
	Utilization *SoulUtilization `yaml:"utilization,omitempty"`
	Cleanup     *SoulCleanup     `yaml:"cleanup,omitempty"`
	Logging     SoulLogging      `yaml:"logging,omitempty"`
	Metrics     *SoulMetrics     `yaml:"metrics,omitempty"`
	OTel        *SoulOTel        `yaml:"otel,omitempty"`

	PluginRuntime *PluginRuntime `yaml:"plugin_runtime,omitempty"`
	HotReload     *HotReload     `yaml:"hot_reload,omitempty"`
}

// SoulPaths — file-system раскладка Soul-а.
type SoulPaths struct {
	Modules string `yaml:"modules,omitempty"`
	Seed    string `yaml:"seed,omitempty"`
}

// SoulKeeper — подключение к Keeper-кластеру (см. docs/soul/connection.md).
//
// MaxApplySizeMB — потолок размера одного входящего FromKeeper-сообщения,
// прежде всего `ApplyRequest` с пачкой отрендеренных `RenderedTask` (рендер
// Destiny — Keeper-side, ADR-012). Применяется как
// `grpc.MaxCallRecvMsgSize` в dial EventStream-клиента (заменяет gRPC-дефолт
// 4 MiB, малый для крупного Destiny). 0/опущено → дефолт
// [DefaultMaxApplySizeMB] (8 MiB).
//
// Инвариант контракта Keeper↔Soul: Keeper-send-лимит
// (`listen.grpc.event_stream.max_apply_size_mb` в `keeper.yml`) должен быть
// ≤ этому recv-лимиту, иначе Keeper отправит то, что Soul отвергнет. Дефолты
// обеих сторон совпадают (8 MiB).
type SoulKeeper struct {
	Endpoints      []SoulKeeperEndpoint `yaml:"endpoints"`
	Retry          *SoulKeeperRetry     `yaml:"retry,omitempty"`
	Failback       *SoulKeeperFailback  `yaml:"failback,omitempty"`
	TLS            SoulKeeperTLS        `yaml:"tls"`
	MaxApplySizeMB int                  `yaml:"max_apply_size_mb,omitempty"`
}

// ResolvedMaxApplySize возвращает эффективный recv-лимит EventStream-клиента в
// байтах: 0/опущено → [DefaultMaxApplySizeMB]. Валидация (>0, ≥ минимума) —
// в schema-фазе; здесь только резолв дефолта.
func (k SoulKeeper) ResolvedMaxApplySize() int {
	mb := k.MaxApplySizeMB
	if mb <= 0 {
		mb = DefaultMaxApplySizeMB
	}
	return mb * bytesPerMiB
}

// SoulKeeperEndpoint — один Keeper-инстанс кластера. Хосты обеих фаз
// (Bootstrap и EventStream) совпадают, различаются только порты —
// поэтому один список покрывает обе фазы (см. docs/soul/connection.md).
//
// `bootstrap_port` обязателен наравне с `event_stream_port` — никакого
// молчаливого ухода bootstrap-фазы на event_stream-порт («безопасность
// на первом месте»; в проде порты обычно разные).
type SoulKeeperEndpoint struct {
	Host            string `yaml:"host"`
	EventStreamPort int    `yaml:"event_stream_port"`
	BootstrapPort   int    `yaml:"bootstrap_port"`
	Priority        int    `yaml:"priority,omitempty"`
}

// EventStreamAddr — `host:event_stream_port`, цель EventStream-фазы
// (`soul run`, mTLS bidi-stream). net.JoinHostPort оборачивает IPv6-литерал
// в скобки (`[::1]:9443`) — симметрия со schema.go::checkHostPort
// (net.SplitHostPort).
func (e SoulKeeperEndpoint) EventStreamAddr() string {
	return net.JoinHostPort(e.Host, strconv.Itoa(e.EventStreamPort))
}

// BootstrapAddr — `host:bootstrap_port`, цель bootstrap-фазы
// (`soul init`, server-only TLS unary RPC). См. EventStreamAddr про IPv6.
func (e SoulKeeperEndpoint) BootstrapAddr() string {
	return net.JoinHostPort(e.Host, strconv.Itoa(e.BootstrapPort))
}

type SoulKeeperRetry struct {
	MaxAttempts      int               `yaml:"max_attempts,omitempty"`
	Backoff          SoulKeeperBackoff `yaml:"backoff,omitempty"`
	HandshakeTimeout string            `yaml:"handshake_timeout,omitempty"`
}

type SoulKeeperBackoff struct {
	Initial string `yaml:"initial,omitempty"`
	Max     string `yaml:"max,omitempty"`
	Jitter  bool   `yaml:"jitter,omitempty"`
}

type SoulKeeperFailback struct {
	Enabled  bool   `yaml:"enabled"`
	Interval string `yaml:"interval,omitempty"`
	Spray    string `yaml:"spray,omitempty"`
}

type SoulKeeperTLS struct {
	CA string `yaml:"ca"`
}

// SoulSoulprint — параметры периодического сбора фактов.
type SoulSoulprint struct {
	RefreshInterval string `yaml:"refresh_interval,omitempty"`
}

// SoulUtilization — параметры периодической отправки живой утилизации хоста
// (ADR-072). `interval` — каденс pulse (default 30s, floor 10s в cmd/soul).
type SoulUtilization struct {
	Interval string `yaml:"interval,omitempty"`
}

// SoulCleanup — локальный cleanup-цикл кеша модулей.
type SoulCleanup struct {
	ModulesTTLDays int    `yaml:"modules_ttl_days,omitempty"`
	RunInterval    string `yaml:"run_interval,omitempty"`
}

// SoulLogging — отличается от KeeperLogging дефолтами; схема — та же.
type SoulLogging struct {
	Level    string           `yaml:"level,omitempty"`
	Format   string           `yaml:"format,omitempty"`
	File     string           `yaml:"file,omitempty"`
	Rotation *LoggingRotation `yaml:"rotation,omitempty"`
}

// SoulMetrics — публикация метрик (опц.; на Soul выключаемо).
type SoulMetrics struct {
	Enabled   bool                  `yaml:"enabled"`
	Listen    string                `yaml:"listen,omitempty"`
	BasicAuth *SoulMetricsBasicAuth `yaml:"basic_auth,omitempty"`
}

// SoulMetricsBasicAuth — HTTP Basic-auth на soul-`/metrics`.
//
// Зеркало keeper-`metrics.auth.basic` (та же constant-time проверка в
// shared/obs), но источник пароля — **файл на диске**, не Vault-ref: у Soul
// нет vault-клиента (ADR-012), поэтому пароль резолвится из
// `password_file` (одна строка, trailing-newline отбрасывается). Plaintext
// пароля прямо в конфиге не допускаем («безопасность на первом месте»):
// только путь к файлу, права на который — забота оператора (рекомендация
// 0400). При Enabled оба — Username и PasswordFile — обязательны
// (валидация в schema-фазе).
type SoulMetricsBasicAuth struct {
	Enabled      bool   `yaml:"enabled"`
	Username     string `yaml:"username,omitempty"`
	PasswordFile string `yaml:"password_file,omitempty"`
}

// SoulOTel — OTel-экспорт Soul-а.
type SoulOTel struct {
	Enabled  bool   `yaml:"enabled"`
	Endpoint string `yaml:"endpoint,omitempty"`

	// ExportMetrics — опц. push метрик по OTLP в дополнение к
	// Prometheus-scrape (ADR-024 §1.2 / observability.md §5). Заглушка
	// под Slice 2: читается из конфига, но OTLP-метрик-pipeline ещё не
	// поднимается (в Slice 0 экспортируются только трейсы).
	ExportMetrics bool `yaml:"export_metrics,omitempty"`
}
