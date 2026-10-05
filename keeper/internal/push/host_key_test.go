package push

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apissh "github.com/gravitational/teleport/api/ssh"
	"golang.org/x/crypto/ssh"
)

// A host presenting a certificate from a CA we do not trust must surface as a
// HostKeyError through a REAL handshake: the join wait in core.ssh retries
// anything else, and a refused key retried for fifteen minutes reaches the
// scenario as a timeout (NIM-905). The callback-level tests above prove the
// refusal; this proves the type survives x/crypto's handshake wrapping.
func TestDial_ForeignHostCA_IsHostKeyError(t *testing.T) {
	hostCA, _ := testCAKey(t)
	_, trustedCA := testCAKey(t)

	target := newLiveSSHServer(t, hostCA, "127.0.0.1", nil)
	target.handleConn = targetHandle(target)
	defer target.close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := Dial(ctx, DialConfig{
		Host:            target.host,
		Port:            target.port,
		User:            "soul",
		Auth:            userAuthForLiveTests(t, hostCA),
		HostAuthorities: []NamedHostKeyAuthority{{Name: "trusted", CAPubKey: trustedCA}},
		Timeout:         3 * time.Second,
	})
	if err == nil {
		t.Fatal("Dial accepted a host certificate from an untrusted CA")
	}
	if !IsHostKeyError(err) {
		t.Fatalf("a refused host certificate is not a HostKeyError: %v", err)
	}
}

// A connect failure is NOT a HostKeyError — the classification would otherwise
// stop the join wait on the one failure it exists to wait out.
func TestDial_ConnectRefused_IsNotHostKeyError(t *testing.T) {
	_, caPub := testCAKey(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := Dial(ctx, DialConfig{
		Host:            "127.0.0.1",
		Port:            1, // nothing listens on port 1
		User:            "soul",
		HostAuthorities: []NamedHostKeyAuthority{{Name: "ca", CAPubKey: caPub}},
		Timeout:         time.Second,
	})
	if err == nil {
		t.Fatal("Dial to a closed port succeeded")
	}
	if IsHostKeyError(err) {
		t.Fatalf("a refused TCP connect was classified as a host-key refusal: %v", err)
	}
}

func TestTagHostKeyCallback_NilStaysNil(t *testing.T) {
	if tagHostKeyCallback(nil) != nil {
		t.Fatal("a nil callback became non-nil: it would either panic in the key exchange or accept every host")
	}
}

func TestTagTeleportHostKey(t *testing.T) {
	t.Run("no callback is a HostKeyError", func(t *testing.T) {
		_, err := tagTeleportHostKey(apissh.ClientConfig{})
		if !IsHostKeyError(err) {
			t.Fatalf("an identity without an SSH CA must be refused as a HostKeyError, got %v", err)
		}
	})
	t.Run("a refusal from the callback is a HostKeyError", func(t *testing.T) {
		refused := errors.New("key not signed by the cluster CA")
		cfg, err := tagTeleportHostKey(apissh.ClientConfig{
			HostKeyCallback: func(string, net.Addr, ssh.PublicKey) error { return refused },
		})
		if err != nil {
			t.Fatalf("a configured callback was refused: %v", err)
		}
		cbErr := cfg.HostKeyCallback("node-1:22", nil, nil)
		if !IsHostKeyError(cbErr) || !errors.Is(cbErr, refused) {
			t.Fatalf("callback refusal = %v, want a HostKeyError wrapping the original", cbErr)
		}
	})
}

// An identity file with no SSH CA can verify no host, so every dial through it
// fails at the handshake. The daemon must refuse it at startup.
func TestNewTeleportDialer_RefusesIdentityWithoutHostCA(t *testing.T) {
	d, err := NewTeleportDialer(TeleportDialerConfig{
		ProxyAddr:    "proxy.example.com:443",
		IdentityFile: writeIdentityFile(t, false),
		Cluster:      "c1",
	})
	if err == nil {
		t.Fatal("an identity without an SSH CA passed the preflight")
	}
	if !errors.Is(err, errNoHostKeyCallback) {
		t.Fatalf("refusal does not name the missing host CA: %v", err)
	}
	if d != nil {
		t.Error("dialer must be nil on a preflight refusal")
	}
}

// The identity file is re-read on every dial, so one reissued WITHOUT an SSH CA
// after a clean start must fail the dial at once — before the proxy is asked
// anything, or the join wait retries DialHost's "not joined yet" until its budget
// runs out and the refusal arrives as a timeout.
//
// Mutation: drop the nil-callback check after creds.SSHClientConfig() in the dial
// closure and this reddens — the dial goes on to the (unreachable) proxy.
func TestTeleportDial_IdentityReissuedWithoutHostCA_IsRefusedAtOnce(t *testing.T) {
	path := writeIdentityFile(t, true)
	dial, err := NewTeleportDialer(TeleportDialerConfig{
		ProxyAddr:    "127.0.0.1:1",
		IdentityFile: path,
		Cluster:      "c1",
	})
	if err != nil {
		t.Fatalf("valid identity refused: %v", err)
	}
	reissued, err := os.ReadFile(writeIdentityFile(t, false))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, reissued, 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = dial(ctx, DialConfig{Host: "node-1", Port: 22, User: "root", Timeout: time.Second})
	if !IsHostKeyError(err) {
		t.Fatalf("dial through an identity with no SSH CA = %v, want a HostKeyError before any proxy call", err)
	}
}

// No production source in the push transport or in core.ssh may name
// ssh.InsecureIgnoreHostKey. The runtime refusal of an empty CA set covers one
// path; this covers the next one someone writes.
func TestNoInsecureIgnoreHostKeyInPushOrCoreSSH(t *testing.T) {
	for _, dir := range []string{".", filepath.Join("..", "coremod", "ssh")} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		checked := 0
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(dir, name)
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			checked++
			ast.Inspect(file, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.Ident:
					if x.Name == "InsecureIgnoreHostKey" {
						t.Errorf("%s names InsecureIgnoreHostKey — a host must be verified against a CA, never accepted blind", path)
					}
				case *ast.KeyValueExpr:
					if key, ok := x.Key.(*ast.Ident); ok && key.Name == "HostKeyCallback" {
						checkHostKeyCallbackSource(t, path, x.Value)
					}
				case *ast.AssignStmt:
					for i, lhs := range x.Lhs {
						if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "HostKeyCallback" && i < len(x.Rhs) {
							checkHostKeyCallbackSource(t, path, x.Rhs[i])
						}
					}
				}
				return true
			})
		}
		if checked == 0 {
			t.Fatalf("no Go sources found in %s — the guard is checking nothing", dir)
		}
	}
}

// checkHostKeyCallbackSource holds every HostKeyCallback set in these packages to
// the two constructors that verify and tag: an inline `func(...) error { return nil }`
// is the blind connect a search for InsecureIgnoreHostKey never finds.
func checkHostKeyCallbackSource(t *testing.T, path string, v ast.Expr) {
	t.Helper()
	if call, ok := v.(*ast.CallExpr); ok {
		if fn, ok := call.Fun.(*ast.Ident); ok && (fn.Name == "hostCertCallback" || fn.Name == "tagHostKeyCallback") {
			return
		}
	}
	t.Errorf("%s sets HostKeyCallback from something other than hostCertCallback/tagHostKeyCallback — a callback built in place can accept every host", path)
}
