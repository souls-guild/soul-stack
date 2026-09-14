//go:build libvirt

// The live lane for NIM-872: a real machine, a real boot, a real sshd that is
// not listening yet.
//
// The unit guards beside this file prove the retry is CALLED and that it stops
// where it should. They cannot prove it SAVES anything: every failure they hand
// the dialer is a value a test wrote, so a classifier reading a shape the live
// path never produces would keep them all green. What only a machine can answer:
// that `connection refused` from a booting guest really arrives here as a
// `*net.OpError` with `Op == "dial"` through push.Dial's `%w`, and that waiting
// through it ends with commands running on that guest.
//
// Outside the default build on purpose — it needs a hypervisor, a staged image
// and a few minutes. Run it deliberately:
//
//	sg libvirt -c 'go test -tags libvirt -timeout 20m -v -run TestLive ./internal/coremod/ssh/'
//
// It skips, naming the missing thing, rather than failing obscurely when a
// prerequisite is absent.
//
// ★ The VM is created through the `vmlocal` artifact itself, not through libvirt
// directly, because the readiness predicate is the subject: vmlocal returns a
// host when the guest has taken a DHCP lease and announced a name, which is the
// cloud's own predicate and is satisfied well before sshd starts. Reproducing the
// domain by hand would reproduce the machine and lose the race that this ticket
// is about.
package ssh

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/push"
	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"golang.org/x/crypto/ssh"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"
)

const (
	liveNamespace = "nim872-live"
	liveBatch     = "nim872"
	liveUser      = "soul"
	// liveMarker is written by the first step and read back by the second. A
	// step that reaches a host and exits non-zero fails the whole run, so the
	// read-back is the assertion: it proves the commands ran ON THE GUEST rather
	// than that a session object was returned.
	liveMarker = "/tmp/nim872-was-here"
	// liveSshdHoldSeconds is how long the guest keeps sshd masked after cloud-init
	// reaches runcmd.
	//
	// ★ The hold is here because the natural window is not reproducible, and a
	// test that only sometimes exercises its subject is not a test. Measured on
	// this host with no hold at all: vmlocal reported the lease 15s after the
	// request and the FIRST dial connected — on a Debian-12 cloud image on a
	// workstation, sshd is already listening when the lease lands. The race the
	// ticket was filed from is real (it was found by a live run) but its width
	// depends on the image, the hypervisor's load and whether the lease was a
	// fresh one; none of that is a thing a test can hold still.
	//
	// So what this lane pins down is the half that IS the engine's: given a real
	// host that is not yet serving ssh, the connect waits instead of failing the
	// run. The machine, the kernel's refusal and the sshd that finally answers
	// are all real — only the moment it answers is made deterministic.
	liveSshdHoldSeconds = 45
)

// --- the guest's credentials ------------------------------------------------

// liveIdentity is everything the two ends need to trust each other, generated
// per run: a host CA whose public half the Keeper pins, a host key and a
// certificate for the guest, and a user key whose public half rides in the
// guest's authorized_keys while the SshProvider hands back the private half.
//
// ★ The host certificate is signed with NO principals. For a host cert that
// means "valid for any host name", which is what a DHCP address needs —
// ssh.CertChecker matches principals against the address actually dialed, and
// the address is not known when the certificate is made.
type liveIdentity struct {
	caPub       ssh.PublicKey
	hostKeyPEM  string
	hostPubText string
	hostCert    string
	userKeyPEM  string
	userPubText string
}

func newLiveIdentity(t *testing.T) liveIdentity {
	t.Helper()

	caPub, caPriv := liveKeypair(t)
	caSigner, err := ssh.NewSignerFromKey(caPriv)
	if err != nil {
		t.Fatalf("ca signer: %v", err)
	}
	caSSHPub, err := ssh.NewPublicKey(caPub)
	if err != nil {
		t.Fatalf("ca public key: %v", err)
	}

	hostPub, hostPriv := liveKeypair(t)
	hostSSHPub, err := ssh.NewPublicKey(hostPub)
	if err != nil {
		t.Fatalf("host public key: %v", err)
	}
	cert := &ssh.Certificate{
		Key:             hostSSHPub,
		CertType:        ssh.HostCert,
		KeyId:           "nim872-live",
		ValidPrincipals: nil,
		ValidAfter:      uint64(time.Now().Add(-5 * time.Minute).Unix()),
		ValidBefore:     uint64(time.Now().Add(4 * time.Hour).Unix()),
	}
	if err := cert.SignCert(rand.Reader, caSigner); err != nil {
		t.Fatalf("sign host cert: %v", err)
	}

	userPub, userPriv := liveKeypair(t)
	userSSHPub, err := ssh.NewPublicKey(userPub)
	if err != nil {
		t.Fatalf("user public key: %v", err)
	}

	return liveIdentity{
		caPub:       caSSHPub,
		hostKeyPEM:  livePrivatePEM(t, hostPriv),
		hostPubText: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(hostSSHPub))),
		hostCert:    strings.TrimSpace(string(ssh.MarshalAuthorizedKey(cert))),
		userKeyPEM:  livePrivatePEM(t, userPriv),
		userPubText: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(userSSHPub))),
	}
}

