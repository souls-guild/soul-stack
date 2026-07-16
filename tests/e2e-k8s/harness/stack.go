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

// Config — L3c stand parameters. ExamplePath / Souls are added in L3c-3
// (Soul StatefulSet and a real service fixture).
type Config struct {
	// ExamplePath — path to examples/service/<name>/ (L3c-3 git-loader).
	ExamplePath string

	// ServiceName — the bare service name for CreateIncarnation (L3c-5
	// redis-cluster resharding and a future service-registry pre-seed).
	ServiceName string

	// Souls — number of Soul StatefulSet replicas (L3c-3).
	Souls int

	// ReaperEnabled — enables the Reaper loop in keeper.yml (L3c-4
	// failover). Default false: L3c-2/3 doesn't need the background job,
	// to minimize side effects. L3c-4 enables it to verify leader election
	// on kill-pod.
	ReaperEnabled bool
}

// Stack — high-level harness wrapper following the L3a/L3b contract.
//
// L3c-2 lifecycle:
//
//	NewStack(t, cfg) -> DeployInfra(t) -> DeployKeeper(t, replicas)
//	  -> KeeperReadyzURL() -> test asserts -> t.Cleanup (LIFO)
//
// L3c-3 adds DeploySoul and the real bootstrap flow.
type Stack struct {
	Cluster   *Cluster
	Clientset *kubernetes.Clientset
	// RESTConfig — for kubectl-exec via client-go (DeploySoul invokes the
	// `kubectl exec` equivalent of `soul init` + `systemctl start` inside
	// the pod).
	RESTConfig *rest.Config

	// reaperEnabled — a copy of Config.ReaperEnabled. Needed by
	// DeployKeeper when rendering keeper.yml. Stored on Stack because
	// NewStack takes Config once, while DeployKeeper is called separately.
	reaperEnabled bool

	// CABundle — the Vault PKI root CA in PEM (filled by DeployInfra). The
	// Soul pod gets it as `/etc/soul/ca.pem` (via ConfigMap) for the
	// server-only TLS handshake with keeper during `soul init`.
	CABundle []byte

	// JWT — the first operator's bootstrap-Archon JWT (issued via
	// `keeper init` in a running keeper pod). Filled by
	// [Stack.BootstrapArchon]. Empty string means bootstrap hasn't run yet.
	JWT string

	// Filled by DeployInfra — in-cluster infra service names/addresses.
	// PGServiceDNS — `<host>:<port>` form for keeper.yml::postgres.dsn
	// (via vault-ref).
	PGServiceDNS    string
	RedisServiceDNS string
	VaultServiceDNS string

	// KeeperOpenAPIPort — the (in-cluster) port of the keeper pod's
	// `/readyz` endpoint. Matches
	// manifests/keeper/deployment.yaml::containerPort.
	KeeperOpenAPIPort int

	// vaultPF — port-forward to Vault for the harness-side seed
	// (PKI/JWT/DSN). Closed automatically in t.Cleanup.
	vaultPF *PortForward
}

// NewStack creates the kind cluster for this test and initializes
// client-go. Further steps (DeployInfra/DeployKeeper) are separate methods.
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

