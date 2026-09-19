//go:build integration

// The live push run: a real Postgres, a real sshd on a real OS, the real
// `soul` binary built from this tree, and the production wiring above them —
// topology → render → SshDispatcher → ShaDeliverer → `soul apply` → NDJSON
// RunResult → push_runs.
//
// WHAT IT REPLACES. This file held a `t.Skip` skeleton from the S6 pilot for a
// year. Nothing else covered the path, so three independent refusals lived in
// it undisturbed (NIM-869): the daemon wired a Deliverer with no SoulSpec,
// delivery wrote `/var/lib/soul-stack/bin/soul` while the exec default was
// `/usr/local/bin/soul`, and the topology presence filter dropped every
// transport=ssh host because it has no EventStream lease and never reaches
// `connected`. Every one of them is upstream of the first line of an
// ApplyRequest, and every one of them was invisible to the unit suites, which
// supply their own SoulSpec, their own SoulPath and their own roster.
//
// WHY IT CANNOT GO GREEN WITHOUT A HOST. The assertions are the host's own
// state: a file the destiny created, read back through the container rather
// than through the run's report, and the sha256 of the delivered binary as the
// host computes it. A run that never connected produces neither.
//
// Run:
//
//	cd keeper && go test -tags=integration -count=1 ./internal/pushorch/...

package pushorch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/integrationenv"
	"github.com/souls-guild/soul-stack/keeper/internal/push"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/keeper/internal/sshdtest"
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

// proofPath is what the destiny writes on the host. Read back out-of-band, it
// is the one assertion no amount of keeper-side bookkeeping can fake.
const proofPath = "/tmp/nim869-push-reached-the-host"

const liveProviderName = "live-ssh"

// The two deliberately-wrong seeds of the NIM-870 acceptance. Both are wrong in
// a way that fails the run on its own, so a green run is evidence the task's
// key overrode them and not evidence that neither mattered.
const (
	// wrongRegistryUser goes into `souls.ssh_target.ssh_user`. sshd does not
	// admit it, and the provider signs for the real user, so a dial that took
	// this one presents a certificate for a different principal.
	wrongRegistryUser = "not-the-user-sshd-admits"
	// unregisteredProviderName is what the router answers. It is absent from
	// the dispatcher's Providers map → push.ErrProviderUnknown before connect.
	unregisteredProviderName = "router-provider-that-is-not-registered"
)