func liveKeypair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}
	return pub, priv
}

func livePrivatePEM(t *testing.T, priv ed25519.PrivateKey) string {
	t.Helper()
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatalf("marshal private key: %v", err)
	}
	return string(pem.EncodeToMemory(block))
}

// userdata is the cloud-config the guest boots with: the host identity above,
// and one user to run the steps as.
func (id liveIdentity) userdata() string {
	var b strings.Builder
	b.WriteString("#cloud-config\n")
	b.WriteString("ssh_keys:\n")
	b.WriteString("  ed25519_private: |\n")
	for _, line := range strings.Split(strings.TrimRight(id.hostKeyPEM, "\n"), "\n") {
		b.WriteString("    " + line + "\n")
	}
	fmt.Fprintf(&b, "  ed25519_public: %q\n", id.hostPubText)
	fmt.Fprintf(&b, "  ed25519_certificate: %q\n", id.hostCert)
	b.WriteString("users:\n")
	fmt.Fprintf(&b, "  - name: %s\n", liveUser)
	b.WriteString("    shell: /bin/bash\n")
	b.WriteString("    sudo: \"ALL=(ALL) NOPASSWD:ALL\"\n")
	b.WriteString("    ssh_authorized_keys:\n")
	fmt.Fprintf(&b, "      - %q\n", id.userPubText)
	// Hold sshd down, then hand it back on a transient systemd timer. `bootcmd`
	// runs early enough that the port is already closed when the lease lands, and
	// `systemd-run --on-active` outlives cloud-init, which a backgrounded shell
	// in `runcmd` does not reliably do.
	b.WriteString("bootcmd:\n")
	b.WriteString("  - [ sh, -c, \"systemctl mask --now ssh.socket ssh.service >/dev/null 2>&1; true\" ]\n")
	b.WriteString("runcmd:\n")
	fmt.Fprintf(&b, "  - [ systemd-run, --on-active=%d, --unit=nim872-release-sshd, sh, -c,"+
		" \"systemctl unmask ssh.socket ssh.service; systemctl start ssh.socket || systemctl start ssh.service\" ]\n",
		liveSshdHoldSeconds)
	return b.String()
}

// liveStaticProvider is an SshProvider in static mode: no certificate, a private
// key whose public half is in the guest's authorized_keys. Exactly the shape
// `ssh-static` returns, and the reason this lane needs no Vault.
type liveStaticProvider struct {
	keyPEM string
	calls  struct {
		mu                sync.Mutex
		authorize, signed int
	}
}

func (p *liveStaticProvider) Authorize(context.Context, *pluginv1.AuthorizeRequest) (*pluginv1.AuthorizeReply, error) {
	p.calls.mu.Lock()
	p.calls.authorize++
	p.calls.mu.Unlock()
	return &pluginv1.AuthorizeReply{Allowed: true}, nil
}

func (p *liveStaticProvider) Sign(context.Context, *pluginv1.SignRequest) (*pluginv1.SignReply, error) {
	p.calls.mu.Lock()
	p.calls.signed++
	p.calls.mu.Unlock()
	return &pluginv1.SignReply{PrivateKey: p.keyPEM}, nil
}

func (p *liveStaticProvider) counts() (authorize, signed int) {
	p.calls.mu.Lock()
	defer p.calls.mu.Unlock()
	return p.calls.authorize, p.calls.signed
}

// --- the vmlocal artifact, spoken to as the Keeper speaks to it -------------

// liveVM is the plugin subprocess serving the `vm` object.
type liveVM struct {
	client pluginv1.SoulModuleClient
}

func liveRepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test file")
	}
	// keeper/internal/coremod/ssh/<file> → four levels up is the tree root.
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", ".."))
}

func liveBuildVmlocal(t *testing.T, root string) string {
	t.Helper()
	src := filepath.Join(root, "examples", "module", "vmlocal")
	if _, err := os.Stat(src); err != nil {
		t.Skipf("the vmlocal artifact is not in this tree (%s): %v", src, err)
	}
	out := filepath.Join(t.TempDir(), "vmlocal")
	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Dir = src
	// Its own live lane builds this way: the artifact is a separate module and
	// must resolve through its go.mod, not through the workspace.
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build vmlocal: %v\n%s", err, b)
	}
	return out
}

