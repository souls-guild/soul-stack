package repo_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/internaltest"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/repo"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

func mustStruct(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatalf("structpb.NewStruct: %v", err)
	}
	return s
}

// runnerFor builds a fakeRunner that makes util.DetectPkgMgr return the given
// pkg-mgr (via `command -v <bin>`).
func runnerFor(mgr util.PkgMgr) *internaltest.Runner {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 127} // command not found for everything else
	bin := map[util.PkgMgr]string{
		util.PkgMgrApt: "apt-get",
		util.PkgMgrDnf: "dnf",
		util.PkgMgrYum: "yum",
		util.PkgMgrApk: "apk",
	}[mgr]
	r.On("command -v "+bin, util.Result{ExitCode: 0})
	return r
}

// newModule builds a Module with directories swapped to TempDir and a runner
// for the given pkg-mgr.
func newModule(t *testing.T, mgr util.PkgMgr) (*repo.Module, string) {
	t.Helper()
	root := t.TempDir()
	m := repo.New()
	m.Runner = runnerFor(mgr)
	m.AptSourcesDir = filepath.Join(root, "apt", "sources.list.d")
	m.AptKeyringsDir = filepath.Join(root, "apt", "keyrings")
	m.YumReposDir = filepath.Join(root, "yum.repos.d")
	m.ApkReposFile = filepath.Join(root, "apk", "repositories")
	return m, root
}

func applyTo(t *testing.T, m *repo.Module, state string, params map[string]any) *internaltest.ApplyStream {
	t.Helper()
	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{State: state, Params: mustStruct(t, params)}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	return stream
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func warningsOf(ev *pluginv1.ApplyEvent) []string {
	if ev.Output == nil {
		return nil
	}
	v, ok := ev.Output.Fields["warnings"]
	if !ok {
		return nil
	}
	lv := v.GetListValue()
	if lv == nil {
		return nil
	}
	out := make([]string, 0, len(lv.Values))
	for _, item := range lv.Values {
		out = append(out, item.GetStringValue())
	}
	return out
}

// --- Validate ---

func TestValidate_UnknownState(t *testing.T) {
	reply, _ := repo.New().Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "frobnicate",
		Params: mustStruct(t, map[string]any{"name": "x", "uri": "https://m"}),
	})
	if reply.Ok {
		t.Fatal("Validate ok=true for an unknown state")
	}
}

func TestValidate_PresentRequiresUri(t *testing.T) {
	reply, _ := repo.New().Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "present",
		Params: mustStruct(t, map[string]any{"name": "x"}),
	})
	if reply.Ok {
		t.Fatal("Validate ok=true for present without uri")
	}
}

func TestValidate_NameRejectsPathTraversal(t *testing.T) {
	for _, name := range []string{"../evil", "a/b", "with space", ".."} {
		reply, _ := repo.New().Validate(context.Background(), &pluginv1.ValidateRequest{
			State:  "present",
			Params: mustStruct(t, map[string]any{"name": name, "uri": "https://m"}),
		})
		if reply.Ok {
			t.Fatalf("Validate ok=true for an unsafe name %q", name)
		}
	}
}

func TestValidate_RejectsNonHTTPScheme(t *testing.T) {
	for _, uri := range []string{"file:///etc/passwd", "ftp://m/x", "ssh://m"} {
		reply, _ := repo.New().Validate(context.Background(), &pluginv1.ValidateRequest{
			State:  "present",
			Params: mustStruct(t, map[string]any{"name": "x", "uri": uri}),
		})
		if reply.Ok {
			t.Fatalf("Validate ok=true for a forbidden scheme %q", uri)
		}
	}
}

func TestValidate_AcceptsHTTPAndHTTPS(t *testing.T) {
	for _, uri := range []string{"https://m/deb", "http://internal-mirror/deb"} {
		reply, _ := repo.New().Validate(context.Background(), &pluginv1.ValidateRequest{
			State:  "present",
			Params: mustStruct(t, map[string]any{"name": "x", "uri": uri}),
		})
		if !reply.Ok {
			t.Fatalf("Validate ok=false for a valid uri %q: %v", uri, reply.Errors)
		}
	}
}

