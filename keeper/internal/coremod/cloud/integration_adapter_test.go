//go:build integration

// Integration-тесты `core.cloud.provisioned` через [PluginAdapter] поверх
// keeper/internal/pluginhost. Полная цепочка: discovery → adapter →
// Module.Apply → Spawn → CloudDriver.Create → INSERT в souls +
// bootstrap_tokens. Имитирует прод-сценарий end-to-end.

package cloud_test

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/souls-guild/soul-stack/keeper/internal/bootstraptoken"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/cloud"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/internaltest"
	"github.com/souls-guild/soul-stack/keeper/internal/migrate"
	"github.com/souls-guild/soul-stack/keeper/internal/pluginhost"
	keepersoul "github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/keeper/migrations"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	sharedhost "github.com/souls-guild/soul-stack/shared/pluginhost"
	"google.golang.org/protobuf/types/known/structpb"
)

var integrationPool *pgxpool.Pool

func TestMain(m *testing.M) { os.Exit(run(m)) }

func run(m *testing.M) int {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	ctr, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("keeper_test"),
		tcpostgres.WithUsername("keeper"),
		tcpostgres.WithPassword("keeper"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		if requireDocker() {
			log.Fatalf("coremod/cloud integration: setup failed (REQUIRE_DOCKER): %v", err)
		}
		log.Printf("coremod/cloud integration: skipping, docker unavailable: %v", err)
		return 0
	}
	defer func() {
		termCtx, termCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer termCancel()
		_ = ctr.Terminate(termCtx)
	}()

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		log.Printf("ConnectionString: %v", err)
		return 1
	}
	if err := migrate.Apply(ctx, dsn, migrations.FS, "."); err != nil {
		log.Printf("migrate.Apply: %v", err)
		return 1
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Printf("pgxpool.New: %v", err)
		return 1
	}
	defer pool.Close()
	integrationPool = pool

	return m.Run()
}

func resetCloud(t *testing.T) {
	t.Helper()
	_, err := integrationPool.Exec(context.Background(),
		`TRUNCATE TABLE soul_seeds, bootstrap_tokens, souls, operators, audit_log CASCADE`)
	if err != nil {
		t.Fatalf("TRUNCATE: %v", err)
	}
}

// buildFqdnPlugin собирает testdata/cloud-plugin-fqdn под отдельным go.mod
// (с GOWORK=off, как и в pluginhost/integration_test.go).
func buildFqdnPlugin(t *testing.T, outDir, outName string) {
	t.Helper()
	srcDir, err := filepath.Abs(filepath.Join("testdata", "cloud-plugin-fqdn"))
	if err != nil {
		t.Fatalf("abs testdata: %v", err)
	}
	binPath := filepath.Join(outDir, outName)
	cmd := exec.Command("go", "build", "-o", binPath, ".")
	cmd.Dir = srcDir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build cloud-plugin-fqdn: %v\n%s", err, out)
	}
}