func liveSpawnVM(t *testing.T, bin string) *liveVM {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "vmlocal.sock")
	cmd := exec.Command(bin, "vm")
	cmd.Env = append(os.Environ(), "SOUL_PLUGIN_SOCKET="+sock)
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start vmlocal: %v", err)
	}

	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("read plugin handshake: %v", err)
	}
	var hs pluginv1.Handshake
	if err := protojson.Unmarshal([]byte(strings.TrimSpace(line)), &hs); err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("parse plugin handshake %q: %v", line, err)
	}
	conn, err := grpc.NewClient("unix://"+hs.GetAddress(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("dial plugin: %v", err)
	}
	// ★ Registered HERE rather than deferred by the caller. t.Cleanup is LIFO and
	// runs after the test's own defers, so a `defer vm.stop()` would tear the
	// plugin down before the cleanup that uses it to destroy the machine — which
	// leaks a running VM, and the next run then ADOPTS it and exercises no boot
	// at all. Registering first means running last.
	t.Cleanup(func() {
		_ = conn.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return &liveVM{client: pluginv1.NewSoulModuleClient(conn)}
}

// apply runs one action and returns the final event.
func (v *liveVM) apply(t *testing.T, state string, own map[string]any) *pluginv1.ApplyEvent {
	t.Helper()
	// Two connection params and no credential: a local libvirtd authenticates by
	// the permissions on its unix socket (NIM-873 removed the mirrored cloud's
	// key_id/secret, and the profile below is now a CLOSED set).
	p := map[string]any{
		"endpoint":  "qemu:///system",
		"namespace": liveNamespace,
	}
	for k, val := range own {
		p[k] = val
	}
	s, err := structpb.NewStruct(p)
	if err != nil {
		t.Fatalf("build vm params: %v", err)
	}
	stream, err := v.client.Apply(context.Background(), &pluginv1.ApplyRequest{State: state, Params: s})
	if err != nil {
		t.Fatalf("vm %s: %v", state, err)
	}
	var last *pluginv1.ApplyEvent
	for {
		ev, rerr := stream.Recv()
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			t.Fatalf("vm %s stream: %v", state, rerr)
		}
		t.Logf("[vm %s] %s", state, ev.GetMessage())
		last = ev
	}
	if last == nil {
		t.Fatalf("vm %s emitted no events", state)
	}
	return last
}

func liveProfile(t *testing.T) map[string]any {
	t.Helper()
	out, err := exec.Command("virsh", "-c", "qemu:///system", "net-uuid", "default").Output()
	if err != nil {
		t.Skipf("no libvirt network named `default` (is libvirtd up, and is this process in the libvirt group?): %v", err)
	}
	return map[string]any{
		"namespace":      liveNamespace,
		"image_name":     "debian-12",
		"network_id":     strings.TrimSpace(string(out)),
		"cpu_size":       float64(1),
		"ram_size":       float64(1 << 30),
		"boot_disk_size": float64(4 << 30),
	}
}

// --- the subject ------------------------------------------------------------

// ★ LIVE GUARD (NIM-872). A host that is reported ready is not a host you can
// reach. vmlocal reports it when the guest has taken a DHCP lease and announced a
// name — the cloud's own readiness predicate — and sshd answers later. Before
// this ticket the first refusal ended the run: `dialDirect` had no retry at all
// and `join_wait_timeout` was inert on this transport.
//
// The four assertions are the ones a mock dialer cannot reach:
//
//  1. more than one attempt was made — if the first connected, the hold did not
//     take and this run exercised no wait, so it fails rather than passing
//     vacuously;
//  2. the refusal carried `*net.OpError{Op: "dial"}` through push.Dial — the
//     shape isConnectFailure reads, taken from the live path instead of written
//     by a test. This is the one that would catch the classifier reading a
//     fiction;
//  3. the steps ran ON THE GUEST, read back by a second step that exits non-zero
//     if the first one did not;
//  4. Authorize and Sign were each called once across the whole wait — the
//     credential is minted before the loop and has to outlive it.
//
// Mutation: call m.Dial directly in dialDirect and this reddens on (1) with the
// exact error the ticket was filed from.
func TestLive_DirectWaitsForSshdOnAFreshVM(t *testing.T) {
	root := liveRepoRoot(t)
	profile := liveProfile(t)
	vm := liveSpawnVM(t, liveBuildVmlocal(t, root))

	id := newLiveIdentity(t)

	t.Cleanup(func() {
		probe := vm.apply(t, "probed", nil)
		var ids []any
		if raw, ok := probe.GetOutput().AsMap()["hosts"].([]any); ok {
			for _, e := range raw {
				if h, ok := e.(map[string]any); ok {
					ids = append(ids, h["vm_id"])
				}
			}
		}
		if len(ids) == 0 {
			return
		}
		vm.apply(t, "destroyed", map[string]any{"vm_ids": ids})
	})

	created := time.Now()
	ev := vm.apply(t, "created", map[string]any{
		"name":     liveBatch,
		"count":    float64(1),
		"profile":  profile,
		"userdata": id.userdata(),
	})
	if ev.GetFailed() {
		t.Fatalf("the machine was not created: %s", ev.GetMessage())
	}
	hosts, ok := ev.GetOutput().AsMap()["hosts"].([]any)
	if !ok || len(hosts) != 1 {
		t.Fatalf("created returned %v, want one host", ev.GetOutput().AsMap()["hosts"])
	}
	host, _ := hosts[0].(map[string]any)
	sid, _ := host["sid"].(string)
	ip, _ := host["primary_ip"].(string)
	if ip == "" {
		t.Fatal("the lease carried no address")
	}
	leaseAt := time.Now()
	t.Logf("★ vmlocal called %s (%s) ready %s after the request — this is the lease, not a booted machine",
		sid, ip, leaseAt.Sub(created).Round(time.Second))

	// The real dialer, wrapped only to record what the live path produces.
	var (
		mu       sync.Mutex
		attempts int
		errs     []error
	)
	prov := &liveStaticProvider{keyPEM: id.userKeyPEM}
	m := &Module{
		Providers: func() map[string]SshProviderHost {
			return map[string]SshProviderHost{"static": prov}
		},
		HostCAs: func() []push.NamedHostKeyAuthority {
			return []push.NamedHostKeyAuthority{{Name: "nim872-live", CAPubKey: id.caPub}}
		},
		// RetryBase/RetryJitter are deliberately left unset: this lane runs the
		// production backoff, because how long the wait actually takes against a
		// booting Debian is one of the things it is here to show.
		Dial: func(ctx context.Context, cfg push.DialConfig) (push.Session, error) {
			sess, err := push.Dial(ctx, cfg)
			mu.Lock()
			attempts++
			n := attempts
			if err != nil {
				errs = append(errs, err)
			}
			mu.Unlock()
			if err != nil {
				t.Logf("dial attempt %d at +%s: %v", n, time.Since(leaseAt).Round(time.Second), err)
			} else {
				t.Logf("★ dial attempt %d at +%s: connected", n, time.Since(leaseAt).Round(time.Second))
			}
			return sess, err
		},
	}

	st := apply(t, m, params(t, map[string]any{
		"ssh_provider": "static",
		"ssh_user":     liveUser,
		"hosts":        []any{map[string]any{"sid": sid, "primary_ip": ip}},
		"steps": []any{
			map[string]any{"run": "touch " + liveMarker},
			map[string]any{"run": "test -f " + liveMarker},
		},
		"join_wait_timeout": "10m",
	}))
	mustSucceed(t, st)
	t.Logf("★ the step completed %s after the lease, over %d dial attempt(s)",
		time.Since(leaseAt).Round(time.Second), attempts)

	mu.Lock()
	defer mu.Unlock()

	if attempts < 2 {
		t.Fatalf("the first dial succeeded — sshd was listening when the host was reported ready, so this run "+
			"exercised no wait at all and proves nothing about NIM-872 (attempts=%d). Check that the guest "+
			"honoured the bootcmd that masks ssh", attempts)
	}

	var refused *net.OpError
	for _, err := range errs {
		var opErr *net.OpError
		if errors.As(err, &opErr) && opErr.Op == "dial" {
			refused = opErr
			break
		}
	}
	if refused == nil {
		t.Fatalf("no attempt failed with a *net.OpError{Op: \"dial\"} — the shape isConnectFailure reads is "+
			"NOT what the live path produces, and the unit guards are asserting a fiction; got %v", errs)
	}
	t.Logf("★ the live refusal is %T{Op:%q}: %v — the shape the classifier reads", refused, refused.Op, refused)

	if authorize, signed := prov.counts(); authorize != 1 || signed != 1 {
		t.Errorf("authorize=%d sign=%d across %d dial attempts, want 1 each — the credential is minted once "+
			"and must outlive the wait", authorize, signed, attempts)
	}
}
