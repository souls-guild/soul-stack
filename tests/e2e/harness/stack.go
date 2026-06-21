//go:build e2e

// Package harness — reusable test-helpers для L3a E2E-тестирования (ADR-039).
//
// Stack — единица изоляции теста: один тест = один Stack = свой PG / Redis /
// Vault через testcontainers + свой Keeper-процесс (sub-process реального
// бинаря) + N soul-stub-ов, открывающих bidi-стрим к Keeper-у. NewStack
// блокируется до полной готовности инфры (PG healthy + keeper run отвечает
// на /readyz + все soul-stub-ы зарегистрированы).
//
// Архитектурные инварианты (см. ADR-039 Amendment 2026-05-26):
//   - harness НЕ импортирует `keeper/internal/*` (Go internal-rules);
//   - все DB-операции — direct SQL через pgx;
//   - все Vault-операции — direct HTTP API (см. vault.go);
//   - Keeper-процесс — sub-process реального бинаря, не in-process импорт.
package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Config — параметры конструирования Stack-а.
//
// ExamplePath — относительный путь к каталогу service-а в репо (например
// "examples/service/smoke-nginx"). Harness читает его, делает snapshot и
// заводит per-test bare git-repo в $TMP (см. git.go).
//
// Souls — количество soul-stub-ов, открывающих стрим к Keeper-у. На каждый
// stub harness генерирует свой SID (например "soul-test-0.example.com") и
// minimal soulprint, если в fixtures/souls.yaml не задано иное.
type Config struct {
	ExamplePath string
	Souls       int
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
	// MetricsURL — Prometheus-эндпоинт keeper-а (отдельный listener,
	// ADR-024). Используется AssertMetricGE.
	MetricsURL string

	// JWT — credential первого Архонта, прочитанный из credential-файла
	// `keeper init --credential-out=...`.
	JWT string

	// Internal state.
	vaultToken string
	tmpDir     string

	db *pgxpool.Pool

	keeperCmd *exec.Cmd

	// keepers — keeper-субпроцессы мульти-кластера (NewMultiKeeperStack).
	// Пуст для single-keeper Stack (keeperCmd выше). См. multikeeper.go.
	keepers []*keeperProc

	// souls — pre-auth-зарегистрированные soul-stub-ы (SID + mTLS client-cert),
	// заполняется NewStack-ом. caBundle — root CA keeper-server-cert-а, общий
	// для всех (ConnectSoulStub верифицирует server-cert по нему). Используется
	// ConnectSoulStub для открытия live EventStream-стрима к Keeper-у.
	souls    []soulIdentity
	caBundle []byte

	containers []testcontainers.Container

	// Cleanup-shutdown order: LIFO через cleanups (как defer-ы); NewStack
	// аккумулирует teardown-handler-ы по мере поднятия зависимостей, Cleanup
	// сливает в обратном порядке.
	cleanups []func()
}

