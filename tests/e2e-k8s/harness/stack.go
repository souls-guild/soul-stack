//go:build e2e_k8s

package harness

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
)

// Config — параметры стенда L3c. ExamplePath / Souls добавляются в L3c-3 (Soul-
// StatefulSet и реальный service-fixture).
type Config struct {
	// ExamplePath — путь к examples/service/<name>/ (L3c-3 git-loader).
	ExamplePath string

	// ServiceName — bare-name сервиса для CreateIncarnation (L3c-5 redis-cluster
	// resharding и future service-registry pre-seed).
	ServiceName string

	// Souls — количество Soul-StatefulSet replicas (L3c-3).
	Souls int

	// ReaperEnabled — включить Reaper-loop в keeper.yml (L3c-4 failover).
	// Дефолт false: L3c-2/3 background-job не нужен, минимизация
	// side-effects. L3c-4 включает, чтобы проверить leader-election на
	// kill-pod.
	ReaperEnabled bool
}

// Stack — высокоуровневая harness-обёртка по контракту L3a/L3b.
//
// Lifecycle L3c-2:
//
//	NewStack(t, cfg) → DeployInfra(t) → DeployKeeper(t, replicas)
//	  → KeeperReadyzURL() → test asserts → t.Cleanup (LIFO)
//
// L3c-3 добавит DeploySoul и реальный bootstrap-flow.
type Stack struct {
	Cluster   *Cluster
	Clientset *kubernetes.Clientset
	// RESTConfig — для kubectl-exec через client-go (DeploySoul вызывает
	// `kubectl exec`-эквивалент `soul init` + `systemctl start` внутри pod-а).
	RESTConfig *rest.Config

	// reaperEnabled — копия Config.ReaperEnabled. Нужна DeployKeeper-у при
	// рендере keeper.yml. Хранится в Stack, потому что NewStack принимает
	// Config один раз, а DeployKeeper вызывается отдельно.
	reaperEnabled bool

	// CABundle — Vault PKI root CA в PEM (заполняется DeployInfra). Soul-pod
	// получает его как `/etc/soul/ca.pem` (через ConfigMap) для server-only
	// TLS-handshake-а с keeper-ом во время `soul init`.
	CABundle []byte

	// JWT — bootstrap-Archon JWT первого оператора (выпускается через
	// `keeper init` в running keeper-pod-е). Заполняется
	// [Stack.BootstrapArchon]. Пустая строка — bootstrap ещё не выполнен.
	JWT string

	// Заполняются DeployInfra-ом — in-cluster service-имена/адреса инфры.
	// PGServiceDNS — `<host>:<port>` form for keeper.yml::postgres.dsn (через
	// vault-ref).
	PGServiceDNS    string
	RedisServiceDNS string
	VaultServiceDNS string

	// KeeperOpenAPIPort — порт (внутри cluster) `/readyz` endpoint-а keeper-pod-а.
	// Совпадает с manifests/keeper/deployment.yaml::containerPort.
	KeeperOpenAPIPort int

	// vaultPF — port-forward к Vault для harness-side seed (PKI/JWT/DSN).
	// Закрывается в t.Cleanup автоматически.
	vaultPF *PortForward
}

// NewStack создаёт kind-cluster под этот тест и инициализирует client-go.
// Дальнейшие шаги (DeployInfra/DeployKeeper) — отдельными методами.
func NewStack(t *testing.T, cfg Config) *Stack {
	t.Helper()

	cluster := NewCluster(t)

	restCfg, err := clientcmd.BuildConfigFromFlags("", cluster.Kubeconfig)
	if err != nil {
		t.Fatalf("build kubeconfig %q: %v", cluster.Kubeconfig, err)
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		t.Fatalf("client-go NewForConfig: %v", err)
	}

	return &Stack{
		Cluster:       cluster,
		Clientset:     cs,
		RESTConfig:    restCfg,
		reaperEnabled: cfg.ReaperEnabled,
	}
}

