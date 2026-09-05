package pluginhost

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/sdk/schema"
	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"
)

func TestDiscoverAllKinds(t *testing.T) {
	root := t.TempDir()

	// soul_module, a bundle serving two modules under one alias.
	d1 := slot(t, root, "redis")
	writeArtifact(t, d1, "redis", soulModuleDoc(
		modDef("acl", nil, nil),
		modDef("config", nil, nil),
	), exitScript)

	// ssh_provider — a single endpoint, no modules.
	d3 := slot(t, root, "vault-ssh")
	writeArtifact(t, d3, "soul-ssh-vault", schema.Document{
		Kind:            schema.KindSSHProvider,
		ProtocolVersion: 1,
		ProviderKind:    "vault_ssh_ca",
	}, exitScript)

	found, warns, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(warns) != 0 {
		t.Errorf("unexpected warnings: %v", warns)
	}
	// Two modules from the bundle plus one entry for the single-endpoint kind.
	if len(found) != 3 {
		t.Fatalf("found = %d, want 3: %+v", len(found), addressesOf(found))
	}

	byAddr := make(map[string]Discovered, len(found))
	for _, d := range found {
		byAddr[d.Address()] = d
	}
	for _, want := range []string{"redis.acl", "redis.config", "vault-ssh"} {
		if _, ok := byAddr[want]; !ok {
			t.Errorf("address %q missing from %v", want, addressesOf(found))
		}
	}
	if got := byAddr["redis.acl"].Kind(); got != sharedplugin.KindSoulModule {
		t.Errorf("redis.acl kind = %q", got)
	}
	if got := byAddr["vault-ssh"].Kind(); got != sharedplugin.KindSSHProvider {
		t.Errorf("vault-ssh kind = %q", got)
	}
	if byAddr["vault-ssh"].Module != "" {
		t.Errorf("ssh_provider entry has module %q, want empty", byAddr["vault-ssh"].Module)
	}
	// Both entries of the bundle point at the same file with the same digest.
	acl, cfg := byAddr["redis.acl"], byAddr["redis.config"]
	if acl.BinaryPath != cfg.BinaryPath || acl.Digest != cfg.Digest {
		t.Errorf("bundle entries disagree on the artifact: %+v vs %+v", acl, cfg)
	}
	if acl.Digest == "" {
		t.Error("digest not computed")
	}
}

// The alias comes from the SLOT, never from the artifact: the same bytes registered
// twice yield two address spaces and never collide.
func TestDiscoverAliasComesFromTheSlot(t *testing.T) {
	root := t.TempDir()
	doc := soulModuleDoc(modDef("acl", nil, nil))

	writeArtifact(t, slot(t, root, "redis"), "artifact", doc, exitScript)
	writeArtifact(t, slot(t, root, "redis-community"), "artifact", doc, exitScript)

	found, warns, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(warns) != 0 {
		t.Errorf("unexpected warnings: %v", warns)
	}
	got := addressesOf(found)
	if len(got) != 2 || !contains(got, "redis.acl") || !contains(got, "redis-community.acl") {
		t.Fatalf("addresses = %v, want redis.acl and redis-community.acl", got)
	}
}

// DiscoverSlot takes the alias as an argument rather than deriving it from the
// directory: the Keeper host reaches a slot through a `current` symlink whose basename
// says nothing about the registration.
func TestDiscoverSlotAliasIsIndependentOfDirectoryName(t *testing.T) {
	dir := t.TempDir()
	writeArtifact(t, dir, "artifact", soulModuleDoc(modDef("acl", nil, nil)), exitScript)

	found, warns := DiscoverSlot("redis", dir)
	if len(warns) != 0 {
		t.Fatalf("unexpected warnings: %v", warns)
	}
	if len(found) != 1 || found[0].Address() != "redis.acl" {
		t.Fatalf("addresses = %v, want [redis.acl]", addressesOf(found))
	}
}

// GUARD: an artifact that was never stamped has no readable disclosure, and a slot
// with no readable disclosure is skipped. Not defaulted to an empty schema, not
// guessed at.
func TestDiscoverSlotMissingTrailerFailsClosed(t *testing.T) {
	dir := t.TempDir()
	writeBareExecutable(t, dir, "artifact")

	found, warns := DiscoverSlot("redis", dir)
	if len(found) != 0 {
		t.Fatalf("unstamped artifact was discovered: %+v", found)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "trailer") {
		t.Fatalf("warnings = %v, want one mentioning the trailer", warns)
	}
}

