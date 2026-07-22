package cloud_test

import (
	"context"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/coremod/cloud"
	"github.com/souls-guild/soul-stack/keeper/internal/pluginhost"
	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"
)

func TestNewPluginAdapter_NilHost(t *testing.T) {
	if _, err := cloud.NewPluginAdapter(nil, nil); err == nil {
		t.Fatal("expected error for nil host")
	}
}

func TestNewPluginAdapter_IndexesByName(t *testing.T) {
	h := &pluginhost.Host{}
	a, err := cloud.NewPluginAdapter(h, []pluginhost.Discovered{
		{Manifest: &sharedplugin.Manifest{Kind: pluginhost.KindCloudDriver, Namespace: "soulstack", Name: "aws"}},
		{Manifest: &sharedplugin.Manifest{Kind: pluginhost.KindCloudDriver, Namespace: "soulstack", Name: "gcp"}},
	})
	if err != nil {
		t.Fatalf("NewPluginAdapter: %v", err)
	}
	got := a.Providers()
	want := map[string]bool{"aws": true, "gcp": true}
	if len(got) != len(want) {
		t.Fatalf("Providers = %v, want %v", got, want)
	}
	for _, n := range got {
		if !want[n] {
			t.Errorf("unexpected provider %q", n)
		}
	}
}

func TestNewPluginAdapter_SkipsNonCloudKinds(t *testing.T) {
	h := &pluginhost.Host{}
	a, err := cloud.NewPluginAdapter(h, []pluginhost.Discovered{
		{Manifest: &sharedplugin.Manifest{Kind: pluginhost.KindCloudDriver, Namespace: "soulstack", Name: "aws"}},
		{Manifest: &sharedplugin.Manifest{Kind: pluginhost.KindSSHProvider, Namespace: "soulstack", Name: "vault-ssh"}},
		{Manifest: nil},
	})
	if err != nil {
		t.Fatalf("NewPluginAdapter: %v", err)
	}
	if got := a.Providers(); len(got) != 1 || got[0] != "aws" {
		t.Errorf("Providers = %v, want [aws]", got)
	}
}

func TestNewPluginAdapter_RejectsDuplicateNames(t *testing.T) {
	h := &pluginhost.Host{}
	_, err := cloud.NewPluginAdapter(h, []pluginhost.Discovered{
		{Manifest: &sharedplugin.Manifest{Kind: pluginhost.KindCloudDriver, Namespace: "a", Name: "aws"}},
		{Manifest: &sharedplugin.Manifest{Kind: pluginhost.KindCloudDriver, Namespace: "b", Name: "aws"}},
	})
	if err == nil {
		t.Fatal("expected duplicate-name error")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("err = %v, want contains 'duplicate'", err)
	}
}

func TestPluginAdapter_UnknownProvider(t *testing.T) {
	h := &pluginhost.Host{}
	a, err := cloud.NewPluginAdapter(h, nil)
	if err != nil {
		t.Fatalf("NewPluginAdapter: %v", err)
	}
	if _, err := a.Create(context.Background(), "missing", nil, nil, 1, "", ""); err == nil {
		t.Fatal("expected unknown-driver error on Create")
	}
	if _, err := a.Destroy(context.Background(), "missing", nil, []string{"vm-1"}); err == nil {
		t.Fatal("expected unknown-driver error on Destroy")
	}
}
