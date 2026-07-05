//go:build e2e_live

// Package harness — reusable test-helpers для L3b E2E-тестирования
// (real-soul-in-container, ADR-039).
//
// L3b отличается от L3a тем, что вместо soul-stub-helper-пакета поднимает
// реальный soul-binary в Linux-контейнере (Debian-12 systemd-PID-1) и проходит
// реальный CSR-Bootstrap-flow. См. tests/e2e-live/README.md.
//
// Stack — единица изоляции теста: один тест = один Stack = свой PG / Redis /
// Vault + свой Keeper-процесс + N soul-контейнеров. NewStack блокируется до
// полной готовности инфры. soul-container spawn появится в L3b-2-slice.
//
// Архитектурные инварианты (ADR-039 Amendment 2026-05-26):
//   - harness НЕ импортирует `keeper/internal/*` (Go internal-rules);
//   - все DB-операции — direct SQL через pgx;
//   - все Vault-операции — direct HTTP API (см. vault.go);
//   - Keeper-процесс — sub-process реального бинаря, не in-process импорт;
//   - soul — реальный binary в privileged-контейнере, cross-compiled `make build-linux`.
package harness

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"encoding/json"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Config — параметры конструирования Stack-а.
//
// ExamplePath — относительный путь к каталогу service-а в репо (например
// "examples/service/smoke-nginx-live"). Harness читает его, делает snapshot и
// заводит per-test bare git-repo в $TMP (L3b-3).
//
// Souls — количество soul-контейнеров, спавнящихся через SpawnSoulContainer
// (L3b-2+). 0 — keeper-only стенд (без soul-контейнеров и без pre-flight
// требования soul-linux-бинаря): лёгкие тесты keeper-side поверхностей
// (plugin-канал NIM-32 S1 и т.п.).
//
// SoulModules — каталог `plugins.soul_modules[]` keeper.yml (ADR-065(b)):
// SoulModule-плагины, которые keeper при старте git-резолвит в cache_root
// (plugingit, слот `<ns>-<name>/current/`). Непустой список автоматически
// включает Sigil (config_builder пишет sigil-блок, NewStack сеет
// ed25519-ключ подписи в Vault) — без Signer-а allow-флоу не поднимается.
type Config struct {
	ExamplePath string
	ServiceName string
	Souls       int
	SoulModules []SoulModuleEntry
}

// SoulModuleEntry — одна запись каталога `plugins.soul_modules[]`
// (`{name, source, ref}`, зеркало config.PluginCatalogEntry — harness не
// импортирует shared/config, публичный контракт тестируется как чёрный ящик).
type SoulModuleEntry struct {
	Name   string
	Source string
	Ref    string
}

// Stack — изолированный E2E-стенд одного теста.
type Stack struct {
	t *testing.T

	cfg Config

	// Resolved endpoints (заполняются NewStack-ом после spawn-а).
	PGURL               string
	RedisAddr           string
	VaultAddr           string
	KeeperHTTPURL       string
	KeeperGRPCAddr      string
	KeeperBootstrapGRPC string
	// MetricsURL — Prometheus-эндпоинт keeper-а (отдельный listener, ADR-024).
	MetricsURL string

	// JWT — credential первого Архонта, прочитанный из credential-файла
	// `keeper init --credential-out=...`.
	JWT string

	// SoulContainers — populated SpawnSoulContainer-ом в L3b-2+; nil/empty на L3b-1.
	SoulContainers []*SoulContainer

	// PluginCacheRoot — `plugins.cache_root` keeper.yml (заполняется
	// buildKeeperYAML-ом): сюда plugingit-резолвер материализует слоты
	// `<ns>-<name>/current/` каталога плагинов (ADR-065(b)/(g)). Тесты
	// plugin-канала ассертят слот по этому пути.
	PluginCacheRoot string

	// caBundle — PEM-bundle Vault PKI root CA, выданный IssueKeeperServerCert-ом.
	// soul-контейнер mount-ит его как `/etc/soul/ca.pem`, чтобы верифицировать
	// keeper-server-cert при server-only TLS-handshake-е (`soul init`).
	caBundle []byte

	// Порты keeper-gRPC-listener-ов. YAML биндит на `0.0.0.0:<port>` (доступно
	// host-side-probe + контейнеру через host.docker.internal), а Stack-поля
	// KeeperBootstrapGRPC/KeeperGRPCAddr — `127.0.0.1:<port>` (host-side).
	bootstrapPort   int
	eventStreamPort int

	// dockerNetwork — user-defined bridge для soul-контейнеров. nil на L3a-style
	// прогонах (cfg.Souls=0); создаётся при первом SpawnSoulContainer-е.
	dockerNetwork *testcontainers.DockerNetwork

	// Internal state.
	vaultToken string
	tmpDir     string

	db *pgxpool.Pool

	keeperCmd *exec.Cmd

	containers []testcontainers.Container

	// Cleanup-shutdown order: LIFO через cleanups (как defer-ы); NewStack
	// аккумулирует teardown-handler-ы по мере поднятия зависимостей, Cleanup
	// сливает в обратном порядке.
	cleanups []func()
}