// GUARD: a trailer whose bytes were damaged after stamping is an error, never a
// partial read. Truncating the artifact leaves the magic in place with a length that
// no longer fits.
func TestDiscoverSlotCorruptTrailerFailsClosed(t *testing.T) {
	dir := t.TempDir()
	path := writeArtifact(t, dir, "artifact", soulModuleDoc(modDef("acl", nil, nil)), exitScript)

	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// Cut out the middle of the payload: the footer (length + magic) survives, so the
	// trailer still announces more payload than the file now holds.
	if err := os.Truncate(path, st.Size()-16); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	// Put the footer back at the new end so the magic is found and only the length
	// is wrong.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := os.WriteFile(path, append(raw, footerFor(1<<20)...), 0o755); err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	found, warns := DiscoverSlot("redis", dir)
	if len(found) != 0 {
		t.Fatalf("corrupt artifact was discovered: %+v", found)
	}
	if len(warns) != 1 {
		t.Fatalf("warnings = %v, want exactly one", warns)
	}
}

// GUARD: no fallback to a sibling file. A slot with a perfectly good schema.json next
// to an unstamped artifact is still refused — anyone who can write into the slot could
// have put that file there, and it is not what the signature covers.
func TestDiscoverSlotDoesNotFallBackToSchemaFile(t *testing.T) {
	dir := t.TempDir()
	writeBareExecutable(t, dir, "artifact")

	payload, err := schema.Marshal(soulModuleDoc(modDef("acl", nil, nil)))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, sharedplugin.SchemaFileName), payload, 0o644); err != nil {
		t.Fatalf("write schema.json: %v", err)
	}

	found, warns := DiscoverSlot("redis", dir)
	if len(found) != 0 {
		t.Fatalf("slot resolved from the sibling schema.json: %+v", found)
	}
	if len(warns) != 1 {
		t.Fatalf("warnings = %v, want exactly one", warns)
	}
}

// A slot holds exactly one artifact. Two executables is ambiguous, and picking one
// would let directory listing order decide which code runs.
func TestDiscoverSlotRefusesTwoExecutables(t *testing.T) {
	dir := t.TempDir()
	doc := soulModuleDoc(modDef("acl", nil, nil))
	writeArtifact(t, dir, "artifact-a", doc, exitScript)
	writeArtifact(t, dir, "artifact-b", doc, exitScript)

	found, warns := DiscoverSlot("redis", dir)
	if len(found) != 0 {
		t.Fatalf("ambiguous slot was discovered: %+v", found)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "exactly one") {
		t.Fatalf("warnings = %v, want one about the one-artifact rule", warns)
	}
}

func TestDiscoverSlotRefusesEmptySlot(t *testing.T) {
	dir := t.TempDir()
	// A non-executable file is not an artifact.
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	found, warns := DiscoverSlot("redis", dir)
	if len(found) != 0 {
		t.Fatalf("empty slot was discovered: %+v", found)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "no executable") {
		t.Fatalf("warnings = %v, want one about the missing artifact", warns)
	}
}

// The digest sidecar sits in the slot beside the artifact and must not be mistaken for
// one — otherwise every slot would look ambiguous after its first spawn.
func TestDiscoverSlotIgnoresDigestSidecar(t *testing.T) {
	dir := t.TempDir()
	writeArtifact(t, dir, "artifact", soulModuleDoc(modDef("acl", nil, nil)), exitScript)
	// Written executable on purpose: even so it is a dot-file, and dot-files are not
	// artifacts.
	if err := os.WriteFile(filepath.Join(dir, DigestSidecarName), []byte("deadbeef"), 0o755); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}

	found, warns := DiscoverSlot("redis", dir)
	if len(warns) != 0 || len(found) != 1 {
		t.Fatalf("found = %v, warns = %v", addressesOf(found), warns)
	}
}

// A document that parses but does not validate is refused as firmly as one that does
// not parse: a duplicate module name would make an address ambiguous.
func TestDiscoverSlotRefusesInvalidDocument(t *testing.T) {
	dir := t.TempDir()
	writeArtifact(t, dir, "artifact", soulModuleDoc(
		modDef("acl", nil, nil),
		modDef("acl", nil, nil),
	), exitScript)

	found, warns := DiscoverSlot("redis", dir)
	if len(found) != 0 {
		t.Fatalf("invalid document was discovered: %+v", found)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "module_name_duplicate") {
		t.Fatalf("warnings = %v, want one about the duplicate module", warns)
	}
}