// --- apt: present ---

func TestApt_Present_WritesListWithSignedByAndKey(t *testing.T) {
	m, _ := newModule(t, util.PkgMgrApt)
	stream := applyTo(t, m, "present", map[string]any{
		"name":       "docker",
		"uri":        "https://download.docker.com/linux/ubuntu",
		"suite":      "jammy",
		"components": []any{"stable"},
		"gpg_key":    "-----BEGIN PGP PUBLIC KEY-----\nABC\n-----END PGP PUBLIC KEY-----\n",
	})
	ev := stream.Last()
	if !ev.Changed || ev.Failed {
		t.Fatalf("changed=%v failed=%v msg=%s", ev.Changed, ev.Failed, ev.Message)
	}
	listPath := filepath.Join(m.AptSourcesDir, "docker.list")
	keyPath := filepath.Join(m.AptKeyringsDir, "docker.gpg")

	got := read(t, listPath)
	wantLine := "deb [signed-by=" + keyPath + "] https://download.docker.com/linux/ubuntu jammy stable\n"
	if got != wantLine {
		t.Fatalf(".list=%q want %q", got, wantLine)
	}
	if k := read(t, keyPath); !strings.Contains(k, "BEGIN PGP PUBLIC KEY") {
		t.Fatalf("key not materialized: %q", k)
	}
}

func TestApt_Present_DisabledCommentsLine(t *testing.T) {
	m, _ := newModule(t, util.PkgMgrApt)
	applyTo(t, m, "present", map[string]any{
		"name":    "extra",
		"uri":     "https://m/deb",
		"suite":   "stable",
		"enabled": false,
	})
	got := read(t, filepath.Join(m.AptSourcesDir, "extra.list"))
	if !strings.HasPrefix(got, "# deb ") {
		t.Fatalf("enabled=false should comment out the line: %q", got)
	}
}

func TestApt_Present_Idempotent(t *testing.T) {
	m, _ := newModule(t, util.PkgMgrApt)
	params := map[string]any{
		"name":    "docker",
		"uri":     "https://m/deb",
		"suite":   "jammy",
		"gpg_key": "KEYDATA",
	}
	first := applyTo(t, m, "present", params)
	if !first.Last().Changed {
		t.Fatal("first run: changed=false")
	}
	second := applyTo(t, m, "present", params)
	if second.Last().Changed {
		t.Fatal("repeat run: changed=true (not idempotent)")
	}
}

func TestApt_Present_KeyChangeTriggersChanged(t *testing.T) {
	m, _ := newModule(t, util.PkgMgrApt)
	base := map[string]any{"name": "docker", "uri": "https://m/deb", "suite": "x", "gpg_key": "OLD"}
	applyTo(t, m, "present", base)

	base["gpg_key"] = "NEW"
	stream := applyTo(t, m, "present", base)
	if !stream.Last().Changed {
		t.Fatal("changing the key should give changed=true")
	}
	if k := read(t, filepath.Join(m.AptKeyringsDir, "docker.gpg")); k != "NEW" {
		t.Fatalf("key not updated: %q", k)
	}
}

// --- yum/dnf: present ---

func TestYum_Present_WritesIni(t *testing.T) {
	m, _ := newModule(t, util.PkgMgrDnf)
	applyTo(t, m, "present", map[string]any{
		"name":    "epel",
		"uri":     "https://m/epel",
		"gpg_key": "https://m/RPM-GPG-KEY-EPEL",
	})
	got := read(t, filepath.Join(m.YumReposDir, "epel.repo"))
	for _, want := range []string{
		"[epel]",
		"name=epel",
		"baseurl=https://m/epel",
		"enabled=1",
		"gpgcheck=1",
		"gpgkey=https://m/RPM-GPG-KEY-EPEL",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf(".repo does not contain %q:\n%s", want, got)
		}
	}
}

