//go:build e2e_k8s

package harness

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
)

// jwt.go — issues the first Archon's bootstrap JWT via `keeper init` in one
// of the running keeper pods. Symmetric to L3b runKeeperInit, but in L3c the
// keeper binary runs inside a pod, not host-side; we exec via the k8s
// remotecommand API.
//
// Why exec into a running pod rather than a separate Job: keeper pods are
// already started with `--initialize` (bootstrap-pending mode), and all
// dependencies (PG/Vault/Redis) are reachable via service DNS inside the
// cluster network -- the cheapest path. The PG advisory lock in keeper init
// guarantees idempotency even if it's accidentally called on different pods
// at the same time.
//
// Returns a plain JWT -- the caller (test) puts it in the Authorization
// Bearer header.

// BootstrapArchon — wrapper that fills Stack.JWT via IssueBootstrapArchon.
// Idempotent: a repeat call on an already-initialized keeper fails with
// keeper init exit 1 ("already initialized"). In L3c-5 each test spins up
// its own kind cluster (per-test isolation), so a repeat call never happens.
func (s *Stack) BootstrapArchon(t *testing.T) {
	t.Helper()
	s.JWT = IssueBootstrapArchon(t, s)
}

// IssueBootstrapArchon runs `keeper init --archon=archon-test
// --credential-out=/tmp/archon.token` in the first running keeper pod and
// reads the result via `cat`. Returns the plain JWT (no trailing newline).
//
// Requires: keeper deployment Ready (ReadyReplicas>=1) before calling.
//
// archonAID is validated keeper-side (regex); here it's hardcoded to
// `archon-test` -- a single name shared by all L3c tests, no config needed.
func IssueBootstrapArchon(t *testing.T, s *Stack) string {
	t.Helper()

	const (
		archonAID = "archon-test"
		credPath  = "/tmp/archon-test.credential"
	)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	podName := firstReadyKeeperPod(t, s, ctx)

	// `keeper init` -- exit 0 on success. A repeat call (operators
	// non-empty) returns exit 1 with "already initialized" -- for this test
	// that's still an error, since the test expects a clean keeper startup.
	out, code, err := s.execInKeeperPod(ctx, podName, []string{
		"/usr/local/bin/keeper", "init",
		"--archon=" + archonAID,
		"--config=/etc/keeper/keeper.yml",
		"--credential-out=" + credPath,
	})
	if err != nil || code != 0 {
		t.Fatalf("IssueBootstrapArchon: keeper init: code=%d err=%v output=%s", code, err, out)
	}

	// Read the credential file via cat. Permission 0400 -- accessible to
	// the keeper uid; the pod runs as root.
	credOut, credCode, credErr := s.execInKeeperPod(ctx, podName, []string{
		"cat", credPath,
	})
	if credErr != nil || credCode != 0 {
		t.Fatalf("IssueBootstrapArchon: cat %s: code=%d err=%v output=%s",
			credPath, credCode, credErr, credOut)
	}
	jwt := strings.TrimSpace(credOut)
	if jwt == "" {
		t.Fatalf("IssueBootstrapArchon: credential file %s is empty", credPath)
	}
	return jwt
}

// firstReadyKeeperPod returns the name of the first pod with label
// app=keeper that is PodRunning + Ready. Polls for up to 60s -- the keeper
// deployment needs time to become ready (PG migrations).
func firstReadyKeeperPod(t *testing.T, s *Stack, ctx context.Context) string {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		pods, err := s.Clientset.CoreV1().Pods("default").List(ctx, metav1.ListOptions{
			LabelSelector: "app=keeper",
		})
		if err == nil {
			for _, p := range pods.Items {
				if p.Status.Phase != corev1.PodRunning {
					continue
				}
				for _, c := range p.Status.ContainerStatuses {
					if c.Ready {
						return p.Name
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("firstReadyKeeperPod: ctx done: %v", ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
	t.Fatalf("firstReadyKeeperPod: no Ready pod with app=keeper within 60s")
	return ""
}

// execInKeeperPod — exec into container=keeper. Symmetric to
// Stack.execInPod, but with container name "keeper" instead of "soul"
// (statefulset.yaml::containers[0].name for a soul pod = "soul",
// deployment.yaml::containers[0].name for a keeper pod = "keeper"). A DRY
// merge via a container parameter would be over-engineering for two call
// sites.
func (s *Stack) execInKeeperPod(ctx context.Context, podName string, cmd []string) (string, int, error) {
	req := s.Clientset.CoreV1().RESTClient().
		Post().
		Resource("pods").
		Name(podName).
		Namespace("default").
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: "keeper",
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
	type exitCoder interface{ ExitStatus() int }
	if ec, ok := streamErr.(exitCoder); ok {
		return combined.String(), ec.ExitStatus(), nil
	}
	return combined.String(), -1, streamErr
}