// DeployInfra — bitnami Helm install PostgreSQL/Redis/Vault в default-namespace.
// Чарты ждут полной готовности (`--wait`), а Vault дополнительно пингуется
// через port-forward + seed-ится (PKI mount + JWT signing-key + PG DSN + keeper-
// server TLS cert).
//
// После успеха Stack-поля PGServiceDNS / RedisServiceDNS / VaultServiceDNS
// заполнены in-cluster service-DNS-именами. cert/key/ca keeper-server TLS
// возвращаются как PEM-байты — caller (DeployKeeper) кладёт их в Secret.
func (s *Stack) DeployInfra(t *testing.T) (certPEM, keyPEM, caPEM []byte) {
	t.Helper()
	requireHelm(t)
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skipf("L3c: kubectl не найден в PATH: %v", err)
	}

	// 1. Helm repo + чарты.
	s.Cluster.helmRepoEnsure(t)
	repoRoot := repoRootFromTestWD(t)
	valuesDir := filepath.Join(repoRoot, "tests", "e2e-k8s", "helm-values")

	s.Cluster.helmInstall(t, "postgres", "bitnami/postgresql",
		filepath.Join(valuesDir, "postgres.yaml"), helmInstallTimeout)
	s.Cluster.helmInstall(t, "redis", "bitnami/redis",
		filepath.Join(valuesDir, "redis.yaml"), helmRedisTimeout)
	s.Cluster.helmInstall(t, "vault", "bitnami/vault",
		filepath.Join(valuesDir, "vault.yaml"), helmVaultTimeout)

	// 2. Service-DNS in-cluster (bitnami-конвенция `<release>-<chart>`/`<release>-master`).
	// PG DSN сеется в Vault; keeper читает через `dsn_ref: vault:secret/keeper/postgres`.
	s.PGServiceDNS = "postgres-postgresql.default.svc.cluster.local:5432"
	s.RedisServiceDNS = "redis-master.default.svc.cluster.local:6379"
	s.VaultServiceDNS = "vault.default.svc.cluster.local:8200"
	pgDSN := fmt.Sprintf("postgresql://postgres:testpass@%s/keeper_test?sslmode=disable", s.PGServiceDNS)

	// 3. Vault seed через port-forward (host-side доступ к ClusterIP-only сервису).
	pf := s.Cluster.PortForward(t, "svc/vault", 8200, 60*time.Second)
	s.vaultPF = pf
	vaultAddr := fmt.Sprintf("http://127.0.0.1:%d", pf.LocalPort)
	waitVaultReady(t, vaultAddr, 60*time.Second)
	certPEM, keyPEM, caPEM = seedVaultSecrets(t, vaultAddr, pgDSN)
	s.CABundle = caPEM
	return certPEM, keyPEM, caPEM
}

