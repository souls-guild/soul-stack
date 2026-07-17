package config

import (
	"net"
	"strconv"
)

// SoulConfig is the typed representation of `soul.yml`.
// Normative block spec — [docs/soul/config.md].
//
// Soul has no `auth:` block (mTLS / SoulSeed instead of JWT, see ADR-014).
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

// SoulPaths is the Soul's file-system layout.
type SoulPaths struct {
	Modules string `yaml:"modules,omitempty"`
	Seed    string `yaml:"seed,omitempty"`
}

// SoulKeeper is the connection to the Keeper cluster (see docs/soul/connection.md).
//
// MaxApplySizeMB caps the size of a single incoming FromKeeper message,
// primarily `ApplyRequest` carrying a batch of rendered `RenderedTask`
// (Destiny render is Keeper-side, ADR-012). Applied as
// `grpc.MaxCallRecvMsgSize` on the EventStream client dial (replaces the 4 MiB
// gRPC default, too small for a large Destiny). 0/omitted → default
// [DefaultMaxApplySizeMB] (8 MiB).
//
// Keeper↔Soul contract invariant: the Keeper send-limit
// (`listen.grpc.event_stream.max_apply_size_mb` in `keeper.yml`) must be
// ≤ this recv-limit, else Keeper sends what Soul rejects. Both defaults match
// (8 MiB).
type SoulKeeper struct {
	Endpoints      []SoulKeeperEndpoint `yaml:"endpoints"`
	Retry          *SoulKeeperRetry     `yaml:"retry,omitempty"`
	Failback       *SoulKeeperFailback  `yaml:"failback,omitempty"`
	TLS            SoulKeeperTLS        `yaml:"tls"`
	MaxApplySizeMB int                  `yaml:"max_apply_size_mb,omitempty"`
}

// ResolvedMaxApplySize returns the effective EventStream client recv-limit in
// bytes: 0/omitted → [DefaultMaxApplySizeMB]. Validation (>0, ≥ minimum) is in
// the schema phase; here only default resolution.
func (k SoulKeeper) ResolvedMaxApplySize() int {
	mb := k.MaxApplySizeMB
	if mb <= 0 {
		mb = DefaultMaxApplySizeMB
	}
	return mb * bytesPerMiB
}

// SoulKeeperEndpoint is one Keeper cluster instance. Both phases (Bootstrap and
// EventStream) share the same hosts and differ only by port, so one list covers
// both phases (see docs/soul/connection.md).
//
// `bootstrap_port` is required alongside `event_stream_port` — no silent
// fallback of the bootstrap phase onto the event_stream port ("security first";
// in prod the ports usually differ).
type SoulKeeperEndpoint struct {
	Host            string `yaml:"host"`
	EventStreamPort int    `yaml:"event_stream_port"`
	BootstrapPort   int    `yaml:"bootstrap_port"`
	Priority        int    `yaml:"priority,omitempty"`
}

// EventStreamAddr is `host:event_stream_port`, the EventStream-phase target
// (`soul run`, mTLS bidi-stream). net.JoinHostPort wraps an IPv6 literal in
// brackets (`[::1]:9443`) — symmetric with schema.go::checkHostPort
// (net.SplitHostPort).
func (e SoulKeeperEndpoint) EventStreamAddr() string {
	return net.JoinHostPort(e.Host, strconv.Itoa(e.EventStreamPort))
}

// BootstrapAddr is `host:bootstrap_port`, the bootstrap-phase target
// (`soul init`, server-only TLS unary RPC). See EventStreamAddr on IPv6.
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

// SoulSoulprint holds the periodic fact-collection parameters.
type SoulSoulprint struct {
	RefreshInterval string `yaml:"refresh_interval,omitempty"`
}

// SoulUtilization — параметры периодической отправки живой утилизации хоста
// (ADR-072). `interval` — каденс pulse (default 30s, floor 10s в cmd/soul).
type SoulUtilization struct {
	Interval string `yaml:"interval,omitempty"`
}

// SoulCleanup is the local module-cache cleanup cycle.
type SoulCleanup struct {
	ModulesTTLDays int    `yaml:"modules_ttl_days,omitempty"`
	RunInterval    string `yaml:"run_interval,omitempty"`
}

// SoulLogging differs from KeeperLogging only in defaults; same schema.
type SoulLogging struct {
	Level    string           `yaml:"level,omitempty"`
	Format   string           `yaml:"format,omitempty"`
	File     string           `yaml:"file,omitempty"`
	Rotation *LoggingRotation `yaml:"rotation,omitempty"`
}

// SoulMetrics is metrics publishing (optional; disableable on Soul).
type SoulMetrics struct {
	Enabled   bool                  `yaml:"enabled"`
	Listen    string                `yaml:"listen,omitempty"`
	BasicAuth *SoulMetricsBasicAuth `yaml:"basic_auth,omitempty"`
}

// SoulMetricsBasicAuth is HTTP Basic-auth on soul-`/metrics`.
//
// Mirrors keeper-`metrics.auth.basic` (same constant-time check in shared/obs),
// but the password source is a **file on disk**, not a Vault-ref: Soul has no
// vault client (ADR-012), so the password is resolved from `password_file`
// (single line, trailing newline dropped). A plaintext password in the config
// is not allowed ("security first"): only a file path, whose permissions are the
// operator's concern (0400 recommended). When Enabled, both Username and
// PasswordFile are required (validated in the schema phase).
type SoulMetricsBasicAuth struct {
	Enabled      bool   `yaml:"enabled"`
	Username     string `yaml:"username,omitempty"`
	PasswordFile string `yaml:"password_file,omitempty"`
}

// SoulOTel is the Soul's OTel export.
type SoulOTel struct {
	Enabled  bool   `yaml:"enabled"`
	Endpoint string `yaml:"endpoint,omitempty"`

	// ExportMetrics optionally pushes metrics over OTLP in addition to
	// Prometheus scrape (ADR-024 §1.2 / observability.md §5). Stub for Slice 2:
	// read from config, but the OTLP metrics pipeline is not up yet (Slice 0
	// exports traces only).
	ExportMetrics bool `yaml:"export_metrics,omitempty"`
}
