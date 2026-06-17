//go:build e2e_k8s

package harness

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"testing"
	"time"
)

// portforward.go — обёртка над `kubectl port-forward` для host-side доступа к
// ClusterIP-сервисам в kind-кластере.
//
// Реализация: спавним long-running subprocess `kubectl port-forward
// <target> :<remotePort>` (local-port=0 → kubectl выбирает свободный),
// парсим stdout первой строкой `Forwarding from 127.0.0.1:<port> -> ...`,
// возвращаем PortForward с LocalPort. Close() — kill процесса.

// PortForward — handle к live `kubectl port-forward`-туннелю.
type PortForward struct {
	LocalPort int
	cmd       *exec.Cmd
}

// Close завершает port-forward-подпроцесс. Идемпотентен.
func (pf *PortForward) Close() {
	if pf == nil || pf.cmd == nil || pf.cmd.Process == nil {
		return
	}
	_ = pf.cmd.Process.Kill()
	_ = pf.cmd.Wait()
}

// portForwardLineRE — формат первой строки stdout `kubectl port-forward`:
//
//	Forwarding from 127.0.0.1:54321 -> 8200
//
// На IPv6-окружениях может быть `[::1]:` — игнорируем, ловим только 127.0.0.1
// (мы запрашиваем `--address 127.0.0.1` явно).
var portForwardLineRE = regexp.MustCompile(`Forwarding from 127\.0\.0\.1:(\d+) ->`)

// PortForward спавнит `kubectl port-forward <target> :<remotePort>` и
// блокируется до первой строки stdout с локальным портом. timeout — deadline
// на готовность туннеля. t.Cleanup регистрирует Close.
//
// target — k8s-форма ресурса (`svc/vault`, `service/postgres-postgresql`,
// `pod/keeper-xxx`).
func (c *Cluster) PortForward(t *testing.T, target string, remotePort int, timeout time.Duration) *PortForward {
	t.Helper()

	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skipf("L3c: kubectl не найден в PATH: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "kubectl",
		"port-forward",
		"--address", "127.0.0.1",
		target,
		fmt.Sprintf(":%d", remotePort),
	)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+c.Kubeconfig)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatalf("port-forward %s: stdout pipe: %v", target, err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		t.Fatalf("port-forward %s: stderr pipe: %v", target, err)
	}

	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("port-forward %s start: %v", target, err)
	}

	// Stderr форвардим в t.Log (kubectl на ошибках пишет туда).
	go drainToTestLog(t, "pf-stderr:"+target, stderr)

	// Stdout — ловим первую строку с локальным портом; остаток дренируем.
	portCh := make(chan int, 1)
	errCh := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			if matches := portForwardLineRE.FindStringSubmatch(line); len(matches) == 2 {
				p, perr := strconv.Atoi(matches[1])
				if perr != nil {
					errCh <- fmt.Errorf("parse port from %q: %w", line, perr)
					return
				}
				portCh <- p
				// Дренируем остаток — иначе kubectl заблокируется на pipe write.
				// scanner-буфер уже потреблён, читаем напрямую с stdout pipe.
				_, _ = io.Copy(io.Discard, stdout)
				return
			}
			t.Logf("[pf-stdout:%s] %s", target, line)
		}
		if err := scanner.Err(); err != nil {
			errCh <- err
			return
		}
		errCh <- fmt.Errorf("stdout closed without Forwarding-line")
	}()

	pf := &PortForward{cmd: cmd}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	select {
	case p := <-portCh:
		pf.LocalPort = p
	case err := <-errCh:
		cancel()
		_ = cmd.Wait()
		t.Fatalf("port-forward %s: %v", target, err)
	case <-deadline.C:
		cancel()
		_ = cmd.Wait()
		t.Fatalf("port-forward %s: timeout %v без Forwarding-line", target, timeout)
	}

	t.Cleanup(func() {
		cancel()
		pf.Close()
	})
	return pf
}

// drainToTestLog читает r построчно и форвардит в t.Logf. Используется для
// stderr/stdout-tail port-forward subprocess.
func drainToTestLog(t *testing.T, prefix string, r io.Reader) {
	t.Helper()
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		t.Logf("[%s] %s", prefix, scanner.Text())
	}
}