// shortDir — короткая temp-директория в /tmp; на darwin sun_path Unix-сокета
// ограничен ~104 байтами, t.TempDir под /var/folders/... съедает лимит.
func shortDir(t *testing.T, prefix string) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", prefix)
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// setupAdapter поднимает host + discovery + adapter с fqdn-плагином,
// зарегистрированным под именем "fqdn".
func setupAdapter(t *testing.T) *cloud.PluginAdapter {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("plugin host requires Unix sockets")
	}
	cacheRoot := shortDir(t, "ss-cloud-mods-")
	socketDir := shortDir(t, "ss-cloud-sock-")
	// R-nested layout (ADR-026 A1-S1): <cacheRoot>/<ns>-<name>/<commit_sha>/ —
	// иммутабельный слот с бинарём+manifest; <ns>-<name>/current → <commit_sha>
	// (относительный symlink на активный слот, как наполняет git-резолвер через
	// updateCurrentSymlink). Discover/ReadSlot читают плагин ТОЛЬКО через current.
	pluginDir := filepath.Join(cacheRoot, "soulstack-fqdn")
	const commitSHA = "0123456789abcdef0123456789abcdef01234567"
	slotDir := filepath.Join(pluginDir, commitSHA)
	if err := os.MkdirAll(slotDir, 0o755); err != nil {
		t.Fatalf("mkdir slot dir: %v", err)
	}
	buildFqdnPlugin(t, slotDir, "soul-cloud-fqdn")
	if err := os.WriteFile(filepath.Join(slotDir, "manifest.yaml"), []byte(`kind: cloud_driver
protocol_version: 1
namespace: soulstack
name: fqdn
required_capabilities: []
side_effects: []
spec:
  provider_kind: fake
  profile_schema:
    type: object
`), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	// current → <commit_sha> относительной целью (как updateCurrentSymlink).
	if err := os.Symlink(commitSHA, filepath.Join(pluginDir, pluginhost.CurrentLink)); err != nil {
		t.Fatalf("symlink current: %v", err)
	}

	found, warns, err := pluginhost.Discover(cacheRoot)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(warns) != 0 {
		t.Logf("discovery warnings: %v", warns)
	}
	if len(found) != 1 {
		t.Fatalf("expected 1 plugin in cache, got %d", len(found))
	}

	// Sigil verify-gate (S6b) теперь гейтит Spawn: навешиваем валидный
	// trust-anchor + допуск под discovered-плагин, иначе Spawn fail-closed
	// (no_trust_anchor / no_sigil). Подпись через тот же общий BuildSigilBlock,
	// что keeper-Signer (симметрия sign↔verify).
	pub, lookup := sigilForHost(t, found[0])
	host, err := pluginhost.NewHost(nil, []ed25519.PublicKey{pub}, lookup)
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	host.SocketDir = socketDir
	host.StartupTimeout = 10 * time.Second
	host.ShutdownGrace = 3 * time.Second

	adapter, err := cloud.NewPluginAdapter(host, found)
	if err != nil {
		t.Fatalf("NewPluginAdapter: %v", err)
	}
	return adapter
}

// sigilForHost подписывает валидный SigilRecord под бинарь+manifest из
// Discovered тем же helper-ом, что keeper при Sign (BuildSigilBlock +
// NormalizeManifestBytes). Возвращает trust-anchor и lookup с единственным
// допуском, готовые к навешиванию на Host.
func sigilForHost(t *testing.T, d pluginhost.Discovered) (ed25519.PublicKey, sharedhost.SigilLookup) {
	t.Helper()
	manifest, err := os.ReadFile(filepath.Join(d.Dir, "manifest.yaml"))
	if err != nil {
		t.Fatalf("read manifest for sigil: %v", err)
	}
	binBytes, err := os.ReadFile(d.BinaryPath)
	if err != nil {
		t.Fatalf("read binary for sigil: %v", err)
	}
	binSum := sha256.Sum256(binBytes)
	binHex := hex.EncodeToString(binSum[:])
	binRaw, _ := hex.DecodeString(binHex)

	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	manDigest := sha256.Sum256(sharedhost.NormalizeManifestBytes(manifest))
	const ref = "v1.0.0"
	block := sharedhost.BuildSigilBlock(d.Manifest.Namespace, d.Manifest.Name, ref, binRaw, manDigest[:])
	rec := &sharedhost.SigilRecord{
		Namespace:       d.Manifest.Namespace,
		Name:            d.Manifest.Name,
		Ref:             ref,
		BinarySHA256hex: binHex,
		Signature:       ed25519.Sign(priv, block),
		Manifest:        manifest,
	}
	return pub, hostTestLookup{d.Manifest.Namespace + "." + d.Manifest.Name: rec}
}

// hostTestLookup — минимальный sharedhost.SigilLookup поверх map для integration-теста.
type hostTestLookup map[string]*sharedhost.SigilRecord

