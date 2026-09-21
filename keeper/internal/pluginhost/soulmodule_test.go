package pluginhost

import (
	"context"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/sdk/schema"
	"github.com/souls-guild/soul-stack/shared/config"
)

// GUARD: the spawn door itself refuses a module that has not declared
// `side: keeper`, BEFORE the fork.
//
// This is the second of two barriers on purpose. The registry never hands a
// Soul-side module to the dispatcher, and the host refuses to start one even if
// something did — a plugin in the Keeper's process tree is the most privileged
// execution the platform has, and one gate for it would be one gate too few.
// The refusal must also happen before Host.Spawn, so a Soul-side artifact is
// never executed and then judged.
func TestSpawnSoulModule_RefusesWithoutSideKeeper(t *testing.T) {
	h, err := NewHost(&config.PluginRuntime{SocketDir: t.TempDir()}, nil, nil)
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	for _, tc := range []struct {
		name string
		side schema.Side
	}{
		{"explicit soul", schema.SideSoul},
		{"absent key", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := h.SpawnSoulModule(context.Background(), keeperSideEntry("redis", "acl", tc.side))
			if err == nil {
				t.Fatal("SpawnSoulModule: nil error, want a refusal")
			}
			if !strings.Contains(err.Error(), "side=soul") {
				t.Errorf("error = %q, want it to name the declared side", err)
			}
		})
	}
}

// An unrecognised value is not the default (`module_side_invalid` is the
// linter's job) — here it simply is not `keeper`, so it does not run. Fail
// closed: `side: Keeper` quietly meaning "runs on the keeper" is the failure
// the enum check exists for on the other end.
func TestSpawnSoulModule_RefusesUnrecognisedSide(t *testing.T) {
	h, err := NewHost(&config.PluginRuntime{SocketDir: t.TempDir()}, nil, nil)
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	if _, err := h.SpawnSoulModule(context.Background(), keeperSideEntry("democloud", "vm", "Keeper")); err == nil {
		t.Fatal("SpawnSoulModule: nil error, want a refusal for a side outside the enum")
	}
}

func TestSpawnSoulModule_RefusesOtherKind(t *testing.T) {
	h, err := NewHost(&config.PluginRuntime{SocketDir: t.TempDir()}, nil, nil)
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	doc := schema.Document{Kind: schema.KindSSHProvider, ProtocolVersion: 1}
	_, err = h.SpawnSoulModule(context.Background(), Discovered{Alias: "aws", Doc: &doc})
	if err == nil || !strings.Contains(err.Error(), "kind=soul_module") {
		t.Fatalf("SpawnSoulModule(ssh_provider) = %v, want a kind refusal", err)
	}
}

// The kind-agnostic Spawn keeps refusing soul_module. It has no side gate, so
// if it started accepting the kind it would be a way around the one above.
func TestSpawn_StillRefusesSoulModule(t *testing.T) {
	h, err := NewHost(&config.PluginRuntime{SocketDir: t.TempDir()}, nil, nil)
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	_, err = h.Spawn(context.Background(), keeperSideEntry("democloud", "vm", schema.SideKeeper))
	if err == nil || !strings.Contains(err.Error(), "ssh_provider") {
		t.Fatalf("Spawn(soul_module) = %v, want the kind refusal", err)
	}
}

func TestDeclaredSide(t *testing.T) {
	if got := DeclaredSide(keeperSideEntry("democloud", "vm", schema.SideKeeper)); got != schema.SideKeeper {
		t.Errorf("DeclaredSide(side: keeper) = %q, want keeper", got)
	}
	if got := DeclaredSide(keeperSideEntry("legacy", "thing", "")); got != schema.SideSoul {
		t.Errorf("DeclaredSide(absent key) = %q, want soul", got)
	}
	// No document, or an entry whose module the document does not describe: the
	// question is "may the Keeper run it", so silence answers soul.
	if got := DeclaredSide(Discovered{Alias: "x"}); got != schema.SideSoul {
		t.Errorf("DeclaredSide(no document) = %q, want soul", got)
	}
}