// DeployKeeper разворачивает keeper в kind: TLS-Secret + ConfigMap (рендеренный
// keeper.yml) + Deployment + Service. Грузит локально собранный образ
// `keeper:e2e-k8s` в kind через `kind load docker-image`. Блокируется до Ready
// pods (timeout 5m) и до /readyz=200 через port-forward.
//
// replicas — число реплик Deployment-а. L3c-2 принимает 1; L3c-3 расширит на 3.
//
// Возвращает port-forward на keeper-pod-овский /readyz (8080→localhost:<random>).
// pf.Close() регистрируется в t.Cleanup.
func (s *Stack) DeployKeeper(t *testing.T, replicas int, certPEM, keyPEM, caPEM []byte) *PortForward {
	t.Helper()
	if replicas < 1 {
		t.Fatalf("DeployKeeper: replicas must be >= 1, got %d", replicas)
	}

	// 1. Load image.
	s.Cluster.LoadDockerImage(t, "keeper:e2e-k8s")

	// 2. Render keeper.yml с in-cluster адресами.
	keeperYAML := renderKeeperYAML(keeperYAMLInputs{
		VaultAddr:           "http://" + s.VaultServiceDNS,
		VaultToken:          vaultRootToken,
		RedisAddr:           s.RedisServiceDNS,
		OpenAPIPort:         8080,
		MCPPort:             8090,
		MetricsPort:         9100,
		BootstrapGRPCPort:   9094,
		EventStreamGRPCPort: 9095,
		ReaperEnabled:       s.reaperEnabled,
	})

	// 3. Создаём Secret + ConfigMap через client-go (атомарно, без kubectl).
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tlsSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "keeper-tls", Namespace: "default"},
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"keeper.crt":   certPEM,
			"keeper.key":   keyPEM,
			"vault-ca.crt": caPEM,
		},
	}
	if _, err := s.Clientset.CoreV1().Secrets("default").Create(ctx, tlsSecret, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create secret keeper-tls: %v", err)
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "keeper-config", Namespace: "default"},
		Data: map[string]string{
			"keeper.yml": keeperYAML,
		},
	}
	if _, err := s.Clientset.CoreV1().ConfigMaps("default").Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create configmap keeper-config: %v", err)
	}

	// 4. Apply raw Deployment+Service. Файлы — committed YAML в manifests/keeper/.
	repoRoot := repoRootFromTestWD(t)
	deploymentPath := filepath.Join(repoRoot, "tests", "e2e-k8s", "manifests", "keeper", "deployment.yaml")
	s.Cluster.KubectlApply(t, deploymentPath)

	// 5. Patch replicas, если запрошено != 3 (manifest коммитится с replicas:3
	//    как L3c-3-default для multi-keeper HA; legacy single-keeper L3c-2-тесты
	//    патчат в 1).
	if replicas != 3 {
		patch := fmt.Sprintf(`{"spec":{"replicas":%d}}`, replicas)
		patchCmd := exec.Command("kubectl", "patch", "deployment", "keeper",
			"-n", "default", "--type", "merge", "-p", patch)
		patchCmd.Env = append(os.Environ(), "KUBECONFIG="+s.Cluster.Kubeconfig)
		if out, err := patchCmd.CombinedOutput(); err != nil {
			t.Fatalf("kubectl patch keeper replicas=%d: %v\n%s", replicas, err, out)
		}
	}

	// 6. Wait Ready (Deployment.status.readyReplicas == spec.replicas).
	waitDeploymentReady(t, s.Clientset, "default", "keeper", int32(replicas), 5*time.Minute)
	s.KeeperOpenAPIPort = 8080

	// 7. Port-forward keeper-svc:8080 → host loopback. Используется ping-тестом.
	return s.Cluster.PortForward(t, "svc/keeper", s.KeeperOpenAPIPort, 60*time.Second)
}

// waitDeploymentReady блокируется до тех пор, пока Deployment.status.readyReplicas
// не достигнет ожидаемого значения. Поллинг 1s — k8s-watch overkill для test-
// окружения с маленьким cluster-state.
func waitDeploymentReady(t *testing.T, cs *kubernetes.Clientset, ns, name string, want int32, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		dep, err := cs.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		cancel()
		if err == nil {
			if dep.Status.ReadyReplicas >= want {
				return
			}
			t.Logf("waiting deployment %s/%s: ready=%d/%d", ns, name, dep.Status.ReadyReplicas, want)
		} else {
			t.Logf("get deployment %s/%s: %v", ns, name, err)
		}
		time.Sleep(2 * time.Second)
	}
	// На timeout пытаемся подтянуть pod-events для диагностики.
	dumpKeeperPodDiagnostics(t, cs, ns, name)
	t.Fatalf("deployment %s/%s did not become Ready (want=%d) within %v", ns, name, want, timeout)
}

// dumpKeeperPodDiagnostics — best-effort дамп events+logs первого pod-а
// keeper-deployment-а при timeout-е. Чисто диагностика — ошибки glob/list
// игнорируем (мы уже в fatal-пути).
func dumpKeeperPodDiagnostics(t *testing.T, cs *kubernetes.Clientset, ns, deploymentName string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: "app=" + deploymentName})
	if err != nil {
		t.Logf("dump: list pods: %v", err)
		return
	}
	for _, p := range pods.Items {
		t.Logf("pod %s phase=%s", p.Name, p.Status.Phase)
		for _, cs := range p.Status.ContainerStatuses {
			t.Logf("  container=%s ready=%v restarts=%d state=%+v", cs.Name, cs.Ready, cs.RestartCount, cs.State)
		}
	}
}

// repoRootFromTestWD возвращает абсолютный путь к корню репо. Test-cwd —
// `tests/e2e-k8s/`, поэтому корень репо = `../..`.
func repoRootFromTestWD(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}

