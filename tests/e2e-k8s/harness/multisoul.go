//go:build e2e_k8s

package harness

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// multisoul.go — DeployMultiSoul для L3c-5 (TestL3cToll_DegradedMode и
// TestL3cRedisCluster_Resharding).
//
// Архитектурное отличие от [Stack.DeploySoul] (L3c-3, single-pod StatefulSet):
// поднимаем N независимых Pod-ов (kind=Pod) через client-go, не StatefulSet.
// Причина — каждый Soul-pod нуждается в собственном bootstrap-token (per-SID
// уникальном), а template StatefulSet-а не умеет в per-replica Secret-ы без
// initContainer-magic. Прямые Pod-ы дают одну логическую единицу = одна
// уникальная конфигурация без YAML-шаблонов.
//
// Pod-ы создаются с labels `app=soul-multi` (отличить от L3c-3 StatefulSet-а
// app=soul); pod-имена — `soul-0..soul-{N-1}`; SID-ы — `soul-<i>.example.com`.

// DeployMultiSoul поднимает N независимых Pod-ов Soul. Симметрично DeploySoul,
// но без StatefulSet (per-pod Secret/ConfigMap). Каждый Pod проходит реальный
// CSR Bootstrap-flow и устанавливает EventStream-соединение.
//
// SID-схема: `soul-<i>.example.com` для i=[0..count-1].
//
// Блокируется до `souls.status='connected'` у всех N. timeout-окно широкое —
// systemd-boot + soul init + EventStream-attach на 5 pod-ах под Kind/Linux
// занимает 1-3 мин.
func (s *Stack) DeployMultiSoul(t *testing.T, count int) []string {
	t.Helper()
	if count < 1 {
		t.Fatalf("DeployMultiSoul: count must be >= 1, got %d", count)
	}

	// 1. Load image один раз. kind load — idempotent (повторный load на тот же
	//    tag — no-op).
	s.Cluster.LoadDockerImage(t, "soul:e2e-k8s")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	sids := make([]string, count)
	tokens := make([]string, count)
	podNames := make([]string, count)

	// 2. Issue bootstrap-token per soul + создать Secret/ConfigMap per pod.
	for i := 0; i < count; i++ {
		sid := fmt.Sprintf("soul-%d.example.com", i)
		podName := fmt.Sprintf("soul-%d", i)
		sids[i] = sid
		podNames[i] = podName

		token := IssueBootstrapToken(t, s, sid)
		tokens[i] = token

		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("soul-bootstrap-%d", i),
				Namespace: "default",
			},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{
				"bootstrap-token": []byte(token),
			},
		}
		if _, err := s.Clientset.CoreV1().Secrets("default").Create(ctx, secret, metav1.CreateOptions{}); err != nil {
			t.Fatalf("DeployMultiSoul: create secret soul-bootstrap-%d: %v", i, err)
		}

		soulYAML := renderSoulYAML(sid)
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("soul-config-%d", i),
				Namespace: "default",
			},
			Data: map[string]string{
				"soul.yml": soulYAML,
				"ca.pem":   string(s.CABundle),
			},
		}
		if _, err := s.Clientset.CoreV1().ConfigMaps("default").Create(ctx, cm, metav1.CreateOptions{}); err != nil {
			t.Fatalf("DeployMultiSoul: create configmap soul-config-%d: %v", i, err)
		}
	}

	// 3. Создать Pod-ы. Параллельно для скорости (5 pod-ов × 30s sequential
	//    create — это лишние 2 мин). Apply через client-go (без YAML).
	for i := 0; i < count; i++ {
		pod := buildSoulPod(i)
		if _, err := s.Clientset.CoreV1().Pods("default").Create(ctx, pod, metav1.CreateOptions{}); err != nil {
			t.Fatalf("DeployMultiSoul: create pod soul-%d: %v", i, err)
		}
	}

	// 4. Wait All N pod Ready.
	waitMultiSoulReady(t, s, podNames, 4*time.Minute)

	// 5. `soul init` + `systemctl start soul.service` per-pod. Sequential —
	//    параллель тут не критична (init быстрый ~1-2s).
	for i, podName := range podNames {
		initCtx, initCancel := context.WithTimeout(context.Background(), 60*time.Second)
		out, code, err := s.execInPod(initCtx, podName, []string{
			"/usr/local/bin/soul", "init",
			"--config", "/etc/soul/soul.yml",
			"--token", tokens[i],
			"--sid", sids[i],
		})
		initCancel()
		if err != nil || code != 0 {
			t.Fatalf("DeployMultiSoul: soul init pod=%s: code=%d err=%v out=%s",
				podName, code, err, out)
		}

		startCtx, startCancel := context.WithTimeout(context.Background(), 30*time.Second)
		out2, code2, err2 := s.execInPod(startCtx, podName, []string{
			"systemctl", "start", "soul.service",
		})
		startCancel()
		if err2 != nil || code2 != 0 {
			t.Fatalf("DeployMultiSoul: systemctl start pod=%s: code=%d err=%v out=%s",
				podName, code2, err2, out2)
		}
	}

	// 6. Wait souls.status='connected' для всех.
	for _, sid := range sids {
		WaitForSoulConnected(t, s, sid, 2*time.Minute)
	}

	return sids
}