func (l hostTestLookup) Get(ns, name string) *sharedhost.SigilRecord { return l[ns+"."+name] }

func mustStructIT(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatalf("structpb.NewStruct: %v", err)
	}
	return s
}

// TestIntegration_Apply_Created — happy path: adapter → plugin → souls/bootstrap_tokens.
func TestIntegration_Apply_Created(t *testing.T) {
	resetCloud(t)
	ctx := context.Background()

	adapter := setupAdapter(t)
	m := cloud.New(
		adapter,
		&fakeResolver{},
		cloud.NewSoulPG(integrationPool),
		cloud.NewTokenPG(integrationPool, cloud.DefaultBootstrapTokenTTL),
		cloud.NewCascadePG(integrationPool),
		nil, // audit опционален — не тестируем audit-pipeline здесь
	)

	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: cloud.StateCreated,
		Params: mustStructIT(t, map[string]any{
			"provider": "fqdn",
			"count":    float64(2),
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev == nil || ev.Failed {
		t.Fatalf("expected success final event, got %+v", ev)
	}
	if !ev.Changed {
		t.Errorf("expected changed=true, got %+v", ev)
	}

	got, err := keepersoul.SelectBySID(ctx, integrationPool, "host-1.example.com")
	if err != nil {
		t.Fatalf("SelectBySID host-1: %v", err)
	}
	if got.Status != keepersoul.StatusPending {
		t.Errorf("status=%q, want pending", got.Status)
	}
	got2, err := keepersoul.SelectBySID(ctx, integrationPool, "host-2.example.com")
	if err != nil {
		t.Fatalf("SelectBySID host-2: %v", err)
	}
	if got2.Status != keepersoul.StatusPending {
		t.Errorf("status host-2=%q, want pending", got2.Status)
	}

	// bootstrap_tokens — один на каждый SID.
	rows, err := integrationPool.Query(ctx, `SELECT sid FROM bootstrap_tokens ORDER BY sid`)
	if err != nil {
		t.Fatalf("query tokens: %v", err)
	}
	defer rows.Close()
	var sids []string
	for rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err != nil {
			t.Fatalf("scan: %v", err)
		}
		sids = append(sids, sid)
	}
	sort.Strings(sids)
	want := []string{"host-1.example.com", "host-2.example.com"}
	if len(sids) != len(want) || sids[0] != want[0] || sids[1] != want[1] {
		t.Errorf("bootstrap_tokens.sids = %v, want %v", sids, want)
	}
}

// TestIntegration_Apply_UnknownProvider — failed-event при неизвестном provider.
func TestIntegration_Apply_UnknownProvider(t *testing.T) {
	resetCloud(t)
	adapter := setupAdapter(t)
	m := cloud.New(adapter, &fakeResolver{}, cloud.NewSoulPG(integrationPool),
		cloud.NewTokenPG(integrationPool, cloud.DefaultBootstrapTokenTTL),
		cloud.NewCascadePG(integrationPool), nil)

	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: cloud.StateCreated,
		Params: mustStructIT(t, map[string]any{
			"provider": "missing",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev == nil || !ev.Failed {
		t.Fatalf("expected failed event, got %+v", ev)
	}
}

// TestIntegration_Apply_Destroyed_Cascade — ADR-017 cascade end-to-end:
// после успешного PluginHost.Destroy одна PG-tx переводит souls→destroyed,
// активные soul_seeds→orphaned, активные bootstrap_tokens→burned.
// Revoked seed-ы НЕ перетираются (precedence revoked > orphaned).
func TestIntegration_Apply_Destroyed_Cascade(t *testing.T) {
	resetCloud(t)
	ctx := context.Background()

	// Сначала через Apply(created) создаём 2 хоста — это даст полный
	// набор: souls(pending) + bootstrap_tokens(active) + soul_seeds(0).
	adapter := setupAdapter(t)
	m := cloud.New(adapter,
		&fakeResolver{},
		cloud.NewSoulPG(integrationPool),
		cloud.NewTokenPG(integrationPool, cloud.DefaultBootstrapTokenTTL),
		cloud.NewCascadePG(integrationPool), nil)

	createStream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: cloud.StateCreated,
		Params: mustStructIT(t, map[string]any{
			"provider": "fqdn",
			"count":    float64(2),
		}),
	}, createStream); err != nil {
		t.Fatalf("Apply(created): %v", err)
	}
	if createStream.Last().Failed {
		t.Fatalf("created failed: %+v", createStream.Last())
	}

	// Имитируем выпуск active-seed-а для host-1: cascade должен перевести
	// его в `orphaned`, а у host-2 активного seed-а нет (cascade игнорирует
	// без ошибки).
	_, err := integrationPool.Exec(ctx,
		`INSERT INTO soul_seeds (sid, fingerprint, serial_number, expires_at, status)
		 VALUES ($1, $2, $3, NOW() + INTERVAL '7 days', 'active')`,
		"host-1.example.com",
		// 64 lower-hex, валидный fingerprint-format.
		"abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
		"sn-1",
	)
	if err != nil {
		t.Fatalf("seed insert host-1: %v", err)
	}
	// Для host-2 — revoked-seed (не должен быть перезаписан orphaned-ом).
	_, err = integrationPool.Exec(ctx,
		`INSERT INTO soul_seeds (sid, fingerprint, serial_number, expires_at, status)
		 VALUES ($1, $2, $3, NOW() + INTERVAL '7 days', 'revoked')`,
		"host-2.example.com",
		"deadbeef0123456789abcdef0123456789abcdef0123456789abcdef01234567",
		"sn-2",
	)
	if err != nil {
		t.Fatalf("seed insert host-2: %v", err)
	}

	// Apply(destroyed) с обоими SID.
	destroyStream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: cloud.StateDestroyed,
		Params: mustStructIT(t, map[string]any{
			"provider": "fqdn",
			"vm_ids":   []any{"i-1", "i-2"},
			"sids":     []any{"host-1.example.com", "host-2.example.com"},
		}),
	}, destroyStream); err != nil {
		t.Fatalf("Apply(destroyed): %v", err)
	}
	ev := destroyStream.Last()
	if ev == nil || ev.Failed {
		t.Fatalf("destroyed final event = %+v", ev)
	}

	// souls: оба → destroyed.
	rows, err := integrationPool.Query(ctx, `SELECT sid, status FROM souls ORDER BY sid`)
	if err != nil {
		t.Fatalf("query souls: %v", err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var sid, status string
		if err := rows.Scan(&sid, &status); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[sid] = status
	}
	for _, sid := range []string{"host-1.example.com", "host-2.example.com"} {
		if got[sid] != "destroyed" {
			t.Errorf("souls[%q].status = %q, want destroyed", sid, got[sid])
		}
	}

	// soul_seeds: host-1 active → orphaned; host-2 revoked → revoked (precedence).
	seedRows, err := integrationPool.Query(ctx,
		`SELECT sid, status FROM soul_seeds ORDER BY sid`)
	if err != nil {
		t.Fatalf("query seeds: %v", err)
	}
	defer seedRows.Close()
	gotSeeds := map[string]string{}
	for seedRows.Next() {
		var sid, status string
		if err := seedRows.Scan(&sid, &status); err != nil {
			t.Fatalf("scan: %v", err)
		}
		gotSeeds[sid] = status
	}
	if gotSeeds["host-1.example.com"] != "orphaned" {
		t.Errorf("seed host-1.status = %q, want orphaned", gotSeeds["host-1.example.com"])
	}
	if gotSeeds["host-2.example.com"] != "revoked" {
		t.Errorf("seed host-2.status = %q, want revoked (precedence over orphaned)", gotSeeds["host-2.example.com"])
	}

	// bootstrap_tokens: оба → burned (used_at != NULL, used_by_kid = system-cloud-destroy).
	tokRows, err := integrationPool.Query(ctx,
		`SELECT sid, used_at IS NOT NULL AS used, used_by_kid FROM bootstrap_tokens ORDER BY sid`)
	if err != nil {
		t.Fatalf("query tokens: %v", err)
	}
	defer tokRows.Close()
	burned := 0
	for tokRows.Next() {
		var sid, kid string
		var used bool
		if err := tokRows.Scan(&sid, &used, &kid); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if !used {
			t.Errorf("token for %q not burned (used_at IS NULL)", sid)
			continue
		}
		if kid != bootstraptoken.SystemKIDCloudDestroy {
			t.Errorf("token for %q used_by_kid = %q, want %q", sid, kid, bootstraptoken.SystemKIDCloudDestroy)
		}
		burned++
	}
	if burned != 2 {
		t.Errorf("burned tokens = %d, want 2", burned)
	}

	// Output модуля — содержит cascade-counts.
	out := ev.Output.AsMap()
	for _, k := range []string{"souls_updated", "seeds_orphaned", "tokens_burned"} {
		if _, has := out[k]; !has {
			t.Errorf("output missing %q: %v", k, out)
		}
	}
	if out["souls_updated"] != float64(2) {
		t.Errorf("souls_updated = %v, want 2", out["souls_updated"])
	}
	// seeds_orphaned = 1 (только active host-1; revoked host-2 пропущен).
	if out["seeds_orphaned"] != float64(1) {
		t.Errorf("seeds_orphaned = %v, want 1", out["seeds_orphaned"])
	}
	if out["tokens_burned"] != float64(2) {
		t.Errorf("tokens_burned = %v, want 2", out["tokens_burned"])
	}
}