// NewStack поднимает изолированный стенд и блокируется до готовности.
//
// L3b-1: поднимает PG/Redis/Vault/keeper тем же путём, что L3a (копия harness-а).
// soul-container spawn deferred to L3b-2 slice — параметр cfg.Souls на L3b-1
// логируется warn-ом, но никакие контейнеры не поднимаются.
//
// Pre-flight (L3b-specific):
//   - keeper-бинарь (env `KEEPER_BIN` или дефолтный `./keeper/bin/keeper`);
//     `make build` собирает нативный бинарь (host-arch), L3b-1 запускает
//     keeper НА ХОСТЕ (не в контейнере) — нативный keeper подходит.
//   - soul-linux-binary (env `SOUL_BIN_LINUX` или дефолтный
//     `./soul/bin/soul-linux-amd64`) — нужен в L3b-2+ для mount в контейнер.
//     L3b-1 пока только проверяет наличие (skip-тест если не собран).
//
// Без любого из бинарей — t.Skip с подсказкой про `make build` / `make build-linux`.
func NewStack(t *testing.T, cfg Config) *Stack {
	t.Helper()
	if cfg.Souls < 0 {
		cfg.Souls = 0
	}

	// Pre-flight: keeper-бинарь (нативный, host-arch).
	if _, err := locateKeeperBinary(); err != nil {
		t.Skipf("L3b: keeper-бинарь не найден (%v); экспортируй KEEPER_BIN или сделай `make build`", err)
	}
	// Pre-flight: linux-soul-бинарь (для L3b-2+). Keeper-only стенд (Souls=0)
	// его не монтирует — не требуем.
	if cfg.Souls > 0 {
		if _, err := locateLinuxSoulBinary(); err != nil {
			t.Skipf("L3b: soul-linux-amd64 не найден (%v); экспортируй SOUL_BIN_LINUX или сделай `make build-linux`", err)
		}
	}

	s := &Stack{
		t:      t,
		cfg:    cfg,
		tmpDir: t.TempDir(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if err := s.startPostgres(ctx); err != nil {
		s.runCleanups()
		t.Fatalf("NewStack: postgres: %v", err)
	}
	if err := s.startRedis(ctx); err != nil {
		s.runCleanups()
		t.Fatalf("NewStack: redis: %v", err)
	}
	if err := s.startVault(ctx); err != nil {
		s.runCleanups()
		t.Fatalf("NewStack: vault: %v", err)
	}

	// Vault test-secrets: PKI + JWT signing-key. Симметрично provision.sh.
	InitVaultTestSecrets(t, s)

	// Sigil-ключ подписи — только при непустом каталоге SoulModules: keeper с
	// sigil-блоком в конфиге падает на старте без Vault-секрета (buildSigilSigner
	// cfg-fallback), а без sigil-блока plugin.allow-роуты не регистрируются.
	if len(cfg.SoulModules) > 0 {
		SeedSigilSigningKey(t, s)
	}

	// Outgoing-TLS material для keeper-server listener-ов.
	keeperCertPEM, keeperKeyPEM, caPEM := IssueKeeperServerCert(t, s)
	s.caBundle = caPEM
	tlsDir := filepath.Join(s.tmpDir, "tls")
	if err := os.MkdirAll(tlsDir, 0o755); err != nil {
		t.Fatalf("NewStack: mkdir tls: %v", err)
	}
	certPath := filepath.Join(tlsDir, "keeper.crt")
	keyPath := filepath.Join(tlsDir, "keeper.key")
	caPath := filepath.Join(tlsDir, "vault-ca.crt")
	if err := os.WriteFile(certPath, keeperCertPEM, 0o644); err != nil {
		t.Fatalf("NewStack: write keeper.crt: %v", err)
	}
	if err := os.WriteFile(keyPath, keeperKeyPEM, 0o600); err != nil {
		t.Fatalf("NewStack: write keeper.key: %v", err)
	}
	if err := os.WriteFile(caPath, caPEM, 0o644); err != nil {
		t.Fatalf("NewStack: write vault-ca.crt: %v", err)
	}

	// keeper.yml — рендерится в tmpDir.
	keeperYAML := s.buildKeeperYAML(certPath, keyPath, caPath)
	keeperYAMLPath := filepath.Join(s.tmpDir, "keeper.yml")
	if err := os.WriteFile(keeperYAMLPath, []byte(keeperYAML), 0o600); err != nil {
		t.Fatalf("NewStack: write keeper.yml: %v", err)
	}

	// PG connection pool — для direct SQL после bootstrap.
	pool, err := pgxpool.New(ctx, s.PGURL)
	if err != nil {
		s.runCleanups()
		t.Fatalf("NewStack: pgxpool.New: %v", err)
	}
	s.db = pool
	s.cleanups = append(s.cleanups, func() { pool.Close() })

	// Bootstrap: keeper init --credential-out=...
	credPath := s.runKeeperInit(keeperYAMLPath)
	jwtBytes, err := os.ReadFile(credPath)
	if err != nil {
		s.runCleanups()
		t.Fatalf("NewStack: read credential-out %s: %v", credPath, err)
	}
	s.JWT = strings.TrimSpace(string(jwtBytes))

	// keeper run — sub-process.
	if err := s.startKeeperRun(keeperYAMLPath); err != nil {
		s.runCleanups()
		t.Fatalf("NewStack: keeper run: %v", err)
	}

	// Service-registration (L3b-3): материализует cfg.ExamplePath в per-test
	// git-репо и регистрирует cfg.ServiceName@main через POST /v1/services. Без
	// этого CreateIncarnation отвечает 422 «service is not registered» (ADR-029).
	// Порядок к soul-spawn-у не привязан — соул для регистрации не нужен.
	s.registerExampleService(t)

	// soul-container spawn (L3b-2): по одному privileged Debian-12 systemd-PID-1
	// контейнеру на каждый запрошенный Soul. Имена детерминированные —
	// `soul-live-<idx>.example.com` (PKI-role soul-seed разрешает example.com).
	for i := 0; i < cfg.Souls; i++ {
		sid := fmt.Sprintf("soul-live-%c.example.com", 'a'+i)
		token := IssueBootstrapToken(t, s, sid)
		container := SpawnSoulContainer(t, s, sid, token)
		s.SoulContainers = append(s.SoulContainers, container)
	}

	return s
}

// Cleanup гасит весь стенд. Безопасен к повторному вызову.
func (s *Stack) Cleanup() {
	if s == nil {
		return
	}
	s.runCleanups()
}

func (s *Stack) runCleanups() {
	for i := len(s.cleanups) - 1; i >= 0; i-- {
		func(fn func()) {
			defer func() {
				if r := recover(); r != nil {
					s.t.Logf("cleanup panic: %v", r)
				}
			}()
			fn()
		}(s.cleanups[i])
	}
	s.cleanups = nil
}

// startPostgres поднимает PG-контейнер через testcontainers-go/modules/postgres.
func (s *Stack) startPostgres(ctx context.Context) error {
	pgC, err := tcpostgres.RunContainer(ctx,
		testcontainers.WithImage("postgres:16-alpine"),
		tcpostgres.WithDatabase("keeper"),
		tcpostgres.WithUsername("keeper"),
		tcpostgres.WithPassword("keeper"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		return fmt.Errorf("postgres container: %w", err)
	}
	s.containers = append(s.containers, pgC)
	s.cleanups = append(s.cleanups, func() {
		ctxTo, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = pgC.Terminate(ctxTo)
	})

	dsn, err := pgC.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return fmt.Errorf("postgres dsn: %w", err)
	}
	s.PGURL = dsn
	return nil
}

func (s *Stack) startRedis(ctx context.Context) error {
	rC, err := tcredis.RunContainer(ctx,
		testcontainers.WithImage("redis:7-alpine"),
	)
	if err != nil {
		return fmt.Errorf("redis container: %w", err)
	}
	s.containers = append(s.containers, rC)
	s.cleanups = append(s.cleanups, func() {
		ctxTo, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = rC.Terminate(ctxTo)
	})

	addr, err := rC.ConnectionString(ctx)
	if err != nil {
		return fmt.Errorf("redis addr: %w", err)
	}
	// ConnectionString отдаёт `redis://host:port`. Для keeper.yml::redis.addr
	// нужен host:port без схемы.
	addr = strings.TrimPrefix(addr, "redis://")
	s.RedisAddr = addr
	return nil
}

func (s *Stack) startVault(ctx context.Context) error {
	const rootToken = "root-test-token"
	req := testcontainers.ContainerRequest{
		Image:        "hashicorp/vault:1.15",
		ExposedPorts: []string{"8200/tcp"},
		Env: map[string]string{
			"VAULT_DEV_ROOT_TOKEN_ID":  rootToken,
			"VAULT_DEV_LISTEN_ADDRESS": "0.0.0.0:8200",
		},
		WaitingFor: wait.ForLog("Root Token:").WithStartupTimeout(45 * time.Second),
	}
	vc, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		return fmt.Errorf("vault container: %w", err)
	}
	s.containers = append(s.containers, vc)
	s.cleanups = append(s.cleanups, func() {
		ctxTo, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = vc.Terminate(ctxTo)
	})

	host, err := vc.Host(ctx)
	if err != nil {
		return fmt.Errorf("vault host: %w", err)
	}
	port, err := vc.MappedPort(ctx, "8200")
	if err != nil {
		return fmt.Errorf("vault port: %w", err)
	}
	s.VaultAddr = fmt.Sprintf("http://%s:%s", host, port.Port())
	s.vaultToken = rootToken
	return nil
}

// runKeeperInit вызывает `keeper init` с каноническими флагами и возвращает
// путь к credential-файлу (JWT первого Архонта).
func (s *Stack) runKeeperInit(keeperYAMLPath string) string {
	s.t.Helper()
	binaryPath := keeperBinaryPath(s.t)
	credentialPath := filepath.Join(s.tmpDir, "archon-test.credential")

	cmd := exec.Command(binaryPath, "init",
		"--archon=archon-test",
		"--config", keeperYAMLPath,
		"--credential-out", credentialPath,
	)
	cmd.Env = append(os.Environ(), "SOUL_STACK_ALLOW_FILE_REPOS=1")

	output, err := cmd.CombinedOutput()
	if err != nil {
		s.t.Fatalf("keeper init failed: %v\nOUTPUT:\n%s", err, output)
	}
	return credentialPath
}

// startKeeperRun спавнит `keeper run` как sub-process. Блокируется до того,
// как HTTP-listener начнёт отвечать (поллинг /readyz).
func (s *Stack) startKeeperRun(keeperYAMLPath string) error {
	binaryPath := keeperBinaryPath(s.t)
	// Service/destiny git-снапшоты artifact-loader-а кешируются в каталоге,
	// дефолт которого `/var/lib/soul-stack-keeper/...` (не writable: keeper в L3b
	// бежит на ХОСТЕ под обычным юзером). Перенаправляем в tmpDir через env-
	// override (KEEPER_SERVICE_CACHE_DIR / KEEPER_DESTINY_CACHE_DIR /
	// KEEPER_PLUGIN_WORK_DIR — см. cmd/keeper/main.go). Без этого load service
	// при CreateIncarnation падает 500 «mkdir /var/lib/...: permission denied»
	// на материализации service-снапшота из file://-репо (parity L3a).
	serviceCacheDir := filepath.Join(s.tmpDir, "service-cache")
	destinyCacheDir := filepath.Join(s.tmpDir, "destiny-cache")
	pluginWorkDir := filepath.Join(s.tmpDir, "plugin-src")
	cmd := exec.Command(binaryPath, "run", "--config", keeperYAMLPath)
	cmd.Env = append(os.Environ(),
		"SOUL_STACK_ALLOW_FILE_REPOS=1",
		"KEEPER_SERVICE_CACHE_DIR="+serviceCacheDir,
		"KEEPER_DESTINY_CACHE_DIR="+destinyCacheDir,
		"KEEPER_PLUGIN_WORK_DIR="+pluginWorkDir,
	)
	cmd.Stdout = &testLogWriter{t: s.t, prefix: "keeper-stdout"}
	cmd.Stderr = &testLogWriter{t: s.t, prefix: "keeper-stderr"}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start keeper run: %w", err)
	}
	s.keeperCmd = cmd
	s.cleanups = append(s.cleanups, func() {
		if cmd.Process == nil {
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

	// Wait /readyz.
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if probeReady(s.KeeperHTTPURL + "/readyz") {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return errors.New("keeper run: /readyz did not become healthy in 60s")
}

// keeperBinaryPath — путь к keeper-бинарю для exec-вызова. Fatal-fail при
// отсутствии (pre-flight в NewStack уже сделал Skip раньше).
func keeperBinaryPath(t *testing.T) string {
	t.Helper()
	path, err := locateKeeperBinary()
	if err != nil {
		t.Fatalf("keeperBinaryPath: %v", err)
	}
	return path
}

// locateKeeperBinary возвращает путь к keeper-бинарю без testing.TB-зависимости.
// Источник: env KEEPER_BIN (приоритет), иначе `$REPO/keeper/bin/keeper`
// (Makefile-таргет `make build`).
func locateKeeperBinary() (string, error) {
	if v := os.Getenv("KEEPER_BIN"); v != "" {
		if _, err := os.Stat(v); err != nil {
			return "", fmt.Errorf("KEEPER_BIN=%s: %w", v, err)
		}
		return v, nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("getwd: %w", err)
	}
	// tests/e2e-live/<test>.go → repo-root = wd/../..
	repoRoot := filepath.Clean(filepath.Join(wd, "..", ".."))
	candidate := filepath.Join(repoRoot, "keeper", "bin", "keeper")
	if _, err := os.Stat(candidate); err != nil {
		return "", fmt.Errorf("default %s: %w", candidate, err)
	}
	return candidate, nil
}

// locateLinuxSoulBinary — путь к cross-compiled soul-linux-amd64 для mount в
// soul-контейнер (L3b-2+). Источник: env SOUL_BIN_LINUX (приоритет), иначе
// `$REPO/soul/bin/soul-linux-amd64` (Makefile-таргет `make build-linux`).
//
// На L3b-1 функция только вызывается в pre-flight (Skip при отсутствии); сам
// mount контейнером — в L3b-2-slice.
func locateLinuxSoulBinary() (string, error) {
	if v := os.Getenv("SOUL_BIN_LINUX"); v != "" {
		if _, err := os.Stat(v); err != nil {
			return "", fmt.Errorf("SOUL_BIN_LINUX=%s: %w", v, err)
		}
		return v, nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("getwd: %w", err)
	}
	repoRoot := filepath.Clean(filepath.Join(wd, "..", ".."))
	candidate := filepath.Join(repoRoot, "soul", "bin", "soul-linux-amd64")
	if _, err := os.Stat(candidate); err != nil {
		return "", fmt.Errorf("default %s: %w", candidate, err)
	}
	return candidate, nil
}

// testLogWriter форвардит stdout/stderr keeper-процесса в t.Log.
type testLogWriter struct {
	t      *testing.T
	prefix string
}

func (w *testLogWriter) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		if line == "" {
			continue
		}
		w.t.Logf("[%s] %s", w.prefix, line)
	}
	return len(p), nil
}

// CreateIncarnation создаёт incarnation через Operator API Keeper-а.
//
// serviceRef — `<service>@<ref>`; harness отрезает суффикс `@<ref>`
// (ADR-029: POST /v1/incarnations принимает только bare service-name).
//
// 202 → возвращает имя incarnation. Любая другая статус-страница — t.Fatal
// с телом ответа.
//
// Retry на 422 «service is not registered»: реестр сервисов резолвится из
// in-memory Holder-snapshot (serviceregistry.Holder), который обновляется TTL-
// poll-ом (10s) + Redis pub/sub-инвалидацией. registerExampleService публикует
// сервис прямо перед потоком теста, и первый CreateIncarnation может прилететь
// в окне до того, как snapshot подхватил новую запись (POST вернул 201, но
// snapshot ещё устаревший). Это гонка регистрация↔snapshot-refresh, а не
// отсутствие сервиса — короткий retry закрывает её герметично, не трогая
// публичный контракт. На уже-видимом сервисе первый запрос проходит сразу.
func (s *Stack) CreateIncarnation(t *testing.T, name string, serviceRef string, spec map[string]any) string {
	t.Helper()
	c := s.opClient(t)
	service := stripServiceRef(serviceRef)
	body := map[string]any{
		"name":    name,
		"service": service,
	}
	if spec != nil {
		body["input"] = spec
	}

	var resp []byte
	var status int
	var err error
	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, status, err = c.post(context.Background(), "/v1/incarnations", body)
		if err != nil {
			t.Fatalf("CreateIncarnation %s: http: %v", name, err)
		}
		if status == http.StatusAccepted {
			break
		}
		if status == http.StatusUnprocessableEntity &&
			strings.Contains(string(resp), "is not registered") &&
			time.Now().Before(deadline) {
			time.Sleep(250 * time.Millisecond)
			continue
		}
		t.Fatalf("CreateIncarnation %s: status %d, body=%s", name, status, string(resp))
	}
	var out struct {
		ApplyID     string `json:"apply_id"`
		Incarnation string `json:"incarnation"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		t.Fatalf("CreateIncarnation %s: decode: %v (body=%s)", name, err, string(resp))
	}
	return out.Incarnation
}

// CreateIncarnationWithApply — как CreateIncarnation, но возвращает и apply_id
// авто-запущенного scenario `create`. POST /v1/incarnations сразу запускает
// create-прогон и переводит incarnation в `applying` (incarnation.go). Поэтому
// отдельный RunScenario(create) сразу после Create отвергается lock-gate-ом
// («incarnation уже в статусе applying» — run.go), и ожидание его apply_id
// зависает в WaitApplySuccess до timeout. Использовать этот метод вместо пары
// CreateIncarnation + RunScenario(create). Симметрично L3a-harness
// (tests/e2e/harness/stack.go::CreateIncarnationWithApply).
//
// create_scenario=`create` — Фаза-2 контракт (2026-06-29): выбор стартового
// сценария обязателен при непустом create-наборе сервиса; scenario обязан нести
// `create: true`. Bare-путь (без прогона) — CreateIncarnation.
//
// Возвращает (incarnationName, applyID авто-create-прогона).
func (s *Stack) CreateIncarnationWithApply(t *testing.T, name string, serviceRef string, spec map[string]any) (string, string) {
	t.Helper()
	c := s.opClient(t)
	service := stripServiceRef(serviceRef)
	body := map[string]any{
		"name":            name,
		"service":         service,
		"create_scenario": "create",
	}
	if spec != nil {
		body["input"] = spec
	}

	var resp []byte
	var status int
	var err error
	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, status, err = c.post(context.Background(), "/v1/incarnations", body)
		if err != nil {
			t.Fatalf("CreateIncarnationWithApply %s: http: %v", name, err)
		}
		if status == http.StatusAccepted {
			break
		}
		if status == http.StatusUnprocessableEntity &&
			strings.Contains(string(resp), "is not registered") &&
			time.Now().Before(deadline) {
			time.Sleep(250 * time.Millisecond)
			continue
		}
		t.Fatalf("CreateIncarnationWithApply %s: status %d, body=%s", name, status, string(resp))
	}
	var out struct {
		ApplyID     string `json:"apply_id"`
		Incarnation string `json:"incarnation"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		t.Fatalf("CreateIncarnationWithApply %s: decode: %v (body=%s)", name, err, string(resp))
	}
	if out.ApplyID == "" {
		t.Fatalf("CreateIncarnationWithApply %s: пустой apply_id в 202 body=%s (create-scenario не запущен?)", name, string(resp))
	}
	return out.Incarnation, out.ApplyID
}

// CreateIncarnationWithApplyScenario — как CreateIncarnationWithApply, но
// create-сценарий выбирается явно (сервисы с несколькими `create: true`, напр.
// redis create/create_from_souls/migrate_cluster, иначе POST → 422).
func (s *Stack) CreateIncarnationWithApplyScenario(t *testing.T, name, serviceRef, createScenario string, spec map[string]any) (string, string) {
	t.Helper()
	c := s.opClient(t)
	body := map[string]any{
		"name":            name,
		"service":         stripServiceRef(serviceRef),
		"create_scenario": createScenario,
	}
	if spec != nil {
		body["input"] = spec
	}

	var resp []byte
	var status int
	var err error
	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, status, err = c.post(context.Background(), "/v1/incarnations", body)
		if err != nil {
			t.Fatalf("CreateIncarnationWithApplyScenario %s: http: %v", name, err)
		}
		if status == http.StatusAccepted {
			break
		}
		if status == http.StatusUnprocessableEntity &&
			strings.Contains(string(resp), "is not registered") &&
			time.Now().Before(deadline) {
			time.Sleep(250 * time.Millisecond)
			continue
		}
		t.Fatalf("CreateIncarnationWithApplyScenario %s: status %d, body=%s", name, status, string(resp))
	}
	var out struct {
		ApplyID     string `json:"apply_id"`
		Incarnation string `json:"incarnation"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		t.Fatalf("CreateIncarnationWithApplyScenario %s: decode: %v (body=%s)", name, err, string(resp))
	}
	if out.ApplyID == "" {
		t.Fatalf("CreateIncarnationWithApplyScenario %s: пустой apply_id (create-scenario %q не запущен?) body=%s", name, createScenario, string(resp))
	}
	return out.Incarnation, out.ApplyID
}

// RunScenario запускает scenario на существующей incarnation.
//
// 202 → возвращает apply_id. Другая статус-страница — t.Fatal.
func (s *Stack) RunScenario(t *testing.T, incarnationName string, scenarioName string, input map[string]any) string {
	t.Helper()
	c := s.opClient(t)
	body := map[string]any{}
	if input != nil {
		body["input"] = input
	}
	path := fmt.Sprintf("/v1/incarnations/%s/scenarios/%s", incarnationName, scenarioName)
	resp, status, err := c.post(context.Background(), path, body)
	if err != nil {
		t.Fatalf("RunScenario %s/%s: http: %v", incarnationName, scenarioName, err)
	}
	if status != http.StatusAccepted {
		t.Fatalf("RunScenario %s/%s: status %d, body=%s", incarnationName, scenarioName, status, string(resp))
	}
	var out struct {
		ApplyID string `json:"apply_id"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		t.Fatalf("RunScenario %s/%s: decode: %v (body=%s)", incarnationName, scenarioName, err, string(resp))
	}
	if out.ApplyID == "" {
		t.Fatalf("RunScenario %s/%s: empty apply_id in 202 body=%s", incarnationName, scenarioName, string(resp))
	}
	return out.ApplyID
}

