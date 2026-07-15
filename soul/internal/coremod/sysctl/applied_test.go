package sysctl_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/internaltest"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/sysctl"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
)

// dropInPath is the drop-in path built by applyApplied (filepath.Join + .conf).
func dropInPath(dir, fname string) string {
	if !strings.HasSuffix(fname, ".conf") {
		fname += ".conf"
	}
	return filepath.Join(dir, fname)
}

func appliedParams(settings map[string]any, extra map[string]any) map[string]any {
	p := map[string]any{
		"filename": "30-redis",
		"settings": settings,
	}
	for k, v := range extra {
		p[k] = v
	}
	return p
}

func reloadCalled(r *internaltest.Runner) bool {
	for _, c := range r.Calls {
		if strings.Contains(c, "sysctl") && strings.Contains(c, "-p") {
			return true
		}
	}
	return false
}

// TestApplied_NewDropIn_WritesAndReloads — file doesn't exist → write drop-in
// (changed) + reload (auto: file-change). Content is deterministic, sorted keys.
func TestApplied_NewDropIn_WritesAndReloads(t *testing.T) {
	dir := t.TempDir()
	r := internaltest.NewRunner()
	r.On("sysctl -p "+dropInPath(dir, "30-redis"), util.Result{ExitCode: 0})
	m := &sysctl.Module{Runner: r, Dir: dir}

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "applied",
		Params: mustStruct(t, appliedParams(map[string]any{
			"vm.swappiness":        "1",
			"net.core.somaxconn":   "65535",
			"vm.overcommit_memory": "1",
		}, nil)),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Changed {
		t.Fatal("Changed=false при создании drop-in")
	}
	got, err := os.ReadFile(dropInPath(dir, "30-redis"))
	if err != nil {
		t.Fatalf("drop-in: %v", err)
	}
	// SORTED keys: net.core.somaxconn < vm.overcommit_memory < vm.swappiness.
	want := "net.core.somaxconn = 65535\nvm.overcommit_memory = 1\nvm.swappiness = 1\n"
	if string(got) != want {
		t.Fatalf("контент drop-in:\n--- got ---\n%s\n--- want ---\n%s", string(got), want)
	}
	if !reloadCalled(r) {
		t.Fatal("reload (sysctl -p) не вызван при file-change")
	}
}

// TestApplied_EmptySettings_NoOp — empty settings (len==0): early no-op,
// changed=false, no empty drop-in written and no reload (a bulk task with no
// params has nothing to apply).
func TestApplied_EmptySettings_NoOp(t *testing.T) {
	dir := t.TempDir()
	r := internaltest.NewRunner()
	m := &sysctl.Module{Runner: r, Dir: dir}

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "applied",
		Params: mustStruct(t, appliedParams(map[string]any{}, nil)),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stream.Last().Failed {
		t.Fatalf("пустой settings → Failed: %v", stream.Last().Message)
	}
	if stream.Last().Changed {
		t.Fatal("пустой settings → changed=true (ожидался no-op)")
	}
	if _, err := os.Stat(dropInPath(dir, "30-redis")); !os.IsNotExist(err) {
		t.Fatalf("пустой settings записал drop-in (ожидалось: файла нет): %v", err)
	}
	if reloadCalled(r) {
		t.Fatalf("пустой settings вызвал reload: %v", r.Calls)
	}
}

// TestPlanApplied_EmptySettings_NoDrift — empty settings → drift=false
// (symmetric with applyApplied: nothing to apply means no drift), no write.
func TestPlanApplied_EmptySettings_NoDrift(t *testing.T) {
	dir := t.TempDir()
	r := internaltest.NewRunner()
	m := &sysctl.Module{Runner: r, Dir: dir}

	stream := &planStream{}
	if err := m.Plan(&pluginv1.PlanRequest{
		State:  "applied",
		Params: mustStruct(t, appliedParams(map[string]any{}, nil)),
	}, stream); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := stream.last(); got == nil || got.GetChanged() {
		t.Fatalf("changed=%v, want false (пустой settings — no drift)", got.GetChanged())
	}
	if _, err := os.Stat(dropInPath(dir, "30-redis")); !os.IsNotExist(err) {
		t.Fatalf("Plan записал drop-in на пустом settings: %v", err)
	}
}