// buildSoulPod собирает Pod-spec для i-го Soul-агента. Симметрично
// statefulset.yaml::template, но как Go-структура.
func buildSoulPod(i int) *corev1.Pod {
	podName := fmt.Sprintf("soul-%d", i)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: "default",
			Labels: map[string]string{
				"app":     "soul-multi",
				"soul-id": fmt.Sprintf("%d", i),
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:            "soul",
				Image:           "soul:e2e-k8s",
				ImagePullPolicy: corev1.PullNever,
				SecurityContext: &corev1.SecurityContext{
					Privileged: ptrBool(true),
				},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "config", MountPath: "/etc/soul"},
					{Name: "cgroup", MountPath: "/sys/fs/cgroup"},
					{Name: "run", MountPath: "/run"},
					{Name: "run-lock", MountPath: "/run/lock"},
				},
			}},
			Volumes: []corev1.Volume{
				{
					Name: "config",
					VolumeSource: corev1.VolumeSource{
						Projected: &corev1.ProjectedVolumeSource{
							Sources: []corev1.VolumeProjection{
								{
									ConfigMap: &corev1.ConfigMapProjection{
										LocalObjectReference: corev1.LocalObjectReference{
											Name: fmt.Sprintf("soul-config-%d", i),
										},
										Items: []corev1.KeyToPath{
											{Key: "soul.yml", Path: "soul.yml"},
											{Key: "ca.pem", Path: "ca.pem"},
										},
									},
								},
								{
									Secret: &corev1.SecretProjection{
										LocalObjectReference: corev1.LocalObjectReference{
											Name: fmt.Sprintf("soul-bootstrap-%d", i),
										},
										Items: []corev1.KeyToPath{
											{Key: "bootstrap-token", Path: "bootstrap-token"},
										},
									},
								},
							},
						},
					},
				},
				{
					Name: "cgroup",
					VolumeSource: corev1.VolumeSource{
						HostPath: &corev1.HostPathVolumeSource{
							Path: "/sys/fs/cgroup",
							Type: ptrHostPathType(corev1.HostPathDirectory),
						},
					},
				},
				{
					Name: "run",
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory},
					},
				},
				{
					Name: "run-lock",
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory},
					},
				},
			},
		},
	}
}

// waitMultiSoulReady блокируется до тех пор, пока все pod-ы podNames не дойдут
// до PodRunning + Ready=true. Поллинг 2s.
func waitMultiSoulReady(t *testing.T, s *Stack, podNames []string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	remaining := make(map[string]struct{}, len(podNames))
	for _, n := range podNames {
		remaining[n] = struct{}{}
	}
	for time.Now().Before(deadline) && len(remaining) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		for name := range remaining {
			p, err := s.Clientset.CoreV1().Pods("default").Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				continue
			}
			if p.Status.Phase != corev1.PodRunning {
				continue
			}
			ready := false
			for _, cs := range p.Status.ContainerStatuses {
				if cs.Name == "soul" && cs.Ready {
					ready = true
					break
				}
			}
			if ready {
				delete(remaining, name)
			}
		}
		cancel()
		if len(remaining) == 0 {
			return
		}
		t.Logf("waitMultiSoulReady: pending pods=%d", len(remaining))
		time.Sleep(2 * time.Second)
	}
	if len(remaining) > 0 {
		left := make([]string, 0, len(remaining))
		for n := range remaining {
			left = append(left, n)
		}
		t.Fatalf("waitMultiSoulReady: pods не достигли Ready за %v: %v", timeout, left)
	}
}

func ptrBool(b bool) *bool                                 { return &b }
func ptrHostPathType(t corev1.HostPathType) *corev1.HostPathType { return &t }
