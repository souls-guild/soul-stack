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

// portforward.go — a wrapper over `kubectl port-forward` for host-side
// access to ClusterIP services in the kind cluster.
//
// Implementation: spawn a long-running subprocess `kubectl port-forward
// <target> :<remotePort>` (local-port=0 -> kubectl picks a free one), parse
// the first stdout line `Forwarding from 127.0.0.1:<port> -> ...`, return a
// PortForward with LocalPort. Close() kills the process.

// PortForward — a handle to a live `kubectl port-forward` tunnel.
type PortForward struct {
	LocalPort int
	cmd       *exec.Cmd
}

// Close terminates the port-forward subprocess. Idempotent.
func (pf *PortForward) Close() {
	if pf == nil || pf.cmd == nil || pf.cmd.Process == nil {
		return
	}
	_ = pf.cmd.Process.Kill()
	_ = pf.cmd.Wait()
}

// portForwardLineRE — the format of the first stdout line of
// `kubectl port-forward`:
//
//	Forwarding from 127.0.0.1:54321 -> 8200
//
// On IPv6 environments this may be `[::1]:` -- we ignore that and only
// match 127.0.0.1 (we explicitly request `--address 127.0.0.1`).
var portForwardLineRE = regexp.MustCompile(`Forwarding from 127\.0\.0\.1:(\d+) ->`)

// PortForward spawns `kubectl port-forward <target> :<remotePort>` and
// blocks until the first stdout line with the local port. timeout is the
// deadline for the tunnel to become ready. t.Cleanup registers Close.
//
// target — a k8s resource reference (`svc/vault`,
// `service/postgres-postgresql`, `pod/keeper-xxx`).
func (c *Cluster) PortForward(t *testing.T, target string, remotePort int, timeout time.Duration) *PortForward {
	t.Helper()

	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skipf("L3c: kubectl not found in PATH: %v", err)
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

	// Forward stderr to t.Log (kubectl writes errors there).
	go drainToTestLog(t, "pf-stderr:"+target, stderr)

	// Stdout — capture the first line with the local port; drain the rest.
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
				// Drain the rest -- otherwise kubectl blocks on the pipe write.
				// The scanner buffer is already consumed, so read directly
				// from the stdout pipe.
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
		t.Fatalf("port-forward %s: timeout %v without a Forwarding line", target, timeout)
	}

	t.Cleanup(func() {
		cancel()
		pf.Close()
	})
	return pf
}

// drainToTestLog reads r line by line and forwards to t.Logf. Used for
// stderr/stdout tailing of the port-forward subprocess.
func drainToTestLog(t *testing.T, prefix string, r io.Reader) {
	t.Helper()
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		t.Logf("[%s] %s", prefix, scanner.Text())
	}
}