// TestIntegration_Apply_Destroyed_Atomicity — если PluginHost.Destroy
// возвращает ошибку, реестры остаются нетронутыми (souls.status НЕ
// меняется на destroyed). Проверяет «cascade ПОСЛЕ Destroy» инвариант.
func TestIntegration_Apply_Destroyed_Atomicity(t *testing.T) {
	resetCloud(t)
	ctx := context.Background()

	adapter := setupAdapter(t)
	m := cloud.New(adapter,
		&fakeResolver{},
		cloud.NewSoulPG(integrationPool),
		cloud.NewTokenPG(integrationPool, cloud.DefaultBootstrapTokenTTL),
		cloud.NewCascadePG(integrationPool), nil)

	// Создаём один хост.
	createStream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: cloud.StateCreated,
		Params: mustStructIT(t, map[string]any{
			"provider": "fqdn",
			"count":    float64(1),
		}),
	}, createStream); err != nil {
		t.Fatalf("Apply(created): %v", err)
	}

	// Destroy с unknown provider → PluginHost.Destroy ошибётся.
	destroyStream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: cloud.StateDestroyed,
		Params: mustStructIT(t, map[string]any{
			"provider": "missing",
			"vm_ids":   []any{"i-1"},
			"sids":     []any{"host-1.example.com"},
		}),
	}, destroyStream); err != nil {
		t.Fatalf("Apply(destroyed): %v", err)
	}
	if !destroyStream.Last().Failed {
		t.Fatal("expected failed=true on unknown provider")
	}

	// souls.status должен остаться pending (cascade не выполнялся).
	var status string
	if err := integrationPool.QueryRow(ctx,
		`SELECT status FROM souls WHERE sid = $1`, "host-1.example.com").Scan(&status); err != nil {
		t.Fatalf("SELECT status: %v", err)
	}
	if status != "pending" {
		t.Errorf("status = %q, want pending (cascade must NOT run if Destroy failed)", status)
	}
}