// TestApplied_Idempotent_SameMap — drop-in already matches → changed=false, no
// write, no reload (reload-gating: auto + no file-change → no-op).
func TestApplied_Idempotent_SameMap(t *testing.T) {
	dir := t.TempDir()
	path := dropInPath(dir, "30-redis")
	seed := "net.core.somaxconn = 65535\nvm.swappiness = 1\n"
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	r := internaltest.NewRunner()
	m := &sysctl.Module{Runner: r, Dir: dir}

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "applied",
		Params: mustStruct(t, appliedParams(map[string]any{
			"vm.swappiness":      "1",
			"net.core.somaxconn": "65535",
		}, nil)),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stream.Last().Changed {
		t.Fatal("Changed=true для идентичного drop-in")
	}
	if reloadCalled(r) {
		t.Fatalf("reload вызван без file-change: %v", r.Calls)
	}
}

// TestApplied_Drift_RewritesAndReloads — drop-in content differs → rewrite
// (changed) + reload (file-change).
func TestApplied_Drift_RewritesAndReloads(t *testing.T) {
	dir := t.TempDir()
	path := dropInPath(dir, "30-redis")
	if err := os.WriteFile(path, []byte("vm.swappiness = 60\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	r := internaltest.NewRunner()
	r.On("sysctl -p "+path, util.Result{ExitCode: 0})
	m := &sysctl.Module{Runner: r, Dir: dir}

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "applied",
		Params: mustStruct(t, appliedParams(map[string]any{"vm.swappiness": "1"}, nil)),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Changed {
		t.Fatal("Changed=false при drift контента")
	}
	got, _ := os.ReadFile(path)
	if string(got) != "vm.swappiness = 1\n" {
		t.Fatalf("drop-in не перезаписан: %q", string(got))
	}
	if !reloadCalled(r) {
		t.Fatal("reload не вызван при file-change")
	}
}

// TestApplied_ReloadAlways_NoFileChange — reload=always still calls sysctl -p
// even without a file-change; changed=false (reload itself doesn't mark changed).
func TestApplied_ReloadAlways_NoFileChange(t *testing.T) {
	dir := t.TempDir()
	path := dropInPath(dir, "30-redis")
	if err := os.WriteFile(path, []byte("vm.swappiness = 1\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	r := internaltest.NewRunner()
	r.On("sysctl -p "+path, util.Result{ExitCode: 0})
	m := &sysctl.Module{Runner: r, Dir: dir}

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "applied",
		Params: mustStruct(t, appliedParams(map[string]any{"vm.swappiness": "1"}, map[string]any{"reload": "always"})),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stream.Last().Changed {
		t.Fatal("reload=always пометил changed=true без file-change")
	}
	if !reloadCalled(r) {
		t.Fatal("reload=always не вызвал sysctl -p")
	}
}

// TestApplied_ReloadNever_FileChange — reload=never never calls sysctl -p even
// on file-change (explicit opt-out); changed=true (file is still written).
func TestApplied_ReloadNever_FileChange(t *testing.T) {
	dir := t.TempDir()
	r := internaltest.NewRunner()
	m := &sysctl.Module{Runner: r, Dir: dir}

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "applied",
		Params: mustStruct(t, appliedParams(map[string]any{"vm.swappiness": "1"}, map[string]any{"reload": "never"})),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Changed {
		t.Fatal("Changed=false при создании drop-in (reload=never не влияет на запись)")
	}
	if reloadCalled(r) {
		t.Fatalf("reload=never всё равно вызвал sysctl -p: %v", r.Calls)
	}
}

// TestApplied_IgnoreFailures_AddsDashE — ignore_failures=true → reload via
// `sysctl -e -p <file>` (container mode, silences read-only keys).
func TestApplied_IgnoreFailures_AddsDashE(t *testing.T) {
	dir := t.TempDir()
	path := dropInPath(dir, "30-redis")
	r := internaltest.NewRunner()
	r.On("sysctl -e -p "+path, util.Result{ExitCode: 0})
	m := &sysctl.Module{Runner: r, Dir: dir}

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "applied",
		Params: mustStruct(t, appliedParams(map[string]any{"vm.swappiness": "1"}, map[string]any{"ignore_failures": true})),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Changed {
		t.Fatal("Changed=false")
	}
	var found bool
	for _, c := range r.Calls {
		if c == "sysctl -e -p "+path {
			found = true
		}
		if c == "sysctl -p "+path {
			t.Fatalf("ignore_failures=true, а reload без -e: %v", r.Calls)
		}
	}
	if !found {
		t.Fatalf("ожидался `sysctl -e -p`, calls=%v", r.Calls)
	}
}

// TestApplied_ReloadFails_Fails — a non-zero reload exit code fails the step.
func TestApplied_ReloadFails_Fails(t *testing.T) {
	dir := t.TempDir()
	path := dropInPath(dir, "30-redis")
	r := internaltest.NewRunner()
	r.On("sysctl -p "+path, util.Result{ExitCode: 255, Stderr: "permission denied"})
	m := &sysctl.Module{Runner: r, Dir: dir}

	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "applied",
		Params: mustStruct(t, appliedParams(map[string]any{"vm.swappiness": "1"}, nil)),
	}, stream)
	if !stream.Last().Failed {
		t.Fatal("Failed=false при non-zero exit reload-а")
	}
}

// TestApplied_FilenameSuffix — filename without .conf gets the suffix appended.
func TestApplied_FilenameSuffix(t *testing.T) {
	dir := t.TempDir()
	r := internaltest.NewRunner()
	r.On("sysctl -p "+filepath.Join(dir, "99-custom.conf"), util.Result{ExitCode: 0})
	m := &sysctl.Module{Runner: r, Dir: dir}

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "applied",
		Params: mustStruct(t, map[string]any{
			"filename": "99-custom",
			"settings": map[string]any{"vm.swappiness": "1"},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "99-custom.conf")); err != nil {
		t.Fatalf("drop-in 99-custom.conf не создан: %v", err)
	}
}

// TestApplied_Validate_RequiredAndReloadEnum — settings/filename required;
// reload outside the enum → validation error.
func TestApplied_Validate_RequiredAndReloadEnum(t *testing.T) {
	m := sysctl.New()

	// settings missing → ok=false.
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "applied",
		Params: mustStruct(t, map[string]any{"filename": "30-redis"}),
	})
	if reply.Ok {
		t.Fatal("Validate applied без settings: ok unexpectedly")
	}

	// reload with an unknown value → ok=false.
	bad, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: "applied",
		Params: mustStruct(t, map[string]any{
			"filename": "30-redis",
			"settings": map[string]any{"vm.swappiness": "1"},
			"reload":   "reload-pls",
		}),
	})
	if bad.Ok {
		t.Fatal("Validate applied reload=reload-pls: ok=true (должно быть отклонено)")
	}

	// valid set → ok=true.
	good, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: "applied",
		Params: mustStruct(t, map[string]any{
			"filename": "30-redis",
			"settings": map[string]any{"vm.swappiness": "1"},
			"reload":   "auto",
		}),
	})
	if !good.Ok {
		t.Fatalf("Validate applied valid: ok=false: %v", good.Errors)
	}
}

