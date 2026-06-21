//go:build e2e_live

package harness

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	dockercontainer "github.com/moby/moby/api/types/container"
	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	tcnetwork "github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
)

// SoulContainer — обёртка над testcontainers.Container для real-soul instance.
//
// SpawnSoulContainer заполняет SID/BootstrapToken/Container и регистрирует
// контейнер в Stack.SoulContainers + LIFO-cleanup. Дальше Exec используется
// для container-side asserts (L3b-4, заглушки в asserts.go).
type SoulContainer struct {
	// SID — FQDN-имя Soul-а (например `soul-live-a.example.com`). Echo в
	// gRPC payload; авторитет — mTLS peer cert.
	SID string

	// Container — handle на testcontainers.Container. Используется для Exec
	// (container-side asserts L3b-4) и Terminate (через Stack.Cleanup).
	Container testcontainers.Container

	// BootstrapToken — plain SoulSeed-токен, выданный harness-ом до spawn-а.
	// Передаётся в soul.yml внутри контейнера; soul-агент при первом старте
	// делает CSR через Keeper.Bootstrap RPC (mTLS server-only).
	BootstrapToken string
}

// Exec выполняет команду внутри soul-контейнера. Используется container-side
// asserts (AssertHostPkgInstalled / AssertHostServiceActive / ...) — L3b-4.
//
// Возвращает (stdout+stderr, exitCode, err). tcexec.Multiplexed демультиплексирует
// docker-stream (8-байтные frame-header-ы) в чистый текст — без него reader
// содержит сырые header-байты (`\x01\x00…\x07active`), и assert-ы с точным
// сравнением stdout (например AssertHostServiceActive: `== "active"`) ложно
// падают. stdout и stderr склеиваются в один поток (caller-у достаточно exit-
// кода + текста для diag).
func (sc *SoulContainer) Exec(ctx context.Context, cmd []string) (combined string, exitCode int, err error) {
	if sc == nil || sc.Container == nil {
		return "", -1, errors.New("SoulContainer.Exec: nil container")
	}
	code, reader, err := sc.Container.Exec(ctx, cmd, tcexec.Multiplexed())
	if err != nil {
		return "", code, fmt.Errorf("exec %v: %w", cmd, err)
	}
	body, readErr := io.ReadAll(reader)
	if readErr != nil {
		return string(body), code, fmt.Errorf("exec %v: read output: %w", cmd, readErr)
	}
	return string(body), code, nil
}

// soulStartupTimeout — окно от spawn-а контейнера до souls.status='connected'.
// docker build (~60s холодного билда) + systemd-PID-1 boot (~3-10s) + soul init
// (CSR/Vault round-trip ~1s) + soul run dial (~1s) + first connect commit ~ 90s
// верхний потолок; обычно 30-40s.
const soulStartupTimeout = 120 * time.Second

