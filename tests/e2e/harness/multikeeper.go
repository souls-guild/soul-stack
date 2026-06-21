//go:build e2e

package harness

// Multi-keeper crash-harness (GA recovery-доказательство). Расширяет
// single-keeper Stack до N keeper-субпроцессов поверх ОБЩИХ PG / Redis / Vault:
// каждый со своим KID + своими listener-портами + включённым VoyageWorker-pool
// и reaper-правилом reclaim_voyages. Presence keeper-инстансов — через общий
// Redis Conclave (как в prod-HA-кластере).
//
// Цель: убить ПРОЦЕСС keeper-владельца Voyage mid-run (настоящий SIGKILL, не
// SQL-эмуляция) и доказать end-to-end recovery — другой живой keeper
// подхватывает (reclaim_voyages → re-claim) и доводит прогон до терминала.
//
// Single-keeper тесты (NewStack) этот файл НЕ затрагивает: всё аддитивно.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// keeperProc — один живой keeper-субпроцесс мульти-кластера.
type keeperProc struct {
	kid      string
	cmd      *exec.Cmd
	httpURL  string
	grpcAddr string
	// killed — флаг, что процесс уже убит тестом (cleanup тогда не шлёт сигнал
	// мёртвому процессу повторно).
	killed bool
}

// MultiKeeperConfig — параметры мульти-keeper crash-стенда.
type MultiKeeperConfig struct {
	// Keepers — число keeper-субпроцессов (≥2 для crash-сценария).
	Keepers int
	// Souls — число pre-auth soul-stub-ов (общих для всего кластера).
	Souls int
	// VoyageLeaseTTL — TTL PG-claim-lease строки voyages. Короткий (3-5s),
	// чтобы после SIGKILL владельца протухший claim быстро попал под
	// reclaim_voyages. Пустой → дефолт 4s.
	VoyageLeaseTTL time.Duration
}