// DeploySoul разворачивает Soul-StatefulSet (privileged systemd-PID-1 Debian-12,
// parity с L3b) в kind: грузит локально собранный образ `soul:e2e-k8s` через
// `kind load docker-image`, заводит bootstrap-token + souls-row через port-
// forward к PG, создаёт ConfigMap (soul.yml + ca.pem) и Secret (bootstrap-
// token), apply StatefulSet+headless Service, ждёт Ready pod, выполняет
// `soul init` (реальный CSR Bootstrap-flow через gRPC к keeper:9094) и
// `systemctl start soul.service`, блокируется до `souls.status='connected'`.
//
// Single-Soul в L3c-3 (replicas=1 в statefulset.yaml жёстко). Multi-Soul — L3c-5.
//
// Возвращает SID единственного Soul-а (`soul-0.example.com`) для последующих
// assert-ов.
func (s *Stack) DeploySoul(t *testing.T) string {
	t.Helper()

	const (
		sid     = "soul-0.example.com"
		podName = "soul-0"
	)

	// 1. Load image в kind-узел.
	s.Cluster.LoadDockerImage(t, "soul:e2e-k8s")

	// 2. Issue bootstrap-token через port-forward к PG (keeper уже создал
	//    схему миграциями на старте `keeper run`).
	token := IssueBootstrapToken(t, s, sid)

	// 3. Создаём Secret (bootstrap-token plain-byte) + ConfigMap (soul.yml + CA).
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	bootstrapSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "soul-bootstrap", Namespace: "default"},
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			// plain-token: соль-агент читает первой строкой через --token=
			// в `soul init` (см. ниже execInPod).
			"bootstrap-token": []byte(token),
		},
	}
	if _, err := s.Clientset.CoreV1().Secrets("default").Create(ctx, bootstrapSecret, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create secret soul-bootstrap: %v", err)
	}

	soulYAML := renderSoulYAML(sid)
	soulConfigMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "soul-config", Namespace: "default"},
		Data: map[string]string{
			"soul.yml": soulYAML,
			"ca.pem":   string(s.CABundle),
		},
	}
	if _, err := s.Clientset.CoreV1().ConfigMaps("default").Create(ctx, soulConfigMap, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create configmap soul-config: %v", err)
	}

	// 4. Apply StatefulSet+Service.
	repoRoot := repoRootFromTestWD(t)
	statefulsetPath := filepath.Join(repoRoot, "tests", "e2e-k8s", "manifests", "soul", "statefulset.yaml")
	s.Cluster.KubectlApply(t, statefulsetPath)

	// 5. Wait pod-0 Ready. StatefulSet с replicas=1 — ждём readyReplicas=1.
	//    Таймаут 3 мин: systemd boot ~10s + image-load 0 (уже в kind), но
	//    image build-cost уже снесён (LoadDockerImage), таким образом основное
	//    время = systemd-init (~10-20s в kind/Linux).
	waitStatefulSetReady(t, s.Clientset, "default", "soul", 1, 3*time.Minute)

	// 6. `soul init` — реальный CSR Bootstrap-flow. SID matches CN cert-а
	//    (PKI role soul-seed allow_any_name=true, alt-name example.com).
	initOut, initCode, err := s.execInPod(ctx, podName, []string{
		"/usr/local/bin/soul", "init",
		"--config", "/etc/soul/soul.yml",
		"--token", token,
		"--sid", sid,
	})
	if err != nil || initCode != 0 {
		t.Fatalf("DeploySoul: soul init: code=%d err=%v output=%s", initCode, err, initOut)
	}
	t.Logf("DeploySoul: soul init ok: %s", initOut)

	// 7. systemctl start soul.service. Unit-файл уже baked в image, не enabled
	//    by default — стартуем после `soul init` (SoulSeed теперь есть).
	startOut, startCode, err := s.execInPod(ctx, podName, []string{
		"systemctl", "start", "soul.service",
	})
	if err != nil || startCode != 0 {
		t.Fatalf("DeploySoul: systemctl start soul.service: code=%d err=%v output=%s",
			startCode, err, startOut)
	}

	// 8. Wait souls.status='connected'.
	WaitForSoulConnected(t, s, sid, 2*time.Minute)

	return sid
}