// DeployInfra — bitnami Helm install of PostgreSQL/Redis/Vault into the
// default namespace. Charts wait for full readiness (`--wait`), and Vault
// is additionally pinged via port-forward and seeded (PKI mount + JWT
// signing key + PG DSN + keeper-server TLS cert).
//
// On success, the Stack fields PGServiceDNS / RedisServiceDNS /
// VaultServiceDNS are filled with in-cluster service DNS names. The
// keeper-server TLS cert/key/ca are returned as PEM bytes -- the caller
// (DeployKeeper) puts them into a Secret.
func (s *Stack) DeployInfra(t *testing.T) (certPEM, keyPEM, caPEM []byte) {
	t.Helper()
	requireHelm(t)
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skipf("L3c: kubectl not found in PATH: %v", err)
	}

	// 1. Helm repo + charts.
	s.Cluster.helmRepoEnsure(t)
	repoRoot := repoRootFromTestWD(t)
	valuesDir := filepath.Join(repoRoot, "tests", "e2e-k8s", "helm-values")

	s.Cluster.helmInstall(t, "postgres", "bitnami/postgresql",
		filepath.Join(valuesDir, "postgres.yaml"), helmInstallTimeout)
	s.Cluster.helmInstall(t, "redis", "bitnami/redis",
		filepath.Join(valuesDir, "redis.yaml"), helmRedisTimeout)
	s.Cluster.helmInstall(t, "vault", "bitnami/vault",
		filepath.Join(valuesDir, "vault.yaml"), helmVaultTimeout)

	// 2. In-cluster service DNS (bitnami convention `<release>-<chart>`/`<release>-master`).
	// PG DSN is seeded into Vault; keeper reads it via `dsn_ref: vault:secret/keeper/postgres`.
	s.PGServiceDNS = "postgres-postgresql.default.svc.cluster.local:5432"
	s.RedisServiceDNS = "redis-master.default.svc.cluster.local:6379"
	s.VaultServiceDNS = "vault.default.svc.cluster.local:8200"
	pgDSN := fmt.Sprintf("postgresql://postgres:testpass@%s/keeper_test?sslmode=disable", s.PGServiceDNS)

	// 3. Vault seed via port-forward (host-side access to the ClusterIP-only service).
	pf := s.Cluster.PortForward(t, "svc/vault", 8200, 60*time.Second)
	s.vaultPF = pf
	vaultAddr := fmt.Sprintf("http://127.0.0.1:%d", pf.LocalPort)
	waitVaultReady(t, vaultAddr, 60*time.Second)
	certPEM, keyPEM, caPEM = seedVaultSecrets(t, vaultAddr, pgDSN)
	s.CABundle = caPEM
	return certPEM, keyPEM, caPEM
}

// DeployKeeper deploys keeper into kind: TLS Secret + ConfigMap (rendered
// keeper.yml) + Deployment + Service. Loads the locally built
// `keeper:e2e-k8s` image into kind via `kind load docker-image`. Blocks
// until pods are Ready (5m timeout) and until /readyz=200 via port-forward.
//
// replicas — the Deployment's replica count. L3c-2 uses 1; L3c-3 extends it
// to 3.
//
// Returns a port-forward to the keeper pod's /readyz (8080->localhost:<random>).
// pf.Close() is registered in t.Cleanup.
func (s *Stack) DeployKeeper(t *testing.T, replicas int, certPEM, keyPEM, caPEM []byte) *PortForward {
	t.Helper()
	if replicas < 1 {
		t.Fatalf("DeployKeeper: replicas must be >= 1, got %d", replicas)
	}

	// 1. Load image.
	s.Cluster.LoadDockerImage(t, "keeper:e2e-k8s")

	// 2. Render keeper.yml with in-cluster addresses.
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

	// 3. Create Secret + ConfigMap via client-go (atomic, no kubectl).
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

	// 4. Apply raw Deployment+Service. Files are committed YAML in manifests/keeper/.
	repoRoot := repoRootFromTestWD(t)
	deploymentPath := filepath.Join(repoRoot, "tests", "e2e-k8s", "manifests", "keeper", "deployment.yaml")
	s.Cluster.KubectlApply(t, deploymentPath)

	// 5. Patch replicas if requested != 3 (the manifest is committed with
	//    replicas:3 as the L3c-3 default for multi-keeper HA; legacy
	//    single-keeper L3c-2 tests patch it down to 1).
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

	// 7. Port-forward keeper-svc:8080 -> host loopback. Used by the ping test.
	return s.Cluster.PortForward(t, "svc/keeper", s.KeeperOpenAPIPort, 60*time.Second)
}

// waitDeploymentReady blocks until Deployment.status.readyReplicas reaches
// the expected value. Polls every 1s -- k8s-watch would be overkill for a
// test environment with a small cluster state.
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
	// On timeout, try to pull pod events for diagnostics.
	dumpKeeperPodDiagnostics(t, cs, ns, name)
	t.Fatalf("deployment %s/%s did not become Ready (want=%d) within %v", ns, name, want, timeout)
}

// dumpKeeperPodDiagnostics — best-effort dump of events+logs of the first
// keeper-deployment pod on timeout. Diagnostics only -- glob/list errors
// are ignored (we're already on the fatal path).
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