func TestYum_Present_GpgCheckFalseWritesZero(t *testing.T) {
	m, _ := newModule(t, util.PkgMgrYum)
	stream := applyTo(t, m, "present", map[string]any{
		"name":      "local",
		"uri":       "https://m/local",
		"gpg_check": false,
	})
	got := read(t, filepath.Join(m.YumReposDir, "local.repo"))
	if !strings.Contains(got, "gpgcheck=0") {
		t.Fatalf("gpg_check=false should give gpgcheck=0:\n%s", got)
	}
	// gpg_check=false must return a warning (opt-out + warning).
	ws := warningsOf(stream.Last())
	if !hasSubstr(ws, "gpg_check disabled") {
		t.Fatalf("expected a warning about gpg_check, got %v", ws)
	}
}

func TestYum_Present_Idempotent(t *testing.T) {
	m, _ := newModule(t, util.PkgMgrDnf)
	params := map[string]any{"name": "epel", "uri": "https://m/epel"}
	if !applyTo(t, m, "present", params).Last().Changed {
		t.Fatal("first run: changed=false")
	}
	if applyTo(t, m, "present", params).Last().Changed {
		t.Fatal("repeat run: changed=true (not idempotent)")
	}
}

// --- apk ---

func TestApk_Present_UpsertsLine(t *testing.T) {
	m, _ := newModule(t, util.PkgMgrApk)
	if mkErr := os.MkdirAll(filepath.Dir(m.ApkReposFile), 0o755); mkErr != nil {
		t.Fatalf("mkdir: %v", mkErr)
	}
	if werr := os.WriteFile(m.ApkReposFile, []byte("https://dl-cdn.alpinelinux.org/alpine/v3.19/main\n"), 0o644); werr != nil {
		t.Fatalf("seed: %v", werr)
	}
	applyTo(t, m, "present", map[string]any{
		"name": "community",
		"uri":  "https://dl-cdn.alpinelinux.org/alpine/v3.19/community",
	})
	got := read(t, m.ApkReposFile)
	if !strings.Contains(got, "v3.19/main") || !strings.Contains(got, "v3.19/community") {
		t.Fatalf("apk repositories was not appended: %q", got)
	}
}

func TestApk_Present_Idempotent(t *testing.T) {
	m, _ := newModule(t, util.PkgMgrApk)
	params := map[string]any{"name": "community", "uri": "https://m/community"}
	if !applyTo(t, m, "present", params).Last().Changed {
		t.Fatal("first run: changed=false")
	}
	if applyTo(t, m, "present", params).Last().Changed {
		t.Fatal("repeat run: changed=true (not idempotent)")
	}
}

// TestApk_Present_PreservesMode: editing an existing /etc/apk/repositories is
// in-place, so the original file's mode is preserved (AtomicWritePreserving),
// and an idempotent repeat gives changed=false.
func TestApk_Present_PreservesMode(t *testing.T) {
	m, _ := newModule(t, util.PkgMgrApk)
	if mkErr := os.MkdirAll(filepath.Dir(m.ApkReposFile), 0o755); mkErr != nil {
		t.Fatalf("mkdir: %v", mkErr)
	}
	if werr := os.WriteFile(m.ApkReposFile, []byte("https://dl-cdn.alpinelinux.org/alpine/v3.19/main\n"), 0o600); werr != nil {
		t.Fatalf("seed: %v", werr)
	}
	params := map[string]any{
		"name": "community",
		"uri":  "https://dl-cdn.alpinelinux.org/alpine/v3.19/community",
	}
	stream := applyTo(t, m, "present", params)
	if !stream.Last().Changed {
		t.Fatal("first run: changed=false")
	}
	info, err := os.Stat(m.ApkReposFile)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode not preserved: expected 0600, got %o", got)
	}
	if applyTo(t, m, "present", params).Last().Changed {
		t.Fatal("repeat run: changed=true (not idempotent)")
	}
}