// GUARD: an entry discloses ITS OWN module's capabilities and side effects, never the
// union across the artifact. `acl` opens a socket, `config` writes a file; neither
// entry may claim the other's footprint.
func TestDiscoveredDisclosureIsPerModule(t *testing.T) {
	found := discoveredFor(t, "redis", soulModuleDoc(
		modDef("acl",
			[]sharedplugin.Capability{schema.NetworkOutbound},
			[]sharedplugin.SideEffect{{User: "redis_acl_user"}}),
		modDef("config",
			[]sharedplugin.Capability{schema.FSWriteRoot},
			[]sharedplugin.SideEffect{{File: "/etc/redis/redis.conf"}}),
	), exitScript)

	byAddr := make(map[string]Discovered, len(found))
	for _, d := range found {
		byAddr[d.Address()] = d
	}

	acl, cfg := byAddr["redis.acl"], byAddr["redis.config"]
	if got := acl.Capabilities(); len(got) != 1 || got[0] != schema.NetworkOutbound {
		t.Errorf("acl capabilities = %v, want [network_outbound] only", got)
	}
	if got := cfg.Capabilities(); len(got) != 1 || got[0] != schema.FSWriteRoot {
		t.Errorf("config capabilities = %v, want [fs_write_root] only", got)
	}
	if got := acl.SideEffects(); len(got) != 1 || got[0].User != "redis_acl_user" {
		t.Errorf("acl side effects = %v, want the acl user only", got)
	}
	if got := cfg.SideEffects(); len(got) != 1 || got[0].File == "" {
		t.Errorf("config side effects = %v, want the config file only", got)
	}
}

// A single-endpoint kind has no module, so it has no per-module declaration to read.
func TestDiscoveredNoModuleDeclarationForSingleEndpointKind(t *testing.T) {
	found := discoveredFor(t, "aws", schema.Document{
		Kind:            schema.KindSSHProvider,
		ProtocolVersion: 1,
		ProviderKind:    "static_key",
	}, exitScript)

	if len(found) != 1 {
		t.Fatalf("found = %d, want 1", len(found))
	}
	if _, ok := found[0].ModuleDef(); ok {
		t.Error("ModuleDef reported a declaration for a module-less kind")
	}
	if found[0].Capabilities() != nil || found[0].SideEffects() != nil {
		t.Error("module-less kind disclosed capabilities or side effects")
	}
	if found[0].Address() != "aws" {
		t.Errorf("Address = %q, want the bare alias", found[0].Address())
	}
}

func TestDiscoverMissingRootIsFatal(t *testing.T) {
	_, _, err := Discover(filepath.Join(t.TempDir(), "nope"))
	if err == nil {
		t.Fatal("expected an error for a missing cache root")
	}
}

func TestFilterByKindsEmptyAllowedReturnsAll(t *testing.T) {
	// TWO DIFFERENT kinds on purpose: "an empty allow-list keeps everything" is only
	// tested by a set the filter could have split. With both entries the same kind, a
	// regression that hard-coded one kind as always-allowed would still pass.
	input := []Discovered{
		{Doc: &sharedplugin.Document{Kind: sharedplugin.KindSSHProvider}, Alias: "vault-ssh"},
		{Doc: &sharedplugin.Document{Kind: sharedplugin.KindSoulModule}, Alias: "redis"},
	}
	out, warns := FilterByKinds(input, nil)
	if len(out) != 2 || len(warns) != 0 {
		t.Fatalf("out = %d, warns = %v", len(out), warns)
	}
}

func TestFilterByKindsWarningMentionsKind(t *testing.T) {
	input := []Discovered{
		{Dir: "/some/dir", Doc: &sharedplugin.Document{Kind: sharedplugin.KindSSHProvider}, Alias: "vault-ssh"},
	}
	out, warns := FilterByKinds(input, []sharedplugin.Kind{sharedplugin.KindSoulModule})
	if len(out) != 0 {
		t.Fatalf("out = %d, want 0", len(out))
	}
	if len(warns) != 1 || !strings.Contains(warns[0], string(sharedplugin.KindSSHProvider)) {
		t.Fatalf("warns = %v", warns)
	}
}

func addressesOf(ds []Discovered) []string {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.Address())
	}
	return out
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// footerFor builds a trailer footer announcing a payload of n bytes.
func footerFor(n uint64) []byte {
	footer := make([]byte, 8, 8+len(schema.TrailerMagic))
	for i := range 8 {
		footer[i] = byte(n >> (8 * (7 - i)))
	}
	return append(footer, schema.TrailerMagic...)
}