// TestPlanApplied_Clean_NoMutation — drop-in matches → drift=false, no write,
// no reload (Scry pure-read).
func TestPlanApplied_Clean_NoMutation(t *testing.T) {
	dir := t.TempDir()
	path := dropInPath(dir, "30-redis")
	seed := "vm.swappiness = 1\n"
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	r := internaltest.NewRunner()
	m := &sysctl.Module{Runner: r, Dir: dir}

	stream := &planStream{}
	if err := m.Plan(&pluginv1.PlanRequest{
		State:  "applied",
		Params: mustStruct(t, appliedParams(map[string]any{"vm.swappiness": "1"}, nil)),
	}, stream); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := stream.last(); got == nil || got.GetChanged() {
		t.Fatalf("changed=%v, want false (clean)", got.GetChanged())
	}
	if reloadCalled(r) {
		t.Fatalf("Plan вызвал reload: %v", r.Calls)
	}
	if after, _ := os.ReadFile(path); string(after) != seed {
		t.Fatal("Plan изменил drop-in")
	}
}

// TestPlanApplied_Drift — content differs → drift=true, Plan doesn't write the file.
func TestPlanApplied_Drift(t *testing.T) {
	dir := t.TempDir()
	path := dropInPath(dir, "30-redis")
	if err := os.WriteFile(path, []byte("vm.swappiness = 60\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	r := internaltest.NewRunner()
	m := &sysctl.Module{Runner: r, Dir: dir}

	stream := &planStream{}
	if err := m.Plan(&pluginv1.PlanRequest{
		State:  "applied",
		Params: mustStruct(t, appliedParams(map[string]any{"vm.swappiness": "1"}, nil)),
	}, stream); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := stream.last(); got == nil || !got.GetChanged() {
		t.Fatalf("changed=false, want true (drift)")
	}
	if reloadCalled(r) {
		t.Fatalf("Plan вызвал reload: %v", r.Calls)
	}
}