// repoRootFromTestWD returns the absolute path to the repo root. Test cwd
// is `tests/e2e-k8s/`, so the repo root is `../..`.
func repoRootFromTestWD(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}

// DeploySoul deploys a Soul StatefulSet (privileged systemd-PID-1 Debian-12,
// parity with L3b) into kind: loads the locally built `soul:e2e-k8s` image
// via `kind load docker-image`, sets up a bootstrap token + souls row via
// port-forward to PG, creates a ConfigMap (soul.yml + ca.pem) and Secret
// (bootstrap token), applies StatefulSet+headless Service, waits for the
// pod to be Ready, runs `soul init` (a real CSR bootstrap flow over gRPC to
// keeper:9094) and `systemctl start soul.service`, and blocks until
// `souls.status='connected'`.
//
// Single Soul in L3c-3 (replicas=1 hardcoded in statefulset.yaml).
// Multi-Soul is L3c-5.
//
// Returns the SID of the single Soul (`soul-0.example.com`) for subsequent
// asserts.
func (s *Stack) DeploySoul(t *testing.T) string {
	t.Helper()

	const (
		sid     = "soul-0.example.com"
		podName = "soul-0"
	)

	// 1. Load the image into the kind node.
	s.Cluster.LoadDockerImage(t, "soul:e2e-k8s")

	// 2. Issue a bootstrap token via port-forward to PG (keeper already
	//    created the schema via migrations on `keeper run` startup).
	token := IssueBootstrapToken(t, s, sid)

	// 3. Create a Secret (plain-byte bootstrap token) + ConfigMap (soul.yml + CA).
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	bootstrapSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "soul-bootstrap", Namespace: "default"},
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			// plain token: the soul agent reads it via --token= in
			// `soul init` (see execInPod below).
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

	// 5. Wait for pod-0 Ready. StatefulSet with replicas=1 -- we wait for
	//    readyReplicas=1. 3 min timeout: systemd boot ~10s + image-load 0
	//    (already in kind); image build cost is already paid
	//    (LoadDockerImage), so the main time is systemd-init (~10-20s in
	//    kind/Linux).
	waitStatefulSetReady(t, s.Clientset, "default", "soul", 1, 3*time.Minute)

	// 6. `soul init` -- the real CSR bootstrap flow. SID matches the cert's
	//    CN (PKI role soul-seed allow_any_name=true, alt-name example.com).
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

	// 7. systemctl start soul.service. The unit file is already baked into
	//    the image, not enabled by default -- we start it after
	//    `soul init` (a SoulSeed now exists).
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

// renderSoulYAML returns soul.yml for the Soul agent's config inside the
// pod. Endpoints use the Service DNS `keeper:9094/9095` (in-cluster
// ClusterIP); keeper.tls.ca is `/etc/soul/ca.pem` (projected volume from a
// ConfigMap). paths.seed is `/var/lib/soul-stack/seed` (created in the
// Dockerfile).
//
// SID in the config is a fallback in case of override; the harness passes
// --sid explicitly in `soul init` (precedence: --sid > config.sid > hostname).
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

// waitStatefulSetReady blocks until StatefulSet.status.readyReplicas
// reaches want. Polls every 2s -- k8s-watch would be overkill for a test
// environment.
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

// dumpSoulPodDiagnostics — best-effort dump of the StatefulSet's pods on timeout.
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

// ExecInSoulPod — a public alias for execInPod for use from tests
// (verify-asserts like `redis-cli cluster info`). Symmetric to L3b
// SoulContainer.Exec, but via the k8s REST API.
func (s *Stack) ExecInSoulPod(ctx context.Context, podName string, cmd []string) (string, int, error) {
	return s.execInPod(ctx, podName, cmd)
}

// execInPod — the kubectl-exec equivalent via the client-go remotecommand
// executor. Returns (combined stdout+stderr, exitCode, error). Symmetric to
// L3b SoulContainer.Exec, but via the k8s REST API, not docker.
//
// The container name is hardcoded to "soul" (see
// statefulset.yaml::containers[0].name).
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
	// remotecommand wraps a non-zero exit as a CodeExitError.
	type exitCoder interface{ ExitStatus() int }
	if ec, ok := streamErr.(exitCoder); ok {
		return combined.String(), ec.ExitStatus(), nil
	}
	return combined.String(), -1, streamErr
}