// TestIntegration_PushRun_LiveSSHD_ReachesRunResultOnARealHost is the
// acceptance for NIM-869.
func TestIntegration_PushRun_LiveSSHD_ReachesRunResultOnARealHost(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires docker")
	}
	ctx, cancel := integrationenv.SetupContextFor(10 * time.Minute)
	defer cancel()

	soulBinary := buildSoulBinary(t)

	ca := sshdtest.NewCA(t)
	host := sshdtest.Start(ctx, t, ca, sshdtest.Options{})
	sshdtest.AssertHostCertPrincipal(t, host.Addr)

	// The SID is the address, because PGFallbackTargetResolver resolves
	// SSHTarget.Host from the SID. That is a real constraint on push today, not
	// a test shortcut — see the ticket comment on NIM-869.
	sid := host.Addr
	seedPushHost(t, sid, host.Port, host.User)

	// Delivery comes from the production decision over a keeper.yml block, so
	// the empty-SoulSpec bug cannot come back through a path this test bypasses.
	deliverer, soulSpec, err := push.DeliveryFromConfig(&config.KeeperPush{SoulBinaryPath: soulBinary})
	if err != nil {
		t.Fatalf("DeliveryFromConfig: %v", err)
	}
	if deliverer == nil {
		t.Fatal("DeliveryFromConfig returned no Deliverer for a configured binary")
	}

	dispatcher, err := push.NewSshDispatcher(push.Deps{
		Providers: map[string]push.ProviderEntry{
			liveProviderName: {Provider: &sshdtest.Provider{CA: ca, User: host.User}},
		},
		// The production resolvers, with no soul_path in the row: the exec path
		// is whatever the default resolves to, which is the half NIM-869 moved.
		Targets:         &push.PGFallbackTargetResolver{Reader: push.NewPGTargetReader(integrationPool)},
		Souls:           push.NewPGSoulLookup(integrationPool),
		HostAuthorities: []push.NamedHostKeyAuthority{{Name: "test-ca", CAPubKey: ca.Pub}},
		Deliverer:       deliverer,
		SoulSpec:        soulSpec,
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		DialTimeout:     20 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewSshDispatcher: %v", err)
	}

	engine, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}

	// lease=nil is the single-instance shape, and it is the arm that used to
	// keep only `status='connected'`. The seeded host is `pending`.
	runner, err := NewPushRun(Deps{
		Store:         NewStore(integrationPool),
		Topology:      topology.NewResolver(integrationPool, nil, nil),
		Render:        render.NewPipeline(nil, engine, nil, nil),
		DestinyLoader: &liveDestinyLoader{dir: t.TempDir()},
		Template:      liveDestinyTemplate{},
		Dispatcher:    dispatcher,
		Router:        fixedRouter(liveProviderName),
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		KID:           "keeper-live-test",
	})
	if err != nil {
		t.Fatalf("NewPushRun: %v", err)
	}

	applyID, err := runner.Apply(ctx, ApplyRequest{
		InventorySIDs: []string{sid},
		DestinyRef:    "push-proof@v1",
		StartedByAID:  "archon-live",
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	row := awaitTerminal(ctx, t, runner, applyID)
	if row.Status != StatusSuccess {
		t.Fatalf("push run finished %s, want success; summary=%s", row.Status, mustJSON(t, row.Summary))
	}

	// The host's own state, read out-of-band. The run's report says the run
	// succeeded; this says the machine changed.
	got, err := host.Exec(ctx, "cat "+proofPath)
	if err != nil {
		t.Fatalf("the destiny's file is not on the host: %v (summary=%s)", err, mustJSON(t, row.Summary))
	}
	if strings.TrimSpace(got) != "reached" {
		t.Errorf("%s = %q, want \"reached\"", proofPath, strings.TrimSpace(got))
	}

	// Delivery: the binary the run exec'd is the one the keeper shipped, at the
	// contract path, byte for byte.
	out, err := host.Exec(ctx, "sha256sum "+push.HostSoulBinaryPath)
	if err != nil {
		t.Fatalf("no soul binary at the delivery path %s: %v", push.HostSoulBinaryPath, err)
	}
	if want := fileSha256(t, soulBinary); !strings.HasPrefix(strings.TrimSpace(out), want) {
		t.Errorf("delivered binary sha = %q, local = %q", strings.TrimSpace(out), want)
	}
}

// TestIntegration_PushRun_LiveSSHD_NoDeliveryIsNotSilentSuccess — the other
// half of the delivery decision. With `push.soul_binary_path` unset there is
// nothing at the target's soul_path on a bare host, and the run must come back
// failed rather than succeed having done nothing.
func TestIntegration_PushRun_LiveSSHD_NoDeliveryIsNotSilentSuccess(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires docker")
	}
	ctx, cancel := integrationenv.SetupContextFor(10 * time.Minute)
	defer cancel()

	ca := sshdtest.NewCA(t)
	host := sshdtest.Start(ctx, t, ca, sshdtest.Options{})
	sshdtest.AssertHostCertPrincipal(t, host.Addr)
	sid := host.Addr
	seedPushHost(t, sid, host.Port, host.User)

	deliverer, soulSpec, err := push.DeliveryFromConfig(&config.KeeperPush{})
	if err != nil {
		t.Fatalf("DeliveryFromConfig: %v", err)
	}

	dispatcher, err := push.NewSshDispatcher(push.Deps{
		Providers: map[string]push.ProviderEntry{
			liveProviderName: {Provider: &sshdtest.Provider{CA: ca, User: host.User}},
		},
		Targets:         &push.PGFallbackTargetResolver{Reader: push.NewPGTargetReader(integrationPool)},
		Souls:           push.NewPGSoulLookup(integrationPool),
		HostAuthorities: []push.NamedHostKeyAuthority{{Name: "test-ca", CAPubKey: ca.Pub}},
		Deliverer:       deliverer,
		SoulSpec:        soulSpec,
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		DialTimeout:     20 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewSshDispatcher: %v", err)
	}
	engine, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	runner, err := NewPushRun(Deps{
		Store:         NewStore(integrationPool),
		Topology:      topology.NewResolver(integrationPool, nil, nil),
		Render:        render.NewPipeline(nil, engine, nil, nil),
		DestinyLoader: &liveDestinyLoader{dir: t.TempDir()},
		Template:      liveDestinyTemplate{},
		Dispatcher:    dispatcher,
		Router:        fixedRouter(liveProviderName),
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		KID:           "keeper-live-test",
	})
	if err != nil {
		t.Fatalf("NewPushRun: %v", err)
	}

	applyID, err := runner.Apply(ctx, ApplyRequest{
		InventorySIDs: []string{sid},
		DestinyRef:    "push-proof@v1",
		StartedByAID:  "archon-live",
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	row := awaitTerminal(ctx, t, runner, applyID)
	if row.Status == StatusSuccess {
		t.Fatalf("a run with no binary on the host reported success: %s", mustJSON(t, row.Summary))
	}
	if _, err := host.Exec(ctx, "test -e "+proofPath); err == nil {
		t.Errorf("%s exists although nothing could have run", proofPath)
	}
}

// TestIntegration_PushRun_LiveSSHD_TaskTransportBeatsTheRegistry is the
// acceptance for NIM-870: the task's `transport:` beats `souls.ssh_target` and
// beats the router, and the effective values come back in the run summary.
//
// WHY IT CANNOT GO GREEN BY ACCIDENT. Both sources the task overrides are
// seeded WRONG on purpose, and each is wrong in a way that kills the run on its
// own:
//
//   - the registry row carries a user sshd will not admit, and the provider
//     signs a certificate whose principal is the real one — so a dial that took
//     the registry's user presents a cert for somebody else and is refused at
//     the handshake;
//   - the router answers with a provider name that is not in the dispatcher's
//     map, so a route that consulted it dies ErrProviderUnknown before connect.
//
// A green run therefore proves the task won BOTH, and the file the destiny
// writes proves it won them against a real host rather than in a mock.
func TestIntegration_PushRun_LiveSSHD_TaskTransportBeatsTheRegistry(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires docker")
	}
	ctx, cancel := integrationenv.SetupContextFor(10 * time.Minute)
	defer cancel()

	soulBinary := buildSoulBinary(t)

	ca := sshdtest.NewCA(t)
	host := sshdtest.Start(ctx, t, ca, sshdtest.Options{})
	sshdtest.AssertHostCertPrincipal(t, host.Addr)

	sid := host.Addr
	seedPushHost(t, sid, host.Port, wrongRegistryUser)

	deliverer, soulSpec, err := push.DeliveryFromConfig(&config.KeeperPush{SoulBinaryPath: soulBinary})
	if err != nil {
		t.Fatalf("DeliveryFromConfig: %v", err)
	}

	dispatcher, err := push.NewSshDispatcher(push.Deps{
		Providers: map[string]push.ProviderEntry{
			liveProviderName: {Provider: &sshdtest.Provider{CA: ca, User: host.User}},
		},
		Targets:         &push.PGFallbackTargetResolver{Reader: push.NewPGTargetReader(integrationPool)},
		Souls:           push.NewPGSoulLookup(integrationPool),
		HostAuthorities: []push.NamedHostKeyAuthority{{Name: "test-ca", CAPubKey: ca.Pub}},
		Deliverer:       deliverer,
		SoulSpec:        soulSpec,
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		DialTimeout:     20 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewSshDispatcher: %v", err)
	}

	engine, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	runner, err := NewPushRun(Deps{
		Store:         NewStore(integrationPool),
		Topology:      topology.NewResolver(integrationPool, nil, nil),
		Render:        render.NewPipeline(nil, engine, nil, nil),
		DestinyLoader: &liveDestinyLoader{dir: t.TempDir()},
		Template:      liveDestinyTemplate{},
		Dispatcher:    dispatcher,
		Router:        fixedRouter(unregisteredProviderName),
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		KID:           "keeper-live-test",
	})
	if err != nil {
		t.Fatalf("NewPushRun: %v", err)
	}

	// The key exactly as a scenario writes it — the raw DSL value, decoded by
	// the same config.TransportSpecOf the linter validated it with.
	applyID, err := runner.Apply(ctx, ApplyRequest{
		InventorySIDs: []string{sid},
		DestinyRef:    "push-proof@v1",
		StartedByAID:  "archon-live",
		Transport: map[string]any{"ssh": map[string]any{
			"ssh_provider": liveProviderName,
			"user":         host.User,
			"port":         host.Port,
		}},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	row := awaitTerminal(ctx, t, runner, applyID)
	if row.Status != StatusSuccess {
		t.Fatalf("push run finished %s, want success; summary=%s", row.Status, mustJSON(t, row.Summary))
	}

	// The host's own state: the run reached the machine using what the TASK
	// said, since neither seeded source could have got there.
	got, err := host.Exec(ctx, "cat "+proofPath)
	if err != nil {
		t.Fatalf("the destiny's file is not on the host: %v (summary=%s)", err, mustJSON(t, row.Summary))
	}
	if strings.TrimSpace(got) != "reached" {
		t.Errorf("%s = %q, want \"reached\"", proofPath, strings.TrimSpace(got))
	}

	// ★ The other direction, on the same host: a key that is WRITTEN but does
	// not decode must be REFUSED, never quietly fall back to the registry the key
	// exists to beat. Since NIM-880 the refusal happens at Apply, synchronously,
	// so the caller gets it instead of a run row it has to poll — and no run is
	// started at all, which is why there is no terminal to inspect here. The
	// run-level gate behind it still stands and is covered where it lives
	// (render.stampTransport, pushorch.transportOverrideOf), for any path that
	// does not come through this door.
	badID, err := runner.Apply(ctx, ApplyRequest{
		InventorySIDs: []string{sid},
		DestinyRef:    "push-proof@v1",
		StartedByAID:  "archon-live",
		Transport:     map[string]any{"ssh": nil, "agent": nil},
	})
	if err == nil {
		t.Errorf("Apply accepted an undecodable transport and started run %s", badID)
	} else if !errors.Is(err, ErrInvalidTransport) {
		// The ERROR alone proves nothing: a broken fixture would also fail here
		// while this phase claims the key was refused. It has to be the key's
		// sentinel.
		t.Errorf("Apply refused with %v, want %v — the refusal must be the key's, not something else's", err, ErrInvalidTransport)
	}

	// The visibility half: the summary has to name the source that answered,
	// or the override is invisible to whoever reads this run afterwards.
	entry := summaryHostEntry(t, row.Summary, sid)
	for key, want := range map[string]any{
		"transport":    config.TransportSSH,
		"route_source": "task",
		"ssh_provider": liveProviderName,
		"ssh_user":     host.User,
	} {
		if entry[key] != want {
			t.Errorf("summary.hosts[%s].%s = %v, want %v (full=%s)", sid, key, entry[key], want, mustJSON(t, row.Summary))
		}
	}
}

// summaryHostEntry pulls one host's entry out of push_runs.summary. The row
// comes back through jsonb, so the shape is map[string]any all the way down —
// reading it the way an operator's GET /v1/push/{apply_id} does.
func summaryHostEntry(t *testing.T, summary map[string]any, sid string) map[string]any {
	t.Helper()
	hosts, ok := summary["hosts"].([]any)
	if !ok {
		t.Fatalf("summary.hosts is %T, want a list: %s", summary["hosts"], mustJSON(t, summary))
	}
	for _, h := range hosts {
		entry, isMap := h.(map[string]any)
		if isMap && entry["sid"] == sid {
			return entry
		}
	}
	t.Fatalf("no entry for %s in %s", sid, mustJSON(t, summary))
	return nil
}

// --- fixtures ---------------------------------------------------------

// seedPushHost inserts the registry row a push run needs and nothing more: a
// `pending` soul with transport=ssh — the ONLY status such a host ever holds,
// since `connected` is written by the agent's Bootstrap RPC — plus its
// ssh_target WITHOUT a soul_path, so the resolver's default decides the exec
// path.
func seedPushHost(t *testing.T, sid string, port int, user string) {
	t.Helper()
	ctx := context.Background()
	if _, err := integrationPool.Exec(ctx, `TRUNCATE TABLE push_runs, souls, operators CASCADE`); err != nil {
		t.Fatalf("TRUNCATE: %v", err)
	}
	if _, err := integrationPool.Exec(ctx, `
		INSERT INTO operators (aid, display_name, auth_method)
		VALUES ('archon-live', 'Live IT', 'jwt')`); err != nil {
		t.Fatalf("seed operator: %v", err)
	}
	if err := soul.Insert(ctx, integrationPool, &soul.Soul{
		SID:       sid,
		Transport: soul.TransportSSH,
		Status:    soul.StatusPending,
	}); err != nil {
		t.Fatalf("seed soul %s: %v", sid, err)
	}
	if err := soul.UpdateSshTarget(ctx, integrationPool, sid, &soul.SSHTarget{
		SSHPort: port,
		SSHUser: user,
	}); err != nil {
		t.Fatalf("seed ssh_target: %v", err)
	}
}

// buildSoulBinary compiles the agent from this tree. CGO off so the binary is
// static and runs on the alpine container's musl; GOOS pinned because a docker
// daemon on a mac or windows host still runs a linux container, and the failure
// of a wrong-platform binary arrives as a RunResult-less stream rather than as
// "wrong platform". GOARCH is left native: the container shares this machine's.
func buildSoulBinary(t *testing.T) string {
	t.Helper()
	soulModule, err := filepath.Abs(filepath.Join("..", "..", "..", "soul"))
	if err != nil {
		t.Fatalf("resolve soul module: %v", err)
	}
	out := filepath.Join(t.TempDir(), "soul")
	cmd := exec.Command("go", "build", "-o", out, "./cmd/soul")
	cmd.Dir = soulModule
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux")
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build soul: %v\n%s", err, combined)
	}
	return out
}

func fileSha256(t *testing.T, p string) string {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// awaitTerminal polls push_runs until the async goroutine commits a terminal
// state. Polling rather than a hook because that is what an operator's
// GET /v1/push/{apply_id} does.
func awaitTerminal(ctx context.Context, t *testing.T, r *PushRun, applyID string) *PushRunRow {
	t.Helper()
	deadline := time.Now().Add(5 * time.Minute)
	for {
		row, err := r.GetRow(ctx, applyID)
		if err != nil {
			t.Fatalf("GetRow(%s): %v", applyID, err)
		}
		switch row.Status {
		case StatusSuccess, StatusFailed, StatusPartialFailed, StatusCancelled:
			return row
		}
		if time.Now().After(deadline) {
			t.Fatalf("push run %s is still %s after 5m", applyID, row.Status)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%+v", v)
	}
	return string(b)
}

// liveDestinyLoader stands in for the git fetch, and only for that: it returns
// an in-memory destiny whose single task writes [proofPath]. Everything below
// it — render, dispatch, delivery, exec — is production code.
type liveDestinyLoader struct{ dir string }

func (l *liveDestinyLoader) Load(_ context.Context, ref artifact.DestinyRef) (*artifact.DestinyArtifact, error) {
	return &artifact.DestinyArtifact{
		Ref:      ref,
		LocalDir: l.dir,
		Manifest: &config.DestinyManifest{Name: ref.Name},
		Tasks: []config.Task{{
			Name: "prove the run reached the host",
			Module: &config.ModuleTask{
				Module: "core.file.present",
				Params: map[string]any{
					"path":    proofPath,
					"content": "reached\n",
					"mode":    "0644",
				},
			},
		}},
	}, nil
}

// liveDestinyTemplate satisfies DestinyTemplateSource; the URL is never
// fetched, because liveDestinyLoader ignores it.
type liveDestinyTemplate struct{}

func (liveDestinyTemplate) DefaultDestinySource() string {
	return "https://git.invalid/destiny/{name}.git"
}

// fixedRouter pins every SID to one provider. Multi-provider routing has its
// own PG suite (push/router_pg_integration_test.go) and is not what this test
// is about.
type fixedRouter string

func (f fixedRouter) RouteFor(_ context.Context, _ string) (string, push.RouteSource, error) {
	return string(f), push.SourceSoul, nil
}