// SpawnSoulContainer поднимает один real-soul container (Debian-12 systemd-PID-1),
// mount-ит soul-binary с хоста, кладёт soul.yml + CA-bundle, выполняет
// `soul init` (CSR Bootstrap-flow → leaf-cert), стартует `soul run` в фоне и
// ждёт регистрации в keeper-е (souls.status='connected').
//
// Параметры:
//   - sid — FQDN, должен матчить CN cert-а;
//   - bootstrapToken — plain SoulSeed-токен (issued IssueBootstrapToken-ом до spawn-а).
//
// Side effects:
//   - первая инвокация создаёт docker user-bridge `soul-stack-e2e-live-*`
//     (используется для inter-soul-связи в multi-host L3b-5; в одиночных L3b-2
//     сценариях достаточно host.docker.internal до keeper-а);
//   - контейнер регистрируется в Stack.cleanups (LIFO), Terminate вызывается
//     в Stack.Cleanup до Postgres-tearown-а.
func SpawnSoulContainer(t *testing.T, stack *Stack, sid, bootstrapToken string) *SoulContainer {
	t.Helper()
	if stack == nil {
		t.Fatal("SpawnSoulContainer: stack is nil")
	}

	ctx, cancel := context.WithTimeout(context.Background(), soulStartupTimeout)
	defer cancel()

	// 1. Pre-flight: soul-linux binary должен быть собран (`make build-linux`).
	soulBinPath, err := locateLinuxSoulBinary()
	if err != nil {
		t.Fatalf("SpawnSoulContainer: %v", err)
	}

	// 2. Lazy-create общий user-bridge для всех soul-контейнеров этого Stack-а.
	if stack.dockerNetwork == nil {
		nw, err := tcnetwork.New(ctx)
		if err != nil {
			t.Fatalf("SpawnSoulContainer: create network: %v", err)
		}
		stack.dockerNetwork = nw
		stack.cleanups = append(stack.cleanups, func() {
			toCtx, toCancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer toCancel()
			_ = nw.Remove(toCtx)
		})
	}

	// 3. Раскладка bind-mount-ов на хосте: soul-binary + CA + soul.yml.
	mountRoot := filepath.Join(stack.tmpDir, "soul-"+sanitizeSID(sid))
	if err := os.MkdirAll(mountRoot, 0o755); err != nil {
		t.Fatalf("SpawnSoulContainer: mkdir mountRoot: %v", err)
	}
	caPath := filepath.Join(mountRoot, "ca.pem")
	if err := os.WriteFile(caPath, stack.caBundle, 0o644); err != nil {
		t.Fatalf("SpawnSoulContainer: write ca: %v", err)
	}
	soulYAMLPath := filepath.Join(mountRoot, "soul.yml")
	if err := os.WriteFile(soulYAMLPath, []byte(buildSoulYAML(stack)), 0o644); err != nil {
		t.Fatalf("SpawnSoulContainer: write soul.yml: %v", err)
	}

	// 4. ContainerRequest: privileged systemd-PID-1, /sys/fs/cgroup из хоста,
	//    soul-binary read-only mount, soul.yml + CA через /etc/soul/.
	dockerfilePath, err := findDockerfile(t)
	if err != nil {
		t.Fatalf("SpawnSoulContainer: %v", err)
	}
	req := testcontainers.ContainerRequest{
		FromDockerfile: testcontainers.FromDockerfile{
			Context:       filepath.Dir(dockerfilePath),
			Dockerfile:    filepath.Base(dockerfilePath),
			PrintBuildLog: false,
			KeepImage:     true, // одинаковый Dockerfile для всех L3b-тестов — переиспользуем слои.
		},
		Name:       fmt.Sprintf("soul-live-%s-%d", sanitizeSID(sid), time.Now().UnixNano()),
		Hostname:   sid,
		ExtraHosts: keeperExtraHosts(),
		Networks:   []string{stack.dockerNetwork.Name},
		Files: []testcontainers.ContainerFile{
			{HostFilePath: soulBinPath, ContainerFilePath: "/usr/local/bin/soul", FileMode: 0o755},
			{HostFilePath: caPath, ContainerFilePath: "/etc/soul/ca.pem", FileMode: 0o644},
			{HostFilePath: soulYAMLPath, ContainerFilePath: "/etc/soul/soul.yml", FileMode: 0o644},
		},
		HostConfigModifier: func(hc *dockercontainer.HostConfig) {
			hc.Privileged = true
			// systemd-PID-1 требует tmpfs /run + /run/lock; CgroupnsMode=host —
			// чтобы systemd видел cgroup-fs хоста (необходимо для systemctl).
			hc.CgroupnsMode = "host"
			if hc.Tmpfs == nil {
				hc.Tmpfs = map[string]string{}
			}
			hc.Tmpfs["/run"] = "rw"
			hc.Tmpfs["/run/lock"] = "rw"
		},
		// WaitingFor: systemd-готовность — пишется в stdout при boot-е PID-1.
		// "Started" подходит для большинства unit-ов; нам важно дождаться
		// именно того, что systemd принимает команды (потом сами вызываем
		// Exec для soul init/run, см. ниже).
		WaitingFor: wait.ForExec([]string{"systemctl", "is-system-running", "--wait"}).
			WithExitCodeMatcher(func(code int) bool {
				// is-system-running возвращает 0 при `running`, 1 при `degraded`
				// (нам ок: degraded в slim-Debian без unit-ов нормально), 2 при
				// `initializing` (ещё ждём). Принимаем 0 и 1.
				return code == 0 || code == 1
			}).
			WithStartupTimeout(60 * time.Second),
	}

	cont, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("SpawnSoulContainer: generic container: %v", err)
	}
	stack.containers = append(stack.containers, cont)
	stack.cleanups = append(stack.cleanups, func() {
		toCtx, toCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer toCancel()
		_ = cont.Terminate(toCtx)
	})

	sc := &SoulContainer{
		SID:            sid,
		Container:      cont,
		BootstrapToken: bootstrapToken,
	}

	// 5. soul init — реальный CSR Bootstrap-flow.
	initOut, initCode, err := sc.Exec(ctx, []string{
		"/usr/local/bin/soul", "init",
		"--config", "/etc/soul/soul.yml",
		"--token", bootstrapToken,
		"--sid", sid,
	})
	if err != nil || initCode != 0 {
		t.Fatalf("SpawnSoulContainer: soul init: code=%d err=%v output=%s", initCode, err, initOut)
	}

	// 6. soul run — фоновый daemon. testcontainers Exec не поддерживает detach,
	//    поэтому запускаем через nohup внутри shell-а; stdout/stderr уходят в
	//    /var/log/soul.log для последующего разбора при фейле connect-а.
	runOut, runCode, err := sc.Exec(ctx, []string{
		"/bin/sh", "-c",
		"nohup /usr/local/bin/soul run --config /etc/soul/soul.yml " +
			">/var/log/soul.log 2>&1 </dev/null &",
	})
	if err != nil || runCode != 0 {
		t.Fatalf("SpawnSoulContainer: soul run launch: code=%d err=%v output=%s", runCode, err, runOut)
	}

	// 7. Wait souls.status='connected'.
	if err := waitForSoulConnected(ctx, stack, sid, 60*time.Second); err != nil {
		// Дамп /var/log/soul.log в test-лог для диагностики.
		dump, _, _ := sc.Exec(context.Background(),
			[]string{"/bin/sh", "-c", "cat /var/log/soul.log 2>/dev/null | tail -n 100"})
		t.Fatalf("SpawnSoulContainer: %v\nsoul.log tail:\n%s", err, dump)
	}

	return sc
}

