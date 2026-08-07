package pluginhost

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/sdk/schema"
	"github.com/souls-guild/soul-stack/shared/config"
)

// sshProviderDoc / soulModuleDoc build minimally valid documents of the two other kinds
// keeper discovers. Like [cloudDriverDoc] they declare no name of their own.
func sshProviderDoc() schema.Document {
	return schema.Document{
		Kind:            schema.KindSSHProvider,
		ProtocolVersion: 1,
		ProviderKind:    "vault_ssh_ca",
	}
}

func soulModuleDoc(modules ...string) schema.Document {
	mods := make([]schema.Module, 0, len(modules))
	for _, name := range modules {
		mods = append(mods, schema.Module{
			Name:   name,
			States: map[string]schema.State{"present": {Description: "the resource exists"}},
		})
	}
	return schema.Document{Kind: schema.KindSoulModule, ProtocolVersion: 1, Modules: mods}
}

func TestDiscoverFiltersKeeperKinds(t *testing.T) {
	// Lay out all three kinds in R-nested slots, each named by its REGISTRATION
	// ALIAS. Keeper-host discovers cloud+ssh+soul_module (S1 epic
	// core.module.installed: keeper is the registry of SoulModule artifacts for
	// distribution to Souls).
	root := t.TempDir()

	cloud := cloudDriverDoc()
	writeSlot(t, root, "aws", &cloud, "aws")

	ssh := sshProviderDoc()
	writeSlot(t, root, "vault-ssh", &ssh, "vault-ssh")

	mod := soulModuleDoc("failover")
	writeSlot(t, root, "redis-failover", &mod, "redis-failover")

	found, warns, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(found) != 3 {
		t.Fatalf("found = %d, want 3 (cloud+ssh+soul_module): %v", len(found), found)
	}
	if len(warns) != 0 {
		t.Errorf("warns = %d, want 0: %v", len(warns), warns)
	}
	kinds := map[schema.Kind]bool{}
	aliases := map[string]bool{}
	for _, d := range found {
		kinds[d.Kind()] = true
		aliases[d.Alias] = true
	}
	if !kinds[KindSoulModule] {
		t.Errorf("soul_module not discovered: %v", kinds)
	}
	// Address level 1 comes from the SLOT directory, not from the artifact or from
	// the `current` symlink's basename (which is a commit sha).
	for _, want := range []string{"aws", "vault-ssh", "redis-failover"} {
		if !aliases[want] {
			t.Errorf("alias %q missing from discovery: %v", want, aliases)
		}
	}
}

// TestDiscoverYieldsOneEntryPerModule — an artifact serving three modules is three
// addressable entries, because the unit a host spawns and a task addresses is the
// module, not the file.
func TestDiscoverYieldsOneEntryPerModule(t *testing.T) {
	root := t.TempDir()
	mod := soulModuleDoc("acl", "config", "info")
	writeSlot(t, root, "redis", &mod, "redis")

	found, _, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(found) != 3 {
		t.Fatalf("found = %d, want one entry per module", len(found))
	}
	addrs := map[string]bool{}
	for _, d := range found {
		addrs[d.Address()] = true
	}
	for _, want := range []string{"redis.acl", "redis.config", "redis.info"} {
		if !addrs[want] {
			t.Errorf("address %q missing: %v", want, addrs)
		}
	}
}

// TestDiscoverSkipsAmbiguousSlot — a slot with two executables is skipped with a
// warning rather than resolved by listing order.
func TestDiscoverSkipsAmbiguousSlot(t *testing.T) {
	root := t.TempDir()
	cloud := cloudDriverDoc()
	writeSlot(t, root, "aws", &cloud, "artifact-a", "artifact-b")

	found, warns, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(found) != 0 {
		t.Fatalf("found = %d, want 0 (ambiguous slot must be skipped): %v", len(found), found)
	}
	if len(warns) == 0 {
		t.Error("an ambiguous slot must produce a warning, not a silent skip")
	}
}

func TestDiscoverRootMissing(t *testing.T) {
	_, _, err := Discover(filepath.Join(t.TempDir(), "no-such-dir"))
	if err == nil {
		t.Fatal("expected error for missing root")
	}
}