// NewMultiKeeperStack поднимает общий PG/Redis/Vault + N keeper-субпроцессов и
// блокируется до готовности каждого (/readyz). Возвращает Stack, у которого
// Stack.KeeperHTTPURL / KeeperGRPCAddr указывают на keeper[0] (primary —
// точка входа Operator-API и EventStream soul-stub-ов); reclaim, однако,
// исполняет ЛЮБОЙ живой keeper кластера через общий Reaper-leader.
func NewMultiKeeperStack(t *testing.T, cfg MultiKeeperConfig) *Stack {
	t.Helper()
	if cfg.Keepers < 2 {
		t.Fatalf("NewMultiKeeperStack: нужно ≥2 keeper-а для crash-сценария, задано %d", cfg.Keepers)
	}
	if cfg.Souls <= 0 {
		cfg.Souls = 1
	}
	if cfg.VoyageLeaseTTL <= 0 {
		cfg.VoyageLeaseTTL = 4 * time.Second
	}

	if _, err := locateKeeperBinary(); err != nil {
		t.Skipf("multi-keeper: keeper-бинарь не найден (%v); экспортируй KEEPER_BIN или сделай `make build`", err)
	}

	s := &Stack{
		t:      t,
		cfg:    Config{Souls: cfg.Souls},
		tmpDir: t.TempDir(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Общая инфра (как в NewStack).
	if err := s.startPostgres(ctx); err != nil {
		s.runCleanups()
		t.Fatalf("multi-keeper: postgres: %v", err)
	}
	if err := s.startRedis(ctx); err != nil {
		s.runCleanups()
		t.Fatalf("multi-keeper: redis: %v", err)
	}
	if err := s.startVault(ctx); err != nil {
		s.runCleanups()
		t.Fatalf("multi-keeper: vault: %v", err)
	}
	InitVaultTestSecrets(t, s)

	// Общий server-cert для всех keeper-ов (один CA — soul-stub верифицирует
	// server-cert любого keeper-а по нему). SAN включает 127.0.0.1, поэтому
	// один cert годится всем loopback-listener-ам.
	keeperCertPEM, keeperKeyPEM, caPEM := IssueKeeperServerCert(t, s)
	s.caBundle = caPEM
	tlsDir := filepath.Join(s.tmpDir, "tls")
	if err := os.MkdirAll(tlsDir, 0o755); err != nil {
		t.Fatalf("multi-keeper: mkdir tls: %v", err)
	}
	certPath := filepath.Join(tlsDir, "keeper.crt")
	keyPath := filepath.Join(tlsDir, "keeper.key")
	caPath := filepath.Join(tlsDir, "vault-ca.crt")
	mustWrite(t, certPath, keeperCertPEM, 0o644)
	mustWrite(t, keyPath, keeperKeyPEM, 0o600)
	mustWrite(t, caPath, caPEM, 0o644)

	// PG pool для direct SQL (assert claimed_by_kid / status).
	pool, err := pgxpool.New(ctx, s.PGURL)
	if err != nil {
		s.runCleanups()
		t.Fatalf("multi-keeper: pgxpool.New: %v", err)
	}
	s.db = pool
	s.cleanups = append(s.cleanups, func() { pool.Close() })

	// PG DSN в Vault (keeper.yml::postgres.dsn_ref ссылается сюда).
	s.seedPostgresDSN()

	// Рендер per-keeper YAML + аллокация портов. Первый keeper (i==0) —
	// soul-holder primary: его HTTP/gRPC-адреса проставляются в Stack (опорные
	// для opClient и ConnectSoulStub), и он НЕ запускает VoyageWorker-pool
	// (voyage.workers: 0) — значит, никогда не становится владельцем Voyage.
	// Остальные keeper-ы (i>=1) — VoyageWorker-ы без подключённых soul-ов.
	//
	// Зачем разделять: soul-stub-ы подключаются стримом к primary; убийство
	// Voyage-владельца (всегда i>=1) НЕ роняет soul-стримы. Apply-шаги
	// per-incarnation scenario-run-а Voyage-владельца маршрутизируются на
	// soul-holder через cluster-routing (Redis applybus, cluster_routing=true).
	// Так crash Voyage-владельца оставляет флот живым, и reclaim-keeper
	// доисполняет прогон против всё ещё подключённых soul-ов.
	for i := 0; i < cfg.Keepers; i++ {
		kid := fmt.Sprintf("keeper-mk-%02d", i)
		voyageWorkers := 2
		if i == 0 {
			voyageWorkers = 0 // soul-holder не претендует на Voyage
		}
		yamlPath, httpURL, grpcAddr := s.buildMultiKeeperYAML(kid, certPath, keyPath, caPath, cfg.VoyageLeaseTTL, voyageWorkers)

		if i == 0 {
			// Bootstrap первого Архонта — единожды, на primary-конфиге (трогает
			// только PG/Vault, не listener-ы). JWT общий для всего кластера.
			credPath := s.runKeeperInit(yamlPath)
			jwtBytes, rerr := os.ReadFile(credPath)
			if rerr != nil {
				s.runCleanups()
				t.Fatalf("multi-keeper: read credential-out: %v", rerr)
			}
			s.JWT = strings.TrimSpace(string(jwtBytes))
			s.KeeperHTTPURL = httpURL
			s.KeeperGRPCAddr = grpcAddr
		}

		kp, serr := s.spawnKeeperProc(kid, yamlPath, httpURL, grpcAddr)
		if serr != nil {
			s.runCleanups()
			t.Fatalf("multi-keeper: spawn keeper %s: %v", kid, serr)
		}
		s.keepers = append(s.keepers, kp)
	}

	// Pre-auth soul-stub-ы (общие; ConnectSoulStub откроет стрим к primary).
	for i := 0; i < cfg.Souls; i++ {
		sid := fmt.Sprintf("soul-mk-%d.example.com", i)
		cert, key := RegisterSoulPreAuth(t, s, sid)
		s.souls = append(s.souls, soulIdentity{SID: sid, Cert: cert, Key: key})
	}

	return s
}

// buildMultiKeeperYAML рендерит keeper.yml для одного keeper-а мульти-кластера:
// уникальный KID + свои listener-порты, общий PG/Redis/Vault, включённые
// VoyageWorker-pool (короткий lease) и reaper-правило reclaim_voyages. Пишет
// YAML в tmpDir/<kid>.yml. Возвращает (yamlPath, httpURL, grpcEventStreamAddr).
func (s *Stack) buildMultiKeeperYAML(kid, certPath, keyPath, caPath string, leaseTTL time.Duration, voyageWorkers int) (string, string, string) {
	bootstrapAddr := allocLoopback(s.t)
	eventStreamAddr := allocLoopback(s.t)
	httpAddr := allocLoopback(s.t)
	mcpAddr := allocLoopback(s.t)
	metricsAddr := allocLoopback(s.t)

	pluginsCacheDir := filepath.Join(s.tmpDir, kid, "plugins")
	socketsDir := filepath.Join(s.tmpDir, kid, "plugin-sockets")

	// Renew ~1/3 TTL; reaper-интервал и reclaim-stale короткие, чтобы reclaim
	// сработал в пределах секунд после протухания lease убитого владельца.
	leaseRenew := leaseTTL / 3
	if leaseRenew < time.Second {
		leaseRenew = time.Second
	}

	tmpl := `kid: %s

listen:
  grpc:
    bootstrap:
      addr: "%s"
      tls:
        cert: %s
        key:  %s
    event_stream:
      addr: "%s"
      tls:
        cert: %s
        key:  %s
        ca:   %s
  openapi: { addr: "%s" }
  mcp:     { addr: "%s" }
  metrics: { addr: "%s" }

postgres:
  dsn_ref: vault:secret/keeper/postgres
  pool: { min: 2, max: 5 }

redis:
  addr: "%s"
  password_ref: ""

vault:
  addr: "%s"
  token: "%s"
  auth:
    method: token
  pki_mount: "pki"
  pki_role: "soul-seed"

auth:
  jwt:
    signing_key_ref: vault:secret/keeper/jwt-signing-key
    issuer: keeper-mk
    ttl_default: 24h
    ttl_bootstrap: 720h

logging:
  level: info
  format: text
  rotation: { max_size_mb: 100, max_files: 5, compress: false }

plugins:
  cache_root: %s

plugin_runtime:
  socket_dir: %s
  startup_timeout: 10s
  shutdown_grace: 10s
  allowed_capabilities:
    - run_as_root
    - network_outbound
    - network_inbound
    - vault_access
    - fs_write_root
    - exec_subprocess
  conflict_policy: warn
  enable_tls: false

hot_reload:
  enable_signal: false
  enable_inotify: false
  audit_correlation_id: true

audit:
  enabled: true
  otel_export: false
  retention_days: 365

watchman_interval: 5s
watchman_fail_threshold: 3
allow_unsafe_single_path_multi_keeper: true

acolytes: 2

voyage:
  workers: %d
  lease_ttl: %s
  lease_renew_interval: %s
  poll_interval: 1s

reaper:
  enabled: true
  interval: 500ms
  dry_run: false
  batch_size: 1000
  lock_ttl: 2s
  rules:
    reclaim_voyages:
      enabled: true
      stale_after: 1s
`
	yaml := fmt.Sprintf(tmpl,
		kid,
		bootstrapAddr, certPath, keyPath,
		eventStreamAddr, certPath, keyPath, caPath,
		httpAddr, mcpAddr, metricsAddr,
		s.RedisAddr,
		s.VaultAddr, s.vaultToken,
		pluginsCacheDir, socketsDir,
		voyageWorkers, durationYAML(leaseTTL), durationYAML(leaseRenew),
	)

	yamlPath := filepath.Join(s.tmpDir, kid+".yml")
	mustWrite(s.t, yamlPath, []byte(yaml), 0o600)

	return yamlPath, "http://" + httpAddr, eventStreamAddr
}

// spawnKeeperProc запускает `keeper run` для одного KID и блокируется до
// готовности (/readyz). Cleanup-handler шлёт SIGINT (если процесс ещё жив).
func (s *Stack) spawnKeeperProc(kid, yamlPath, httpURL, grpcAddr string) (*keeperProc, error) {
	binaryPath := keeperBinaryPath(s.t)
	serviceCacheDir := filepath.Join(s.tmpDir, kid, "service-cache")
	destinyCacheDir := filepath.Join(s.tmpDir, kid, "destiny-cache")
	pluginWorkDir := filepath.Join(s.tmpDir, kid, "plugin-src")

	cmd := exec.Command(binaryPath, "run", "--config", yamlPath)
	cmd.Env = append(os.Environ(),
		"SOUL_STACK_ALLOW_FILE_REPOS=1",
		"KEEPER_SERVICE_CACHE_DIR="+serviceCacheDir,
		"KEEPER_DESTINY_CACHE_DIR="+destinyCacheDir,
		"KEEPER_PLUGIN_WORK_DIR="+pluginWorkDir,
	)
	cmd.Stdout = &testLogWriter{t: s.t, prefix: kid + "-stdout"}
	cmd.Stderr = &testLogWriter{t: s.t, prefix: kid + "-stderr"}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start: %w", err)
	}
	kp := &keeperProc{kid: kid, cmd: cmd, httpURL: httpURL, grpcAddr: grpcAddr}

	s.cleanups = append(s.cleanups, func() {
		if kp.killed || cmd.Process == nil {
			return
		}
		_ = cmd.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			_ = cmd.Process.Kill()
		}
	})

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if probeReady(httpURL + "/readyz") {
			return kp, nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return nil, errors.New("/readyz did not become healthy in 60s")
}

// KillKeeperByKID шлёт SIGKILL keeper-процессу с заданным KID (настоящий kill
// процесса — не SQL-эмуляция). Fatal, если KID неизвестен или уже убит. После
// kill блокируется до фактического выхода процесса (cmd.Wait), чтобы порты
// освободились и тест не ловил зомби-listener.
func (s *Stack) KillKeeperByKID(t *testing.T, kid string) {
	t.Helper()
	for _, kp := range s.keepers {
		if kp.kid != kid {
			continue
		}
		if kp.killed {
			t.Fatalf("KillKeeperByKID(%s): процесс уже убит", kid)
		}
		if kp.cmd.Process == nil {
			t.Fatalf("KillKeeperByKID(%s): процесс не запущен", kid)
		}
		if err := kp.cmd.Process.Signal(syscall.SIGKILL); err != nil {
			t.Fatalf("KillKeeperByKID(%s): SIGKILL: %v", kid, err)
		}
		kp.killed = true
		_ = kp.cmd.Wait() // снимаем зомби; код выхода нерелевантен (SIGKILL)
		t.Logf("multi-keeper: SIGKILL отправлен и подтверждён для %s", kid)
		return
	}
	t.Fatalf("KillKeeperByKID(%s): KID не найден среди %d keeper-ов", kid, len(s.keepers))
}

// LiveKeeperKIDs возвращает KID-ы ещё живых (не убитых) keeper-ов.
func (s *Stack) LiveKeeperKIDs() []string {
	var out []string
	for _, kp := range s.keepers {
		if !kp.killed {
			out = append(out, kp.kid)
		}
	}
	return out
}

// AllKeeperGRPCAddrs возвращает EventStream-gRPC-адреса всех keeper-ов кластера в
// порядке spawn-а (mk-00 первым). Зеркало soul.yml::keeper.endpoints — soul-stub
// использует список для reconnect-fallback при смерти keeper-холдера стрима.
func (s *Stack) AllKeeperGRPCAddrs() []string {
	out := make([]string, 0, len(s.keepers))
	for _, kp := range s.keepers {
		out = append(out, kp.grpcAddr)
	}
	return out
}

// LiveKeeperGRPCAddrs возвращает EventStream-gRPC-адреса ещё живых keeper-ов
// (исключая убитых SIGKILL-ом). Для reconnect-fallback стаба после краша holder-а.
func (s *Stack) LiveKeeperGRPCAddrs() []string {
	out := make([]string, 0, len(s.keepers))
	for _, kp := range s.keepers {
		if !kp.killed {
			out = append(out, kp.grpcAddr)
		}
	}
	return out
}

// KeeperKIDForGRPCAddr резолвит EventStream-gRPC-адрес → KID keeper-а (обратный
// маппинг к AllKeeperGRPCAddrs). Нужен тесту, чтобы по адресу keeper-холдера
// стрима стаба узнать, какой KID убивать. Пустая строка, если адрес неизвестен.
func (s *Stack) KeeperKIDForGRPCAddr(addr string) string {
	for _, kp := range s.keepers {
		if kp.grpcAddr == addr {
			return kp.kid
		}
	}
	return ""
}

// mustWrite пишет файл, fatal при ошибке.
func mustWrite(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// durationYAML форматирует time.Duration в Go-duration-строку для keeper.yml
// (config-парсер принимает "4s"/"1s" формат time.ParseDuration).
func durationYAML(d time.Duration) string {
	return d.String()
}