// waitForSoulConnected поллит `souls.status` для sid, возвращает nil при
// первом 'connected'. Терминальные статусы (revoked/expired/destroyed) →
// немедленный fail, не ждём timeout.
func waitForSoulConnected(ctx context.Context, stack *Stack, sid string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		var status string
		err := stack.db.QueryRow(ctx,
			"SELECT status FROM souls WHERE sid = $1", sid).Scan(&status)
		if err != nil {
			return fmt.Errorf("query souls(%s): %w", sid, err)
		}
		switch status {
		case "connected":
			return nil
		case "revoked", "expired", "destroyed":
			return fmt.Errorf("soul %s reached terminal status %q", sid, status)
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("soul %s did not reach status=connected within %v", sid, timeout)
}

// defaultKeeperHost — host, на который соул-контейнер дозванивается к keeper-у
// по умолчанию. На native-Linux-CI резолвится через ExtraHosts host-gateway
// (см. SpawnSoulContainer); это рабочий дефолт, ломать его нельзя.
const defaultKeeperHost = "host.docker.internal"

// keeperEndpointHostEnv — env-override host-а keeper-эндпоинта для соул-контейнера.
//
// Зачем: на WSL2 + Docker-Desktop контейнеры живут в DD-VM, а keeper-процесс —
// в WSL2-дистре (разные network-namespace). Из контейнера `host.docker.internal`
// резолвится в DD-VM-шлюз (192.168.65.254), где keeper НЕ слушает → bootstrap
// падает на `connection refused`. Реальный WSL2-хост-IP (`hostname -I` первый,
// напр. 172.27.x.x) из контейнера достижим. Override прописывает этот IP в
// soul.yml::keeper.endpoints[].host + добавляет его же в TLS-SAN keeper-серта.
//
// Если env не задан — дефолт host.docker.internal (native-Linux-CI не ломается).
// Гонять на WSL2: `E2E_KEEPER_HOST=$(hostname -I | awk '{print $1}') go test ...`.
const keeperEndpointHostEnv = "E2E_KEEPER_HOST"

// keeperEndpointHost возвращает host, на который соул-контейнер дозванивается к
// keeper-у: значение env E2E_KEEPER_HOST либо дефолт host.docker.internal.
func keeperEndpointHost() string {
	if v := strings.TrimSpace(os.Getenv(keeperEndpointHostEnv)); v != "" {
		return v
	}
	return defaultKeeperHost
}

// keeperExtraHosts возвращает ExtraHosts-маппинг для соул-контейнера.
//
// Дефолтный `host.docker.internal:host-gateway` держим всегда — на native-Linux
// docker-desktop-alias штатно не настроен, и keeper-эндпоинт по умолчанию его и
// использует. При override именем (не IP) добавляем `<host>:host-gateway`, чтобы
// имя резолвилось в шлюз. IP-override (WSL2-кейс) в ExtraHosts не нуждается —
// контейнер маршрутизирует к хост-IP напрямую.
func keeperExtraHosts() []string {
	hosts := []string{defaultKeeperHost + ":host-gateway"}
	if override := strings.TrimSpace(os.Getenv(keeperEndpointHostEnv)); override != "" &&
		override != defaultKeeperHost && net.ParseIP(override) == nil {
		hosts = append(hosts, override+":host-gateway")
	}
	return hosts
}

// buildSoulYAML рендерит soul.yml для запуска внутри контейнера. Все пути —
// container-side; keeper-endpoint — <host>:<port>, где host берётся из
// keeperEndpointHost() (дефолт host.docker.internal, резолвится через
// ExtraHosts host-gateway; на WSL2 — реальный хост-IP через E2E_KEEPER_HOST).
func buildSoulYAML(stack *Stack) string {
	// metrics.enabled=true → soul поднимает /metrics на loopback 127.0.0.1:9091
	// (default listen). Порт наружу НЕ публикуется (нет ports-mapping) — scrape
	// только container-side через Exec(curl). Нужен FC-3-тесту, который читает
	// soul_apply_task_retries_total; остальным тестам безвреден (loopback-bind).
	const tmpl = `paths:
  seed: /var/lib/soul-stack/seed
  modules: /var/lib/soul-stack/modules
keeper:
  endpoints:
    - host: %s
      bootstrap_port: %d
      event_stream_port: %d
      priority: 1
  tls:
    ca: /etc/soul/ca.pem
logging:
  level: info
  format: text
metrics:
  enabled: true
hot_reload:
  enable_signal: false
  enable_inotify: false
`
	return fmt.Sprintf(tmpl, keeperEndpointHost(), stack.bootstrapPort, stack.eventStreamPort)
}

// findDockerfile возвращает абсолютный путь к L3b-Dockerfile-у. Относительный
// поиск: `tests/e2e-live/dockerfiles/debian-12.Dockerfile` от cwd теста.
func findDockerfile(t *testing.T) (string, error) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("findDockerfile: getwd: %w", err)
	}
	// Walk вверх: тест может лежать в `tests/e2e-live/` или в подпакете.
	dir := wd
	for i := 0; i < 5; i++ {
		candidate := filepath.Join(dir, "dockerfiles", "debian-12.Dockerfile")
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("findDockerfile: debian-12.Dockerfile не найден (wd=%s)", wd)
}

// sanitizeSID превращает FQDN в slug, годный для docker-имени контейнера
// (длина <128, [a-z0-9_.-]).
func sanitizeSID(sid string) string {
	s := strings.ReplaceAll(sid, ".", "-")
	s = strings.ReplaceAll(s, ":", "-")
	if len(s) > 60 {
		s = s[:60]
	}
	return s
}