// renderSoulYAML возвращает soul.yml для конфига Soul-агента внутри pod-а.
// Endpoints — Service-DNS `keeper:9094/9095` (in-cluster ClusterIP);
// keeper.tls.ca — `/etc/soul/ca.pem` (projected volume из ConfigMap).
// paths.seed — `/var/lib/soul-stack/seed` (создан в Dockerfile).
//
// SID в config-е — резерв на случай переопределения; harness в `soul init`
// передаёт --sid явно (precedence: --sid > config.sid > hostname).
func renderSoulYAML(sid string) string {
	const tmpl = `sid: %s
paths:
  seed: /var/lib/soul-stack/seed
  modules: /var/lib/soul-stack/modules
keeper:
  endpoints:
    - host: keeper.default.svc.cluster.local
      bootstrap_port: 9094
      event_stream_port: 9095
      priority: 1
  tls:
    ca: /etc/soul/ca.pem
logging:
  level: info
  format: text
hot_reload:
  enable_signal: false
  enable_inotify: false
`
	return fmt.Sprintf(tmpl, sid)
}

// waitStatefulSetReady блокируется до тех пор, пока StatefulSet.status.readyReplicas
// не достигнет want. Поллинг 2s — k8s-watch overkill для test-окружения.
func waitStatefulSetReady(t *testing.T, cs *kubernetes.Clientset, ns, name string, want int32, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		ss, err := cs.AppsV1().StatefulSets(ns).Get(ctx, name, metav1.GetOptions{})
		cancel()
		if err == nil {
			if ss.Status.ReadyReplicas >= want {
				return
			}
			t.Logf("waiting statefulset %s/%s: ready=%d/%d", ns, name, ss.Status.ReadyReplicas, want)
		} else {
			t.Logf("get statefulset %s/%s: %v", ns, name, err)
		}
		time.Sleep(2 * time.Second)
	}
	dumpSoulPodDiagnostics(t, cs, ns, name)
	t.Fatalf("statefulset %s/%s did not become Ready (want=%d) within %v", ns, name, want, timeout)
}

// dumpSoulPodDiagnostics — best-effort дамп pod-ов StatefulSet-а при timeout-е.
func dumpSoulPodDiagnostics(t *testing.T, cs *kubernetes.Clientset, ns, statefulsetName string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: "app=" + statefulsetName})
	if err != nil {
		t.Logf("dump: list pods: %v", err)
		return
	}
	for _, p := range pods.Items {
		t.Logf("pod %s phase=%s", p.Name, p.Status.Phase)
		for _, cs := range p.Status.ContainerStatuses {
			t.Logf("  container=%s ready=%v restarts=%d state=%+v", cs.Name, cs.Ready, cs.RestartCount, cs.State)
		}
	}
}

// ExecInSoulPod — публичный alias execInPod для использования из тестов
// (verify-asserts типа `redis-cli cluster info`). Симметрично L3b
// SoulContainer.Exec, но через k8s REST API.
func (s *Stack) ExecInSoulPod(ctx context.Context, podName string, cmd []string) (string, int, error) {
	return s.execInPod(ctx, podName, cmd)
}

// execInPod — kubectl-exec эквивалент через client-go remotecommand-executor.
// Возвращает (combined stdout+stderr, exitCode, error). Симметрично L3b
// SoulContainer.Exec, но через k8s REST API, а не docker.
//
// Container-имя жёстко "soul" (см. statefulset.yaml::containers[0].name).
func (s *Stack) execInPod(ctx context.Context, podName string, cmd []string) (string, int, error) {
	req := s.Clientset.CoreV1().RESTClient().
		Post().
		Resource("pods").
		Name(podName).
		Namespace("default").
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: "soul",
			Command:   cmd,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(s.RESTConfig, "POST", req.URL())
	if err != nil {
		return "", -1, fmt.Errorf("new SPDY executor: %w", err)
	}

	var combined bytes.Buffer
	streamErr := exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &combined,
		Stderr: &combined,
	})
	if streamErr == nil {
		return combined.String(), 0, nil
	}
	// remotecommand упаковывает non-zero exit как CodeExitError.
	type exitCoder interface{ ExitStatus() int }
	if ec, ok := streamErr.(exitCoder); ok {
		return combined.String(), ec.ExitStatus(), nil
	}
	return combined.String(), -1, streamErr
}
