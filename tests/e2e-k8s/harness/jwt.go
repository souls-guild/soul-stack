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

// jwt.go — выпуск bootstrap-JWT первого Архонта через `keeper init` в одном из
// running keeper-pod-ов. Симметрично L3b runKeeperInit, но keeper-binary в L3c
// крутится внутри pod-а, а не host-side; exec-им через k8s remotecommand API.
//
// Почему через exec в running pod, а не отдельный Job: keeper-pod-ы уже
// запущены с `--initialize` (bootstrap-pending mode), все зависимости (PG/
// Vault/Redis) доступны через service-DNS внутри cluster-сети — самый дешёвый
// путь. PG advisory lock в keeper init гарантирует idempotency, даже если
// случайно вызвать на разных pod-ах одновременно.
//
// Возвращает plain JWT — caller (тест) кладёт в Authorization Bearer.

// BootstrapArchon — wrapper, заполняющий Stack.JWT через IssueBootstrapArchon.
// Идемпотентен: повторный вызов на уже инициализированном keeper падёт на
// keeper init exit 1 ("already initialized"). В L3c-5 каждый тест поднимает
// свой kind-cluster (per-test isolation), повторного вызова не бывает.
func (s *Stack) BootstrapArchon(t *testing.T) {
	t.Helper()
	s.JWT = IssueBootstrapArchon(t, s)
}

// IssueBootstrapArchon выполняет `keeper init --archon=archon-test
// --credential-out=/tmp/archon.token` в первом running keeper-pod-е и читает
// результат через `cat`. Возвращает plain-JWT (без trailing newline).
//
// Требует: keeper-deployment Ready (ReadyReplicas>=1) до вызова.
//
// archonAID валидируется на стороне keeper (regex), здесь жёстко
// `archon-test` — единое имя на все L3c-тесты, не требующее настройки.
func IssueBootstrapArchon(t *testing.T, s *Stack) string {
	t.Helper()

	const (
		archonAID = "archon-test"
		credPath  = "/tmp/archon-test.credential"
	)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	podName := firstReadyKeeperPod(t, s, ctx)

	// `keeper init` — exit 0 при успехе. На повторный вызов (operators
	// non-empty) вернёт exit 1 с сообщением «already initialized» — для теста
	// это всё ещё ошибка, тест ожидает чистый keeper-startup.
	out, code, err := s.execInKeeperPod(ctx, podName, []string{
		"/usr/local/bin/keeper", "init",
		"--archon=" + archonAID,
		"--config=/etc/keeper/keeper.yml",
		"--credential-out=" + credPath,
	})
	if err != nil || code != 0 {
		t.Fatalf("IssueBootstrapArchon: keeper init: code=%d err=%v output=%s", code, err, out)
	}

	// Прочитать credential-файл через cat. Permission 0400 — keeper-uid-у
	// доступен; pod бежит как root.
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

// firstReadyKeeperPod возвращает имя первого pod-а с label app=keeper и
// PodRunning + Ready. Опрашивает до 60s — keeper-deployment-у нужно время на
// readiness (миграции PG).
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
	t.Fatalf("firstReadyKeeperPod: ни одного Ready pod app=keeper за 60s")
	return ""
}

// execInKeeperPod — exec в container=keeper. Симметричен Stack.execInPod, но
// container-name "keeper" вместо "soul" (statefulset.yaml::containers[0].name
// для soul-pod = "soul", deployment.yaml::containers[0].name для keeper-pod
// = "keeper"). DRY-объединение через параметр container — over-engineering
// для двух call-сайтов.
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
