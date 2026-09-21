package pluginhost

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/souls-guild/soul-stack/sdk/schema"
	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"
)

// Fixtures for a stamped artifact.
//
// Every test in this package that reads a slot goes through here rather than writing a
// trailer by hand: the trailer is what the host trusts, so a fixture that assembled it
// its own way would be testing a format nothing else produces.

// exitScript is the smallest thing a slot can hold that both execs and carries a
// trailer. The shell stops at `exit 0` and never reads into the appended bytes.
const exitScript = "#!/bin/sh\nexit 0\n"

// modDef builds one module declaration with a single `present` state.
func modDef(name string, caps []sharedplugin.Capability, effects []sharedplugin.SideEffect) schema.Module {
	return schema.Module{
		Name:         name,
		Description:  name + " module",
		Capabilities: caps,
		SideEffects:  effects,
		States: map[string]schema.State{
			"present": {
				Description: "the " + name + " resource exists",
				Input: schema.Input{
					"host": {Type: schema.String, Required: true, Description: "host to connect to"},
				},
			},
		},
	}
}

// soulModuleDoc builds a valid kind=soul_module document over the given modules.
func soulModuleDoc(mods ...schema.Module) schema.Document {
	return schema.Document{
		Kind:            schema.KindSoulModule,
		ProtocolVersion: 1,
		Modules:         mods,
	}
}

// writeArtifact writes an executable at dir/name holding script, with doc stamped into
// its trailer, and returns the path.
func writeArtifact(t testing.TB, dir, name string, doc schema.Document, script string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	payload, err := schema.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	if err := schema.WriteTrailerFile(path, payload); err != nil {
		t.Fatalf("stamp artifact: %v", err)
	}
	return path
}

// writeBareExecutable writes an executable with NO trailer — an artifact that was
// never stamped.
func writeBareExecutable(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(exitScript), 0o755); err != nil {
		t.Fatalf("write executable: %v", err)
	}
	return path
}

// slot creates a slot directory named alias under root and returns its path.
func slot(t *testing.T, root, alias string) string {
	t.Helper()
	dir := filepath.Join(root, alias)
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("mkdir slot: %v", err)
	}
	return dir
}

// discoveredFor is the single-slot shortcut used by tests that need a Discovered
// without going through a cache root: it stamps an artifact and reads it back exactly
// as discovery would.
func discoveredFor(t *testing.T, alias string, doc schema.Document, script string) []Discovered {
	t.Helper()
	dir := t.TempDir()
	writeArtifact(t, dir, alias, doc, script)
	found, warns := DiscoverSlot(alias, dir)
	if len(warns) != 0 {
		t.Fatalf("unexpected discovery warnings: %v", warns)
	}
	return found
}