// WaitApplySuccess блокируется до УСПЕШНОГО терминала прогона: все строки
// apply_runs (PK = apply_id+sid, N строк на прогон) в success И apply-брекет
// incarnation.applying_apply_id снят с этого applyID.
//
// Брекет обязателен (NIM-46): keeper-строка (sid="keeper") завершается success
// СТРОГО ДО планирования soul-строк, поэтому «все видимые строки success» без
// снятого брекета — ложное «готово» (NIM-45-гонка). Решение — [applySettled].
//
// Терминальный ≠ success — немедленный t.Fatal с дампом матрицы статусов.
func (s *Stack) WaitApplySuccess(t *testing.T, applyID string, timeoutSec int) {
	t.Helper()
	const q = `
SELECT ar.sid, ar.status, i.applying_apply_id
FROM apply_runs ar
LEFT JOIN incarnation i ON i.name = ar.incarnation_name
WHERE ar.apply_id = $1`
	deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)
	var lastSnap []ApplyRunRow
	var lastInFlight bool
	for time.Now().Before(deadline) {
		rows, err := s.db.Query(context.Background(), q, applyID)
		if err != nil {
			t.Fatalf("WaitApplySuccess %s: query: %v", applyID, err)
		}
		var snap []ApplyRunRow
		inFlight := false
		for rows.Next() {
			var sid, st string
			var applyingID *string
			if err := rows.Scan(&sid, &st, &applyingID); err != nil {
				rows.Close()
				t.Fatalf("WaitApplySuccess %s: scan: %v", applyID, err)
			}
			snap = append(snap, ApplyRunRow{SID: sid, Status: st})
			if applyingID != nil && *applyingID == applyID {
				inFlight = true
			}
		}
		rows.Close()
		lastSnap, lastInFlight = snap, inFlight
		done, failSID, failStatus := applySettled(snap, inFlight)
		if failSID != "" {
			t.Fatalf("WaitApplySuccess %s: sid=%s reached terminal %q (строки=%v)", applyID, failSID, failStatus, snap)
		}
		if done {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("WaitApplySuccess %s: success не достигнут за %ds (applyInFlight=%v, строки=%v)", applyID, timeoutSec, lastInFlight, lastSnap)
}

// WaitIncarnationReady блокируется до перехода incarnation.status в `ready`.
//
// Зачем отдельно от WaitApplySuccess: `apply_runs.status=success` (per-host
// терминал задач) выставляется РАНЬШЕ, чем коммит state_changes в
// incarnation.state. commitSuccess (run.go §8) пишет state + status='ready'
// одной PG-транзакцией ПОСЛЕ барьера всех хостов — т.е. между «apply_runs
// success» и «state закоммичен» есть окно. На L3a (soul-stub отвечает мгновенно)
// окно микроскопическое; на L3b (реальный soul + gRPC round-trip) тест успевает
// прочитать incarnation.state как пустой `{}` → AssertIncarnationState флапает.
// Ждём именно status='ready' — это единственная точка, гарантирующая, что
// state_changes уже в БД.
//
// Терминальный ≠ ready (error_locked / migration_failed / destroy_failed) —
// немедленный t.Fatal с текущим статусом.
func (s *Stack) WaitIncarnationReady(t *testing.T, incarnationName string, timeoutSec int) {
	t.Helper()
	deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)
	var last string
	for time.Now().Before(deadline) {
		var status string
		err := s.db.QueryRow(context.Background(),
			"SELECT status FROM incarnation WHERE name = $1", incarnationName).Scan(&status)
		if err != nil {
			t.Fatalf("WaitIncarnationReady %s: query: %v", incarnationName, err)
		}
		last = status
		switch status {
		case "ready":
			return
		case "error_locked", "migration_failed", "destroy_failed", "destroyed":
			t.Fatalf("WaitIncarnationReady %s: достигнут терминальный статус %q вместо ready", incarnationName, status)
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("WaitIncarnationReady %s: status=ready не достигнут за %ds (последний статус=%q)",
		incarnationName, timeoutSec, last)
}

// WaitIncarnationStatus блокируется до перехода incarnation.status в wantStatus.
//
// Зеркало WaitIncarnationReady для НЕ-ready-исходов (split-brain guard, failed_when
// fail-stop): прогон, который ДОЛЖЕН упасть, оставляет incarnation в
// `error_locked` (run.go §7 — state_changes не коммитятся при terminal-failed
// барьере).
//
// ★ Гонка с seeded-ready. SeedIncarnationReady кладёт incarnation сразу в `ready`;
// RunScenario возвращает apply_id асинхронно, ПЕРЕД тем как lockRun переведёт
// `ready → applying → (terminal)`. Наивный поллер ловил начальный `ready` и
// принимал его за «достигнут не тот терминал». Поэтому ждём в два этапа:
// сначала наблюдаем `applying` (прогон стартовал и снял начальный статус), и
// только ПОСЛЕ этого терминал ≠ wantStatus трактуем как регресс flow-control.
// Если wantStatus == applying — первый этап и есть результат.
func (s *Stack) WaitIncarnationStatus(t *testing.T, incarnationName, wantStatus string, timeoutSec int) {
	t.Helper()
	terminal := map[string]bool{
		"ready": true, "error_locked": true, "migration_failed": true,
		"destroy_failed": true, "destroyed": true,
	}
	deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)
	started := false // прогон снял начальный статус (наблюдён applying или сразу терминал)
	var last string
	for time.Now().Before(deadline) {
		var status string
		err := s.db.QueryRow(context.Background(),
			"SELECT status FROM incarnation WHERE name = $1", incarnationName).Scan(&status)
		if err != nil {
			t.Fatalf("WaitIncarnationStatus %s: query: %v", incarnationName, err)
		}
		last = status
		if status == wantStatus {
			return
		}
		if status == "applying" {
			started = true
		}
		// Терминал-мисматч считаем регрессом ТОЛЬКО после старта прогона: до старта
		// это ещё seeded-исходный статус (обычно ready), а не исход прогона.
		if started && terminal[status] {
			t.Fatalf("WaitIncarnationStatus %s: достигнут терминальный %q, ожидался %q (flow-control исход разошёлся)",
				incarnationName, status, wantStatus)
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("WaitIncarnationStatus %s: status=%q не достигнут за %ds (последний=%q)",
		incarnationName, wantStatus, timeoutSec, last)
}

// stripServiceRef отрезает `@<ref>` (если есть). Operator API создаёт
// incarnation по bare service-name (ADR-029).
func stripServiceRef(ref string) string {
	if i := strings.IndexByte(ref, '@'); i >= 0 {
		return ref[:i]
	}
	return ref
}

// DB возвращает pool для теста (read-only для assert-ов). Не Close-ит caller-у:
// pool управляется Cleanup-ом.
func (s *Stack) DB() *pgxpool.Pool {
	return s.db
}