func TestApk_Absent_RequiresUri(t *testing.T) {
	m, _ := newModule(t, util.PkgMgrApk)
	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "absent",
		Params: mustStruct(t, map[string]any{"name": "community"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("apk absent without uri should fail")
	}
}

// --- absent (apt/yum) ---

func TestApt_Absent_RemovesListKeepsKey(t *testing.T) {
	m, _ := newModule(t, util.PkgMgrApt)
	applyTo(t, m, "present", map[string]any{
		"name": "docker", "uri": "https://m/deb", "suite": "x", "gpg_key": "K",
	})
	keyPath := filepath.Join(m.AptKeyringsDir, "docker.gpg")
	listPath := filepath.Join(m.AptSourcesDir, "docker.list")

	stream := applyTo(t, m, "absent", map[string]any{"name": "docker"})
	if !stream.Last().Changed {
		t.Fatal("absent of an existing repo: changed=false")
	}
	if _, err := os.Stat(listPath); !os.IsNotExist(err) {
		t.Fatal(".list not removed")
	}
	// The key is deliberately left alone (may be shared with other repos).
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("key must not be removed on absent: %v", err)
	}
}

func TestApt_Absent_Idempotent(t *testing.T) {
	m, _ := newModule(t, util.PkgMgrApt)
	stream := applyTo(t, m, "absent", map[string]any{"name": "absent-repo"})
	if stream.Last().Changed {
		t.Fatal("absent of a non-existent repo: changed=true")
	}
}

// --- warnings ---

func TestApt_Present_HTTPUriWarns(t *testing.T) {
	m, _ := newModule(t, util.PkgMgrApt)
	stream := applyTo(t, m, "present", map[string]any{
		"name": "mirror", "uri": "http://internal-mirror/deb", "suite": "x",
	})
	ws := warningsOf(stream.Last())
	if !hasSubstr(ws, "plain http") {
		t.Fatalf("expected a warning about http://, got %v", ws)
	}
}

// TestYum_Present_GpgCheckTrueNoKeyWarns: gpg_check enabled (default) but
// gpg_key unset → warning (for dnf/yum this means gpgcheck=1 without
// gpgkey=, which fails package install on the host). Symmetric with the
// gpg_check=false warning.
func TestYum_Present_GpgCheckTrueNoKeyWarns(t *testing.T) {
	m, _ := newModule(t, util.PkgMgrDnf)
	stream := applyTo(t, m, "present", map[string]any{
		"name": "epel", "uri": "https://m/epel",
	})
	ws := warningsOf(stream.Last())
	if !hasSubstr(ws, "gpg_check enabled but no gpg_key set") {
		t.Fatalf("expected a warning about gpg_check without gpg_key, got %v", ws)
	}
	if !hasSubstr(ws, "gpgcheck=1 without gpgkey will fail package install") {
		t.Fatalf("expected dnf/yum-specific wording in the warning, got %v", ws)
	}
}

// TestYum_Present_GpgCheckTrueWithKeyNoWarn: gpg_key set → no warning.
func TestYum_Present_GpgCheckTrueWithKeyNoWarn(t *testing.T) {
	m, _ := newModule(t, util.PkgMgrDnf)
	stream := applyTo(t, m, "present", map[string]any{
		"name": "epel", "uri": "https://m/epel", "gpg_key": "https://m/RPM-GPG-KEY-EPEL",
	})
	ws := warningsOf(stream.Last())
	if hasSubstr(ws, "gpg_check enabled but no gpg_key set") {
		t.Fatalf("warning about missing gpg_key must not appear when a key is set, got %v", ws)
	}
}

// TestApk_Present_GpgCheckTrueNoKeyWarns: for apk, the missing-gpg_key
// warning should point to /etc/apk/keys, not the dnf/yum-specific gpgkey=.
func TestApk_Present_GpgCheckTrueNoKeyWarns(t *testing.T) {
	m, _ := newModule(t, util.PkgMgrApk)
	stream := applyTo(t, m, "present", map[string]any{
		"name": "alpine-edge", "uri": "https://dl-cdn.alpinelinux.org/alpine/edge/main",
	})
	ws := warningsOf(stream.Last())
	if !hasSubstr(ws, "gpg_check enabled but no gpg_key set") {
		t.Fatalf("expected a warning about gpg_check without gpg_key, got %v", ws)
	}
	if !hasSubstr(ws, "/etc/apk/keys") {
		t.Fatalf("expected apk-specific wording in the warning, got %v", ws)
	}
	if hasSubstr(ws, "gpgkey") {
		t.Fatalf("warning must not attribute dnf/yum gpgkey= wording to apk, got %v", ws)
	}
}

