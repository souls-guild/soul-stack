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
// (L3b-2+). На L3b-1 параметр принимается, но контейнеры НЕ создаются —
// harness логирует warn-сообщение.
type Config struct {
	ExamplePath string
	ServiceName string
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
	// MetricsURL — Prometheus-эндпоинт keeper-а (отдельный listener, ADR-024).
	MetricsURL string

	// JWT — credential первого Архонта, прочитанный из credential-файла
	// `keeper init --credential-out=...`.
	JWT string

	// SoulContainers — populated SpawnSoulContainer-ом в L3b-2+; nil/empty на L3b-1.
	SoulContainers []*SoulContainer

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
	if cfg.Souls <= 0 {
		cfg.Souls = 1
	}

	// Pre-flight: keeper-бинарь (нативный, host-arch).
	if _, err := locateKeeperBinary(); err != nil {
		t.Skipf("L3b: keeper-бинарь не найден (%v); экспортируй KEEPER_BIN или сделай `make build`", err)
	}
	// Pre-flight: linux-soul-бинарь (для L3b-2+).
	if _, err := locateLinuxSoulBinary(); err != nil {
		t.Skipf("L3b: soul-linux-amd64 не найден (%v); экспортируй SOUL_BIN_LINUX или сделай `make build-linux`", err)
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
	cmd := exec.Command(binaryPath, "run", "--config", keeperYAMLPath)
	cmd.Env = append(os.Environ(), "SOUL_STACK_ALLOW_FILE_REPOS=1")
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
	resp, status, err := c.post(context.Background(), "/v1/incarnations", body)
	if err != nil {
		t.Fatalf("CreateIncarnation %s: http: %v", name, err)
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

// WaitApplySuccess блокируется до перехода apply_runs.status в success у всех
// строк прогона. PK apply_runs = (apply_id, sid) → один прогон даёт N строк.
//
// Терминальный ≠ success — немедленный t.Fatal с дампом матрицы статусов.
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
