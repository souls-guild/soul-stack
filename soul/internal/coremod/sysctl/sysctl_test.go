package sysctl_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/internaltest"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/sysctl"
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

func TestValidate(t *testing.T) {
	m := sysctl.New()
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "present",
		Params: mustStruct(t, map[string]any{"name": "net.ipv4.ip_forward"}),
	})
	if reply.Ok {
		t.Fatal("Validate без value: ok unexpectedly")
	}
}

func TestApply_RuntimeAndPersist_NewValue(t *testing.T) {
	dir := t.TempDir()
	r := internaltest.NewRunner()
	r.On("sysctl -n net.ipv4.ip_forward", util.Result{ExitCode: 0, Stdout: "0\n"})
	r.On("sysctl -w net.ipv4.ip_forward=1", util.Result{ExitCode: 0})
	m := &sysctl.Module{Runner: r, Dir: dir}

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "present",
		Params: mustStruct(t, map[string]any{
			"name":  "net.ipv4.ip_forward",
			"value": "1",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Changed {
		t.Fatal("Changed=false при new value")
	}
	want := filepath.Join(dir, "net-ipv4-ip_forward.conf")
	got, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("persist file: %v", err)
	}
	if string(got) != "net.ipv4.ip_forward = 1\n" {
		t.Fatalf("content=%q", string(got))
	}
}

func TestApply_Idempotent_RuntimeMatchesAndPersistExists(t *testing.T) {
	dir := t.TempDir()
	persist := filepath.Join(dir, "net-ipv4-ip_forward.conf")
	if err := os.WriteFile(persist, []byte("net.ipv4.ip_forward = 1\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	r := internaltest.NewRunner()
	r.On("sysctl -n net.ipv4.ip_forward", util.Result{ExitCode: 0, Stdout: "1\n"})
	m := &sysctl.Module{Runner: r, Dir: dir}

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "present",
		Params: mustStruct(t, map[string]any{
			"name":  "net.ipv4.ip_forward",
			"value": "1",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stream.Last().Changed {
		t.Fatal("Changed=true для already-set значения")
	}
	for _, c := range r.Calls {
		if c == "sysctl -w net.ipv4.ip_forward=1" {
			t.Fatalf("неожиданный sysctl -w для уже выставленного")
		}
	}
}

func TestApply_RuntimeMatches_PersistMissing_StillChanges(t *testing.T) {
	dir := t.TempDir()
	r := internaltest.NewRunner()
	r.On("sysctl -n net.ipv4.ip_forward", util.Result{ExitCode: 0, Stdout: "1"})
	m := &sysctl.Module{Runner: r, Dir: dir}

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "present",
		Params: mustStruct(t, map[string]any{
			"name":  "net.ipv4.ip_forward",
			"value": "1",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Changed {
		t.Fatal("Changed=false когда persist-файл отсутствовал")
	}
}

func TestApply_CustomFilename(t *testing.T) {
	dir := t.TempDir()
	r := internaltest.NewRunner()
	r.On("sysctl -n vm.swappiness", util.Result{ExitCode: 0, Stdout: "60\n"})
	r.On("sysctl -w vm.swappiness=10", util.Result{ExitCode: 0})
	m := &sysctl.Module{Runner: r, Dir: dir}

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "present",
		Params: mustStruct(t, map[string]any{
			"name":     "vm.swappiness",
			"value":    "10",
			"filename": "99-tuning",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Changed {
		t.Fatal("Changed=false")
	}
	if _, err := os.Stat(filepath.Join(dir, "99-tuning.conf")); err != nil {
		t.Fatalf("custom filename не создан: %v", err)
	}
}

func TestApply_MultiValueNormalization(t *testing.T) {
	dir := t.TempDir()
	persist := filepath.Join(dir, "net-ipv4-tcp_keepalive.conf")
	if err := os.WriteFile(persist, []byte("net.ipv4.tcp_keepalive = 1 0\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	r := internaltest.NewRunner()
	// kernel returns values separated by a tab
	r.On("sysctl -n net.ipv4.tcp_keepalive", util.Result{ExitCode: 0, Stdout: "1\t0\n"})
	m := &sysctl.Module{Runner: r, Dir: dir}

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "present",
		Params: mustStruct(t, map[string]any{
			"name":  "net.ipv4.tcp_keepalive",
			"value": "1 0",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stream.Last().Changed {
		t.Fatal("Changed=true несмотря на эквивалентный multi-value")
	}
}

func TestApply_SysctlWriteFails(t *testing.T) {
	dir := t.TempDir()
	r := internaltest.NewRunner()
	r.On("sysctl -n net.ipv4.ip_forward", util.Result{ExitCode: 0, Stdout: "0"})
	r.On("sysctl -w net.ipv4.ip_forward=1", util.Result{ExitCode: 255, Stderr: "permission denied"})
	m := &sysctl.Module{Runner: r, Dir: dir}

	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "present",
		Params: mustStruct(t, map[string]any{
			"name":  "net.ipv4.ip_forward",
			"value": "1",
		}),
	}, stream)
	if !stream.Last().Failed {
		t.Fatal("Failed=false при sysctl -w error")
	}
}