func TestFilterByCatalog(t *testing.T) {
	// Discovered entries of three kinds; the keeper.yml catalog declares only aws
	// (cloud), vault-ssh (ssh) and redis (soul_module). The comparison is on the
	// ALIAS — the artifact has no name of its own to compare instead.
	mkCloud := func(alias string) Discovered {
		doc := cloudDriverDoc()
		return Discovered{Alias: alias, Doc: &doc}
	}
	mkSSH := func(alias string) Discovered {
		doc := sshProviderDoc()
		return Discovered{Alias: alias, Doc: &doc}
	}
	mkMod := func(alias, module string) Discovered {
		doc := soulModuleDoc(module)
		return Discovered{Alias: alias, Module: module, Doc: &doc}
	}
	found := []Discovered{
		mkCloud("aws"),
		mkCloud("gcp"),
		mkSSH("vault-ssh"),
		mkSSH("teleport"),
		mkMod("redis", "acl"),
		mkMod("postgres", "role"),
	}
	plugins := &config.KeeperPlugins{
		CloudDrivers: []config.PluginCatalogEntry{
			{Name: "aws", Source: "git@example.com:soul-cloud-aws.git", Ref: "v1.0.0"},
			{Name: "yc", Source: "git@example.com:soul-cloud-yc.git", Ref: "v0.1.0"}, // not in cache
		},
		SSHProviders: []config.PluginCatalogEntry{
			{Name: "vault-ssh", Source: "git@example.com:soul-ssh-vault.git", Ref: "v1.0.0"},
		},
		SoulModules: []config.PluginCatalogEntry{
			{Name: "redis", Source: "git@example.com:community-redis.git", Ref: "v1.0.0"},
			{Name: "mongo", Source: "git@example.com:community-mongo.git", Ref: "v0.1.0"}, // not in cache
		},
	}

	out, warns := FilterByCatalog(found, plugins)
	if len(out) != 3 {
		t.Fatalf("out = %d, want 3: %v", len(out), out)
	}
	aliases := map[string]bool{}
	for _, d := range out {
		aliases[d.Alias] = true
	}
	if !aliases["aws"] || !aliases["vault-ssh"] || !aliases["redis"] {
		t.Errorf("expected aws+vault-ssh+redis, got %v", aliases)
	}

	// Warnings: gcp/teleport/postgres not declared; yc/mongo declared but not in cache.
	var gotGcp, gotTeleport, gotYc, gotPostgres, gotMongo bool
	for _, w := range warns {
		switch {
		case strings.Contains(w, "plugin gcp"):
			gotGcp = true
		case strings.Contains(w, "plugin teleport"):
			gotTeleport = true
		case strings.Contains(w, "name=yc"):
			gotYc = true
		case strings.Contains(w, "plugin postgres"):
			gotPostgres = true
		case strings.Contains(w, "name=mongo"):
			gotMongo = true
		}
	}
	if !gotGcp || !gotTeleport || !gotYc || !gotPostgres || !gotMongo {
		t.Errorf("missing warnings (gcp=%v teleport=%v yc=%v postgres=%v mongo=%v): %v",
			gotGcp, gotTeleport, gotYc, gotPostgres, gotMongo, warns)
	}
}

// TestFilterByCatalogWarnsOncePerAlias — an undeclared artifact serving three modules
// is ONE registration the operator forgot, so it is one warning and not three.
func TestFilterByCatalogWarnsOncePerAlias(t *testing.T) {
	doc := soulModuleDoc("acl", "config", "info")
	found := []Discovered{
		{Alias: "redis", Module: "acl", Doc: &doc},
		{Alias: "redis", Module: "config", Doc: &doc},
		{Alias: "redis", Module: "info", Doc: &doc},
	}
	_, warns := FilterByCatalog(found, &config.KeeperPlugins{})
	if len(warns) != 1 {
		t.Fatalf("warns = %d, want 1 per undeclared alias: %v", len(warns), warns)
	}
}

func TestFilterByCatalogNil(t *testing.T) {
	doc := cloudDriverDoc()
	found := []Discovered{{Alias: "aws", Doc: &doc}}
	out, warns := FilterByCatalog(found, nil)
	if len(out) != 0 {
		t.Errorf("expected 0 with nil catalog, got %d", len(out))
	}
	if len(warns) != 0 {
		t.Errorf("expected no warnings with nil catalog, got %v", warns)
	}
}