// --- backend not detected ---

func TestApply_NoPkgMgr_Fails(t *testing.T) {
	root := t.TempDir()
	m := repo.New()
	m.Runner = internaltest.NewRunner() // everything → 127, nothing found
	m.AptSourcesDir = root
	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "present",
		Params: mustStruct(t, map[string]any{"name": "x", "uri": "https://m"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("without pkg-mgr, Apply should fail")
	}
}

// --- apt: arch (ADR-071 multi-arch) ---

// TestApt_Present_EmitsArch: arch set without gpg_key → options `[arch=...]`.
func TestApt_Present_EmitsArch(t *testing.T) {
	m, _ := newModule(t, util.PkgMgrApt)
	applyTo(t, m, "present", map[string]any{
		"name":       "nexus",
		"uri":        "https://nexus/deb",
		"suite":      "bookworm",
		"components": []any{"main"},
		"arch":       []any{"amd64"},
	})
	got := read(t, filepath.Join(m.AptSourcesDir, "nexus.list"))
	want := "deb [arch=amd64] https://nexus/deb bookworm main\n"
	if got != want {
		t.Fatalf(".list=%q want %q", got, want)
	}
}

// TestApt_Present_ArchMultiValue: multiple architectures → arch=amd64,arm64.
func TestApt_Present_ArchMultiValue(t *testing.T) {
	m, _ := newModule(t, util.PkgMgrApt)
	applyTo(t, m, "present", map[string]any{
		"name":  "multi",
		"uri":   "https://m/deb",
		"suite": "stable",
		"arch":  []any{"amd64", "arm64"},
	})
	got := read(t, filepath.Join(m.AptSourcesDir, "multi.list"))
	if !strings.Contains(got, "[arch=amd64,arm64]") {
		t.Fatalf("multi-arch not joined by comma: %q", got)
	}
}

// TestApt_Present_ArchWithSignedByOrder: with a key set, the option order is
// signed-by then arch (as in the ADR-071 example).
func TestApt_Present_ArchWithSignedByOrder(t *testing.T) {
	m, _ := newModule(t, util.PkgMgrApt)
	applyTo(t, m, "present", map[string]any{
		"name": "ord", "uri": "https://m/deb", "suite": "x",
		"arch": []any{"amd64"}, "gpg_key": "K",
	})
	keyPath := filepath.Join(m.AptKeyringsDir, "ord.gpg")
	got := read(t, filepath.Join(m.AptSourcesDir, "ord.list"))
	want := "deb [signed-by=" + keyPath + " arch=amd64] https://m/deb x\n"
	if got != want {
		t.Fatalf(".list=%q want %q", got, want)
	}
}

// TestApt_Present_ArchIdempotent: same arch → repeat changed=false.
func TestApt_Present_ArchIdempotent(t *testing.T) {
	m, _ := newModule(t, util.PkgMgrApt)
	params := map[string]any{
		"name": "nexus", "uri": "https://m/deb", "suite": "bookworm", "arch": []any{"amd64"},
	}
	if !applyTo(t, m, "present", params).Last().Changed {
		t.Fatal("first run: changed=false")
	}
	if applyTo(t, m, "present", params).Last().Changed {
		t.Fatal("repeat run with the same arch: changed=true (not idempotent)")
	}
}

// TestApt_Present_ArchChangeTriggersChanged: changing arch → drift (changed=true).
func TestApt_Present_ArchChangeTriggersChanged(t *testing.T) {
	m, _ := newModule(t, util.PkgMgrApt)
	base := map[string]any{"name": "nexus", "uri": "https://m/deb", "suite": "x", "arch": []any{"amd64"}}
	applyTo(t, m, "present", base)
	base["arch"] = []any{"arm64"}
	if !applyTo(t, m, "present", base).Last().Changed {
		t.Fatal("changing arch should produce changed=true")
	}
}

// TestValidate_RejectsBadArch: an architecture token with disallowed
// characters (space/bracket/`=`/empty/upper) is rejected — protection
// against injection into apt options.
func TestValidate_RejectsBadArch(t *testing.T) {
	for _, bad := range [][]any{
		{"amd64 evil"}, {"a]b"}, {"arch=x"}, {""}, {"AMD64"},
	} {
		reply, _ := repo.New().Validate(context.Background(), &pluginv1.ValidateRequest{
			State:  "present",
			Params: mustStruct(t, map[string]any{"name": "x", "uri": "https://m", "arch": bad}),
		})
		if reply.Ok {
			t.Fatalf("Validate ok=true for unsafe arch %v", bad)
		}
	}
}

// TestApt_Present_RedisMirrorRecipe — the acceptance scenario for
// NIM-104/ADR-071: mirroring redis.io in Nexus is declared with a single
// core.repo.present (uri=Nexus, arch=amd64, inline gpg key — variant B: the
// key is brought in by core.url.fetched → ${ file() }). Checks the exact apt
// string, the keyring, and idempotency.
func TestApt_Present_RedisMirrorRecipe(t *testing.T) {
	m, _ := newModule(t, util.PkgMgrApt)
	const key = "-----BEGIN PGP PUBLIC KEY BLOCK-----\nREDISKEY\n-----END PGP PUBLIC KEY BLOCK-----\n"
	params := map[string]any{
		"name":       "redis",
		"uri":        "https://nexus.internal/repository/redis-apt",
		"suite":      "bookworm",
		"components": []any{"main"},
		"arch":       []any{"amd64"},
		"gpg_key":    key,
	}
	first := applyTo(t, m, "present", params)
	if !first.Last().Changed || first.Last().Failed {
		t.Fatalf("recipe apply: changed=%v failed=%v", first.Last().Changed, first.Last().Failed)
	}
	keyPath := filepath.Join(m.AptKeyringsDir, "redis.gpg")
	got := read(t, filepath.Join(m.AptSourcesDir, "redis.list"))
	want := "deb [signed-by=" + keyPath + " arch=amd64] https://nexus.internal/repository/redis-apt bookworm main\n"
	if got != want {
		t.Fatalf(".list=%q\nwant %q", got, want)
	}
	if k := read(t, keyPath); k != key {
		t.Fatalf("keyring bytes mismatch: %q", k)
	}
	if applyTo(t, m, "present", params).Last().Changed {
		t.Fatal("recipe repeat: changed=true (not idempotent)")
	}
}

func hasSubstr(items []string, sub string) bool {
	for _, it := range items {
		if strings.Contains(it, sub) {
			return true
		}
	}
	return false
}

// --- gpg_key_path (variant B: reference, no copy) + dest ---

func TestApt_Present_GpgKeyPath_SignedByNoCopy(t *testing.T) {
	m, root := newModule(t, util.PkgMgrApt)
	keyOnHost := filepath.Join(root, "keys", "docker.gpg")
	if err := os.MkdirAll(filepath.Dir(keyOnHost), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyOnHost, []byte("PUBKEY"), 0o644); err != nil {
		t.Fatal(err)
	}
	stream := applyTo(t, m, "present", map[string]any{
		"name":         "docker",
		"uri":          "https://download.docker.com/linux/ubuntu",
		"suite":        "jammy",
		"components":   []any{"stable"},
		"gpg_key_path": keyOnHost,
	})
	ev := stream.Last()
	if !ev.Changed || ev.Failed {
		t.Fatalf("changed=%v failed=%v msg=%s", ev.Changed, ev.Failed, ev.Message)
	}
	got := read(t, filepath.Join(m.AptSourcesDir, "docker.list"))
	want := "deb [signed-by=" + keyOnHost + "] https://download.docker.com/linux/ubuntu jammy stable\n"
	if got != want {
		t.Fatalf(".list=%q want %q", got, want)
	}
	// Variant B: the module must NOT materialize its own keyring copy.
	if _, err := os.Stat(filepath.Join(m.AptKeyringsDir, "docker.gpg")); !os.IsNotExist(err) {
		t.Fatalf("variant B must not copy the key into AptKeyringsDir (stat err=%v)", err)
	}
}

func TestApt_Present_GpgKeyPath_MissingFailsGuard(t *testing.T) {
	m, root := newModule(t, util.PkgMgrApt)
	stream := applyTo(t, m, "present", map[string]any{
		"name":         "docker",
		"uri":          "https://m/deb",
		"suite":        "x",
		"gpg_key_path": filepath.Join(root, "nope.gpg"),
	})
	ev := stream.Last()
	if !ev.Failed {
		t.Fatal("missing gpg_key_path must fail (existence guard)")
	}
	if !strings.Contains(ev.Message, "does not exist") {
		t.Fatalf("want a 'does not exist' guard error, got %q", ev.Message)
	}
	if _, err := os.Stat(filepath.Join(m.AptSourcesDir, "docker.list")); !os.IsNotExist(err) {
		t.Fatal(".list must not be written when the guard fails")
	}
}

func TestApt_Present_DestOverridesPath(t *testing.T) {
	m, root := newModule(t, util.PkgMgrApt)
	dest := filepath.Join(root, "custom", "docker.list")
	applyTo(t, m, "present", map[string]any{
		"name": "docker", "uri": "https://m/deb", "suite": "x", "dest": dest,
	})
	if got := read(t, dest); !strings.Contains(got, "https://m/deb") {
		t.Fatalf("dest not honored: %q", got)
	}
	if _, err := os.Stat(filepath.Join(m.AptSourcesDir, "docker.list")); !os.IsNotExist(err) {
		t.Fatal("default .list path must not be written when dest is set")
	}
}

func TestYum_Present_GpgKeyPathAsGpgkey(t *testing.T) {
	m, root := newModule(t, util.PkgMgrDnf)
	keyOnHost := filepath.Join(root, "RPM-GPG-KEY-foo")
	if err := os.WriteFile(keyOnHost, []byte("PUB"), 0o644); err != nil {
		t.Fatal(err)
	}
	applyTo(t, m, "present", map[string]any{
		"name": "foo", "uri": "https://m/repo", "gpg_key_path": keyOnHost,
	})
	if got := read(t, filepath.Join(m.YumReposDir, "foo.repo")); !strings.Contains(got, "gpgkey="+keyOnHost) {
		t.Fatalf("gpgkey= should reference gpg_key_path: %q", got)
	}
}

func TestApk_RejectsGpgKeyPathAndDest(t *testing.T) {
	for _, extra := range []map[string]any{
		{"gpg_key_path": "/etc/apk/keys/x.rsa.pub"},
		{"dest": "/etc/apk/custom"},
	} {
		m, _ := newModule(t, util.PkgMgrApk)
		params := map[string]any{"name": "edge", "uri": "https://dl-cdn.alpinelinux.org/alpine/edge/main"}
		for k, v := range extra {
			params[k] = v
		}
		if !applyTo(t, m, "present", params).Last().Failed {
			t.Fatalf("apk must reject %v", extra)
		}
	}
}

func TestValidate_GpgKeyAndPathMutuallyExclusive(t *testing.T) {
	reply, _ := repo.New().Validate(context.Background(), &pluginv1.ValidateRequest{
		State: "present",
		Params: mustStruct(t, map[string]any{
			"name": "x", "uri": "https://m",
			"gpg_key": "INLINE", "gpg_key_path": "/etc/apt/keyrings/x.gpg",
		}),
	})
	if reply.Ok {
		t.Fatal("gpg_key + gpg_key_path together must be rejected")
	}
}

func TestValidate_PathsMustBeAbsoluteAndSafe(t *testing.T) {
	for _, tc := range []map[string]any{
		{"name": "x", "uri": "https://m", "gpg_key_path": "relative/key.gpg"},
		{"name": "x", "uri": "https://m", "gpg_key_path": "/etc/../key.gpg"},
		{"name": "x", "uri": "https://m", "dest": "relative.list"},
	} {
		reply, _ := repo.New().Validate(context.Background(), &pluginv1.ValidateRequest{
			State: "present", Params: mustStruct(t, tc),
		})
		if reply.Ok {
			t.Fatalf("unsafe path must be rejected: %v", tc)
		}
	}
}
