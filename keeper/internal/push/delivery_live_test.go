package push

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// liveShellHandle — a target-server handler that understands:
//   - mkdir -p <paths...>
//   - test -f '<p>' && sha256sum '<p>' || echo MISSING
//   - set -e; cat > <p> && chmod 0755 <p>     (stdin → file, no shell)
//   - rm -rf <paths...>
//
// A narrow sub-shell — exactly what ShaDeliverer/ShaCleaner use. No
// fork/exec of a real /bin/sh — entirely in-process, so the L1 test doesn't
// depend on docker.
type liveFs struct {
	mu    sync.Mutex
	root  string // host tmp dir: host-paths are glued onto it
	dirs  map[string]bool
	files map[string][]byte
}

func newLiveFs(t *testing.T) *liveFs {
	t.Helper()
	return &liveFs{root: t.TempDir(), dirs: map[string]bool{}, files: map[string][]byte{}}
}

func (fs *liveFs) handle(t *testing.T, cmd string, stdin io.Reader) (stdout string, exit uint32) {
	t.Helper()
	fs.mu.Lock()
	defer fs.mu.Unlock()
	switch {
	case strings.HasPrefix(cmd, "mkdir -p"):
		for _, p := range strings.Fields(strings.TrimPrefix(cmd, "mkdir -p")) {
			fs.dirs[p] = true
		}
		return "", 0
	case strings.HasPrefix(cmd, "test -f "):
		// Format: test -f '<p>' && sha256sum '<p>' || echo MISSING
		// (single-quote escape, see delivery.go::remoteSha256).
		fields := strings.Fields(cmd)
		if len(fields) < 3 {
			return "", 1
		}
		p := strings.Trim(fields[2], "'")
		data, ok := fs.files[p]
		if !ok {
			return "MISSING\n", 0
		}
		sum := sha256.Sum256(data)
		return fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), p), 0
	case strings.HasPrefix(cmd, "set -e; cat > "):
		rest := strings.TrimPrefix(cmd, "set -e; cat > ")
		parts := strings.SplitN(rest, " ", 2)
		if len(parts) < 1 {
			return "", 1
		}
		p := parts[0]
		parent := filepath.Dir(p)
		if !fs.dirs[parent] {
			return "", 1
		}
		// Read stdin to EOF — matches `cat` behavior.
		buf, err := io.ReadAll(stdin)
		if err != nil {
			return "", 1
		}
		fs.files[p] = buf
		return "", 0
	case strings.HasPrefix(cmd, "rm -rf"):
		for _, p := range strings.Fields(strings.TrimPrefix(cmd, "rm -rf")) {
			delete(fs.dirs, p)
			for k := range fs.files {
				if k == p || strings.HasPrefix(k, p+"/") {
					delete(fs.files, k)
				}
			}
		}
		return "", 0
	default:
		return "", 127
	}
}

// liveShellTarget — an SSH-server handler that calls liveFs.handle for every
// exec command. The session channel accepts stdin for the cat command.
func liveShellTarget(fs *liveFs) func(t *testing.T, sc *ssh.ServerConn, chans <-chan ssh.NewChannel, reqs <-chan *ssh.Request) {
	return func(t *testing.T, _ *ssh.ServerConn, chans <-chan ssh.NewChannel, reqs <-chan *ssh.Request) {
		go ssh.DiscardRequests(reqs)
		for newCh := range chans {
			if newCh.ChannelType() != "session" {
				_ = newCh.Reject(ssh.UnknownChannelType, "только session")
				continue
			}
			ch, chReqs, err := newCh.Accept()
			if err != nil {
				continue
			}
			go func(ch ssh.Channel, chReqs <-chan *ssh.Request) {
				defer ch.Close()
				var execCmd string
				execReady := make(chan struct{}, 1)
				go func() {
					for req := range chReqs {
						if req.Type == "exec" {
							execCmd = parseExecPayload(req.Payload)
							if req.WantReply {
								_ = req.Reply(true, nil)
							}
							execReady <- struct{}{}
						} else if req.WantReply {
							_ = req.Reply(false, nil)
						}
					}
				}()
				select {
				case <-execReady:
				case <-time.After(5 * time.Second):
					return
				}
				stdout, exit := fs.handle(t, execCmd, ch)
				_, _ = ch.Write([]byte(stdout))
				_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{exit}))
			}(ch, chReqs)
		}
	}
}

// TestDeliverThenRunThenCleanup_LiveSSH — end-to-end over in-process SSH:
// bring up a target, ShaDeliverer places files via ssh exec, ShaCleaner
// removes them. Proves our exec commands are compatible with a real sshd
// channel (without a separate sftp/scp dependency).
func TestDeliverThenCleanup_LiveSSH(t *testing.T) {
	caSigner, caPub := testCAKey(t)
	fs := newLiveFs(t)

	target := newLiveSSHServer(t, caSigner, "127.0.0.1", nil)
	target.handleConn = liveShellTarget(fs)
	defer target.close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sess, err := Dial(ctx, DialConfig{
		Host:            target.host,
		Port:            target.port,
		User:            "soul",
		Auth:            userAuthForLiveTests(t, caSigner),
		HostAuthorities: []NamedHostKeyAuthority{{Name: "test-ca", CAPubKey: caPub}},
		Timeout:         3 * time.Second,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer sess.Close()

	// Prepare a local "soul binary" and "module".
	soulDir := t.TempDir()
	soulPath := filepath.Join(soulDir, "soul")
	if err := os.WriteFile(soulPath, []byte("SOUL-BIN-LIVE"), 0o755); err != nil {
		t.Fatalf("write soul: %v", err)
	}
	modPath := filepath.Join(soulDir, "soul-mod-pkg")
	if err := os.WriteFile(modPath, []byte("MOD-PKG-LIVE"), 0o755); err != nil {
		t.Fatalf("write mod: %v", err)
	}

	d := NewShaDeliverer()
	if err := d.Deliver(ctx, sess, SoulSpec{
		SoulBinaryPath: soulPath,
		Modules:        []ModuleSpec{{Name: "soul-mod-pkg", Path: modPath}},
	}); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if got, ok := fs.files[hostSoulDir+"/"+hostSoulFile]; !ok || string(got) != "SOUL-BIN-LIVE" {
		t.Fatalf("soul-бинарь не доехал, got %q ok=%v", got, ok)
	}
	if got, ok := fs.files[hostModulesDir+"/soul-mod-pkg"]; !ok || string(got) != "MOD-PKG-LIVE" {
		t.Fatalf("модуль не доехал, got %q ok=%v", got, ok)
	}

	// Repeat Deliver — must be idempotent (sha256 will match).
	beforeFiles := len(fs.files)
	if err := d.Deliver(ctx, sess, SoulSpec{
		SoulBinaryPath: soulPath,
		Modules:        []ModuleSpec{{Name: "soul-mod-pkg", Path: modPath}},
	}); err != nil {
		t.Fatalf("Deliver (повтор): %v", err)
	}
	if len(fs.files) != beforeFiles {
		t.Errorf("повторный Deliver добавил файлы — нет идемпотентности")
	}

	c := NewShaCleaner()
	if err := c.Cleanup(ctx, sess); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if _, ok := fs.files[hostSoulDir+"/"+hostSoulFile]; ok {
		t.Error("после Cleanup soul-бинарь остался")
	}
	if _, ok := fs.files[hostModulesDir+"/soul-mod-pkg"]; ok {
		t.Error("после Cleanup модуль остался")
	}
}
