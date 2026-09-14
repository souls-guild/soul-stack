package push

import (
	"context"
	"path"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/config"
)

// Delivery and exec are two halves of one run and they named the path
// separately: delivery wrote `/var/lib/soul-stack/bin/soul`, the exec default
// was `/usr/local/bin/soul`. Every unit test set SoulPath by hand, so nothing
// compared them, and a run that delivered its binary correctly still died with
// "command not found" (NIM-869).

// TestHostSoulBinaryPathIsTheDocumentedLayout pins the delivered path to the
// literal docs/keeper/push.md publishes to operators. It is the host-filesystem
// contract `soul_path` defaults to, so moving it is a PM decision — this is the
// line that says so.
func TestHostSoulBinaryPathIsTheDocumentedLayout(t *testing.T) {
	if HostSoulBinaryPath != "/var/lib/soul-stack/bin/soul" {
		t.Fatalf("HostSoulBinaryPath = %q; docs/keeper/push.md publishes /var/lib/soul-stack/bin/soul",
			HostSoulBinaryPath)
	}
	// Two independent sources since the constant moved to the wire package: what
	// Deliver composes from hostSoulDir/hostSoulFile, and what the CLI flag and
	// the OpenAPI field description read. They are only equal because of this.
	if got := path.Join(hostSoulDir, hostSoulFile); got != HostSoulBinaryPath {
		t.Fatalf("Deliver writes %q, the published constant says %q", got, HostSoulBinaryPath)
	}
}

// TestDefaultSoulPathIsTheDeliveredPath — the exec side is the half that moves.
// BOTH resolvers must default to where delivery put the binary; a divergence is
// invisible to every other test, because every other test supplies its own
// SoulPath. Asserted through Resolve rather than on the constant: the constant
// is defined as HostSoulBinaryPath and cannot disagree with itself, while a
// resolver that stops applying the default can.
func TestDefaultSoulPathIsTheDeliveredPath(t *testing.T) {
	cfgTarget, err := NewConfigTargetResolver([]config.KeeperPushTarget{
		{SID: "soul-a.example.com"},
	}).Resolve(context.Background(), "soul-a.example.com")
	if err != nil {
		t.Fatalf("ConfigTargetResolver.Resolve: %v", err)
	}
	if cfgTarget.SoulPath != HostSoulBinaryPath {
		t.Errorf("keeper.yml target with no soul_path resolved to %q, want %q",
			cfgTarget.SoulPath, HostSoulBinaryPath)
	}

	pgResolver := &PGFallbackTargetResolver{Reader: &pairingReader{target: &soul.SSHTarget{}}}
	pgTarget, err := pgResolver.Resolve(context.Background(), "soul-a.example.com")
	if err != nil {
		t.Fatalf("PGFallbackTargetResolver.Resolve: %v", err)
	}
	if pgTarget.SoulPath != HostSoulBinaryPath {
		t.Errorf("souls.ssh_target with no soul_path resolved to %q, want %q",
			pgTarget.SoulPath, HostSoulBinaryPath)
	}
}

// TestSendApply_ExecsWhatItJustDelivered is the pairing itself, end to end
// through the dispatcher with a defaulted target: the command that runs is the
// path the Deliverer reports it wrote to. The reported path is computed from
// the delivery constants rather than observed over ssh — a real ShaDeliverer
// against a real host is the live suite's job ([pushorch]) — so what this
// catches is the EXEC side drifting away from them, which is the half that
// drifted.
func TestSendApply_ExecsWhatItJustDelivered(t *testing.T) {
	sess := &mockSession{stdout: successStdout(t, "ap-pair")}
	deliverer := &recordingDeliverer{}
	disp := newTestDispatcher(t, Deps{
		Providers: map[string]ProviderEntry{testProviderName: {Provider: &mockProvider{authAllowed: true, signReply: validSignReply(t)}}},
		// The real resolver with no soul_path set: the exec path is whatever
		// production would default to, not a literal this test chose.
		Targets:   NewConfigTargetResolver([]config.KeeperPushTarget{{SID: "host-1.example.com"}}),
		Souls:     &mockSouls{s: sshSoul()},
		Deliverer: deliverer,
		SoulSpec:  SoulSpec{SoulBinaryPath: "/keeper/artifacts/soul"},
		Dial: func(_ context.Context, _ DialConfig) (Session, error) {
			return sess, nil
		},
	})

	if _, err := disp.SendApply(context.Background(), "host-1.example.com", testProviderName,
		&keeperv1.ApplyRequest{ApplyId: "ap-pair"}); err != nil {
		t.Fatalf("SendApply: %v", err)
	}

	if deliverer.gotSpec.SoulBinaryPath == "" {
		t.Fatal("Deliver was called with an empty SoulBinaryPath — the spec never reached the dispatcher")
	}
	if !strings.HasPrefix(sess.gotCmd, deliverer.wroteTo+" ") {
		t.Errorf("delivered to %q but ran %q — the two halves name different paths",
			deliverer.wroteTo, sess.gotCmd)
	}
}

// recordingDeliverer reports where [ShaDeliverer] would put the binary for the
// spec it was handed, without an ssh session to do it over.
type recordingDeliverer struct {
	gotSpec SoulSpec
	wroteTo string
}

func (d *recordingDeliverer) Deliver(_ context.Context, _ Session, spec SoulSpec) error {
	d.gotSpec = spec
	d.wroteTo = path.Join(hostSoulDir, hostSoulFile)
	return nil
}

// pairingReader serves one souls.ssh_target row to PGFallbackTargetResolver.
type pairingReader struct{ target *soul.SSHTarget }

func (r *pairingReader) SelectSshTarget(_ context.Context, _ string) (*soul.SSHTarget, error) {
	return r.target, nil
}