// NewStack поднимает изолированный стенд и блокируется до готовности.
//
// Pilot-фаза (до v3): t.Skip без spawn-а. Сейчас (v3) — реальный spawn инфры.
//
// Pre-flight: harness требует наличия keeper-бинаря (env `KEEPER_BIN` или
// дефолтная сборка `make build`); без него тест Skip-ается ДО spawn-а
// testcontainers (иначе разработчик без сборки получает 5-минутный таймаут).
// Симметрично — без docker testcontainers вернёт ошибку spawn-а, и тест
// зафейлится явно: разработчик явно запросил E2E, отсутствие docker — fail,
// не skip.
func NewStack(t *testing.T, cfg Config) *Stack {
	t.Helper()
	if cfg.Souls <= 0 {
		cfg.Souls = 1
	}

	// Pre-flight: keeper-бинарь. Skip — фактически «E2E невозможен в этой среде».
	if _, err := locateKeeperBinary(); err != nil {
		t.Skipf("L3a: keeper-бинарь не найден (%v); экспортируй KEEPER_BIN или сделай `make build`", err)
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

	// Outgoing-TLS material для keeper-server listener-ов.
	keeperCertPEM, keeperKeyPEM, caPEM := IssueKeeperServerCert(t, s)
	// Сохраняем CA для ConnectSoulStub (верификация server-cert-а soul-stub-ом).
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

	// Pre-auth регистрация soul-stub-ов в БД. Сохраняем mTLS client-cert каждого
	// SID-а — ConnectSoulStub откроет по нему live EventStream-стрим к Keeper-у
	// (нужно для dispatch-маршрутизации: Errand/Apply уходят в локальный Outbound
	// только при наличии живого стрима + захваченного Redis SID-lease).
	for i := 0; i < cfg.Souls; i++ {
		sid := fmt.Sprintf("soul-test-%d.example.com", i)
		cert, key := RegisterSoulPreAuth(t, s, sid)
		s.souls = append(s.souls, soulIdentity{SID: sid, Cert: cert, Key: key})
	}

	return s
}

// soulIdentity — pre-auth-зарегистрированный soul-stub: SID + mTLS client-cert.
type soulIdentity struct {
	SID  string
	Cert []byte
	Key  []byte
}

// SoulSID возвращает SID i-го pre-auth soul-а (0-based). Fatal при выходе за
// границы (тест запросил больше Soul-ов, чем создал Config.Souls).
func (s *Stack) SoulSID(i int) string {
	if i < 0 || i >= len(s.souls) {
		s.t.Fatalf("SoulSID(%d): out of range (создано %d soul-ов)", i, len(s.souls))
	}
	return s.souls[i].SID
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
		// vault dev-mode требует IPC_LOCK / cap_add, иначе ругается warning-ом,
		// но стартует. В test-окружении игнорируем.
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
	// дефолт которого `/var/lib/soul-stack-keeper/...` (не writable в test-env).
	// Перенаправляем в tmpDir через env-override (KEEPER_SERVICE_CACHE_DIR /
	// KEEPER_DESTINY_CACHE_DIR / KEEPER_PLUGIN_WORK_DIR — см. cmd/keeper/main.go).
	// Без этого incarnation-create падает 500 «mkdir /var/lib/...: permission
	// denied» на материализации service-снапшота из file://-репо.
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
	// tests/e2e/<test>.go → repo-root = wd/../..
	repoRoot := filepath.Clean(filepath.Join(wd, "..", ".."))
	candidate := filepath.Join(repoRoot, "keeper", "bin", "keeper")
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

// SeedIncarnationReady вставляет incarnation-строку напрямую в Postgres со
// status=ready и заданным baseline state, минуя scenario `create`.
//
// Нужно для e2e мутирующих сценариев сервисов, у которых `create` недоступен в
// L3a-фикстуре (cloud-spawn / declared-role / probe на ещё-не-запущенном хосте —
// напр. redis-cluster): такой сценарий требует предсуществующей ready-incarnation,
// но прогнать её create нельзя. Прямой seed даёт нужную точку входа.
//
// serviceVersion — git-ref сервиса (обычно "main"); state — baseline
// incarnation.state (JSONB). covens НЕ выставляются (declared env-теги не нужны:
// roster резолвится по `incarnation.name ∈ souls.coven[]`, см. AddSoulToCoven).
// created_by_aid = NULL (seed без оператора; FK ON DELETE SET NULL это допускает).
// state_schema_version по умолчанию из DDL (DEFAULT 1) не задаём явно — мутирующий
// сценарий читает state по полям, не по версии.
func (s *Stack) SeedIncarnationReady(t *testing.T, name, service, serviceVersion string, state map[string]any) {
	t.Helper()
	stateJSON, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("SeedIncarnationReady(%s): marshal state: %v", name, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := s.db.Exec(ctx, `
		INSERT INTO incarnation (name, service, service_version, spec, state, status)
		VALUES ($1, $2, $3, '{}'::jsonb, $4::jsonb, 'ready')
	`, name, service, serviceVersion, string(stateJSON)); err != nil {
		t.Fatalf("SeedIncarnationReady(%s): %v", name, err)
	}
}

// CreateIncarnation создаёт incarnation через Operator API Keeper-а.
//
// serviceRef — `<service>@<ref>` по контракту ТЗ; harness отрезает суффикс
// `@<ref>` (POST /v1/incarnations принимает только bare service-name, версия
// разрешается через service registry, ADR-029). spec — `input` body request-а.
//
// 202 → возвращает имя incarnation. Любая другая статус-страница — t.Fatal с
// телом ответа (диагностика 4xx без догадок).
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

	// service-registry propagation: RegisterService коммитит запись в БД и
	// PUBLISH-ит `service:invalidate`, но serviceregistry.Holder обновляет снимок
	// в фоновой goroutine (pub/sub near-instant + 10s TTL-fallback). Между 201
	// RegisterService и тёплым снимком есть короткое окно, в котором
	// incarnation-create видит «service is not registered». Поллим ТОЛЬКО этот
	// транзиентный 422 (по detail-маркеру «not registered»); любой другой статус
	// или 422 иной природы (required-input) — немедленный fatal, без маскировки.
	var resp []byte
	var status int
	var err error
	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, status, err = c.post(context.Background(), "/v1/incarnations", body)
		if err != nil {
			t.Fatalf("CreateIncarnation %s: http: %v", name, err)
		}
		if status == http.StatusUnprocessableEntity &&
			strings.Contains(string(resp), "not registered") &&
			time.Now().Before(deadline) {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		break
	}
	if status != http.StatusAccepted {
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
// авто-запущенного scenario `create` (incarnation.go запускает его сразу,
// переводя incarnation в `applying`). Использовать вместо отдельного
// RunScenario(create) сразу после Create: второй параллельный create-прогон
// отвергается («incarnation уже в статусе applying»), и ожидание его apply_id
// зависнет. Возвращает (incarnationName, applyID).
func (s *Stack) CreateIncarnationWithApply(t *testing.T, name, serviceRef string, spec map[string]any) (string, string) {
	t.Helper()
	c := s.opClient(t)
	body := map[string]any{
		"name":    name,
		"service": stripServiceRef(serviceRef),
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
		if status == http.StatusUnprocessableEntity &&
			strings.Contains(string(resp), "not registered") &&
			time.Now().Before(deadline) {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		break
	}
	if status != http.StatusAccepted {
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

// CreateIncarnationRaw — низкоуровневый POST /v1/incarnations: возвращает
// (responseBody, statusCode) без проверки статуса. Для негативных тестов
// (например 422 sync-валидации required-input — фикс 6ce69ce), где сам код
// ответа — предмет ассерта. Happy-path используйте CreateIncarnation.
func (s *Stack) CreateIncarnationRaw(t *testing.T, name, serviceRef string, spec map[string]any) ([]byte, int) {
	t.Helper()
	c := s.opClient(t)
	body := map[string]any{
		"name":    name,
		"service": stripServiceRef(serviceRef),
	}
	if spec != nil {
		body["input"] = spec
	}
	resp, status, err := c.post(context.Background(), "/v1/incarnations", body)
	if err != nil {
		t.Fatalf("CreateIncarnationRaw %s: http: %v", name, err)
	}
	return resp, status
}

// RunScenario запускает scenario на существующей incarnation.
//
// 202 → возвращает apply_id из тела ответа. Другая статус-страница — t.Fatal.
func (s *Stack) RunScenario(t *testing.T, incarnationName string, scenarioName string, input map[string]any) string {
	t.Helper()
	c := s.opClient(t)
	body := map[string]any{}
	if input != nil {
		body["input"] = input
	}
	path := fmt.Sprintf("/v1/incarnations/%s/scenarios/%s", incarnationName, scenarioName)
	// Тот же транзиентный 422 «service ... not registered», что в CreateIncarnation:
	// serviceregistry.Holder подтягивает снимок асинхронно (pub/sub + 10s TTL).
	// Прямой seed incarnation (SeedIncarnationReady) минует CreateIncarnation-поллинг,
	// поэтому первый RunScenario может попасть в холодное окно снимка. Поллим ТОЛЬКО
	// этот маркер; любой иной 422 (input/required) — немедленный fatal без маскировки.
	var resp []byte
	var status int
	var err error
	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, status, err = c.post(context.Background(), path, body)
		if err != nil {
			t.Fatalf("RunScenario %s/%s: http: %v", incarnationName, scenarioName, err)
		}
		if status == http.StatusUnprocessableEntity &&
			strings.Contains(string(resp), "not registered") &&
			time.Now().Before(deadline) {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		break
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

// WaitApplySuccess блокируется до перехода apply_runs.status в success у всех
// строк прогона. PK apply_runs = (apply_id, sid) → один прогон даёт N строк
// (по числу Soul-хостов). Условие success: все строки в success; любая в
// failed/cancelled/orphaned/no_match — fatal до достижения success.
//
// pre-running статусы (planned/claimed/dispatched/running) считаются
// «в работе», ожидание продолжается. Терминальные ≠ success — немедленный
// t.Fatal с дампом матрицы статусов (без надежды, что «само пройдёт»).
func (s *Stack) WaitApplySuccess(t *testing.T, applyID string, timeoutSec int) {
	t.Helper()
	deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)
	for time.Now().Before(deadline) {
		rows, err := s.db.Query(context.Background(),
			"SELECT sid, status FROM apply_runs WHERE apply_id = $1", applyID)
		if err != nil {
			t.Fatalf("WaitApplySuccess %s: query: %v", applyID, err)
		}
		statuses := map[string]string{}
		for rows.Next() {
			var sid, st string
			if err := rows.Scan(&sid, &st); err != nil {
				rows.Close()
				t.Fatalf("WaitApplySuccess %s: scan: %v", applyID, err)
			}
			statuses[sid] = st
		}
		rows.Close()
		if len(statuses) == 0 {
			time.Sleep(250 * time.Millisecond)
			continue
		}
		allSuccess := true
		for sid, st := range statuses {
			switch st {
			case "success":
				continue
			case "failed", "cancelled", "orphaned", "no_match":
				t.Fatalf("WaitApplySuccess %s: sid=%s reached terminal %q (статусы=%v)", applyID, sid, st, statuses)
			default:
				allSuccess = false
			}
		}
		if allSuccess {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("WaitApplySuccess %s: success не достигнут за %ds", applyID, timeoutSec)
}

// WaitIncarnationReady блокируется до перехода incarnation.status в `ready`.
//
// Зачем отдельно от WaitApplySuccess: apply_runs.status=success (per-host барьер
// задач) выставляется РАНЬШЕ, чем коммит state_changes в incarnation.state —
// commitSuccess (run.go §8) пишет state+status='ready' одной PG-транзакцией ПОСЛЕ
// барьера всех хостов. На smoke-nginx (2 задачи) окно микроскопическое и
// AssertIncarnationState сразу после WaitApplySuccess проходит; на сервисе с
// десятками задач (redis::create — 3 destiny) окно шире, и чтение state ловит
// пустой `{}`. Ждём именно status='ready' — единственная точка, гарантирующая,
// что state_changes уже в БД. Параллель с L3b-harness (tests/e2e-live).
//
// Терминальный ≠ ready (error_locked / migration_failed / destroyed) —
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

// stripServiceRef отрезает `@<ref>` (если есть). Operator API создаёт
// incarnation по bare service-name; ref разрешается через service registry
// (ADR-029). ТЗ harness-у даёт `smoke-nginx@main` — для совместимости с
// `examples/service/<name>` (имя пакета совпадает со «service-name»).
func stripServiceRef(ref string) string {
	if i := strings.IndexByte(ref, '@'); i >= 0 {
		return ref[:i]
	}
	return ref
}

// DB возвращает pool для теста (read-only для assert-ов). Не Closeit-ить
// caller-у: pool управляется Cleanup-ом.
func (s *Stack) DB() *pgxpool.Pool {
	return s.db
}
