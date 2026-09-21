package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// Guard for the UPGRADE path of a removed config key (NIM-761).
//
// `plugins.cloud_drivers` was deleted from [KeeperPlugins] with the CloudDriver
// contract. Every keeper.yml written before that release still carries the block,
// and the reflect walker's default answer to a key the struct does not have is an
// `unknown_key` at error level — which `keeper run` exits on. So the removal, on
// its own, would have turned an upgrade into an outage on every deployment that
// ever registered a cloud driver, to enforce the absence of a list nothing reads.
//
// The invariant, and what makes it a guard rather than a restatement: a removed
// key must be diagnosed WITHOUT raising the error level. The test would fail on
// the obvious regression (dropping the `removedKeys` entry, so the generic
// unknown_key path takes over) and on the subtle one (keeping the entry but
// raising it as an error, which reads as "diagnosed" and still refuses to boot).
//
// The fixture is the SHIPPED reference config with the block put back, not a
// hand-written minimum: what an upgrading operator actually has is a working
// keeper.yml plus the dead key, and a minimum of my own invention could pass this
// while the real thing still failed on something else in the same phase.
const removedCloudDriversBlock = `  cloud_drivers:
    - { name: aws, source: "git@example.com:aws.git", ref: v2.0.0 }
`

// keeperYAMLWithRemovedKey re-inserts the removed block into the reference config,
// directly above the `ssh_providers:` list it used to sit beside.
func keeperYAMLWithRemovedKey(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "examples", "keeper", "keeper.yml"))
	if err != nil {
		t.Fatalf("read the shipped reference config: %v", err)
	}
	const anchor = "  ssh_providers:\n"
	s := string(raw)
	if !strings.Contains(s, anchor) {
		t.Fatalf("the reference config no longer has a `plugins.ssh_providers` list to anchor on; " +
			"the fixture below would be testing a config shape nobody ships")
	}
	return []byte(strings.Replace(s, anchor, removedCloudDriversBlock+anchor, 1))
}

func TestRemovedCloudDriversKeyDoesNotStopTheKeeper(t *testing.T) {
	_, _, ds, err := LoadKeeperFromBytes("keeper.yml", keeperYAMLWithRemovedKey(t), ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadKeeperFromBytes: %v", err)
	}

	var found *diag.Diagnostic
	for i := range ds {
		if strings.Contains(ds[i].Message, "cloud_drivers") {
			found = &ds[i]
		}
	}
	if found == nil {
		t.Fatalf("no diagnostic mentions cloud_drivers — the key is now accepted in silence, "+
			"so an operator carrying the dead block is never told to delete it: %+v", ds)
	}
	if found.Level == diag.LevelError {
		t.Errorf("cloud_drivers is diagnosed at error level (%q %q) — `keeper run` exits on the first "+
			"error, so every deployment whose keeper.yml still names it fails to start after the upgrade",
			found.Code, found.Message)
	}
	if found.Hint == "" {
		t.Errorf("the cloud_drivers diagnostic carries no hint; an operator is told the key is wrong "+
			"but not what to do with the entries: %+v", found)
	}
	if !strings.Contains(found.Hint, "soul_modules") {
		t.Errorf("the hint does not name the replacement (`plugins.soul_modules`): %q", found.Hint)
	}

	// The load itself must survive: this is the half that decides whether the
	// keeper boots, and it is separate from how the key was reported.
	if diag.HasErrors(ds) {
		t.Errorf("the config carries error-level diagnostics, so `keeper run` would refuse to start: %+v", ds)
	}
}

// The suppression must be narrow. A genuinely misspelled key under the same block
// stays an error — otherwise the fix above would have bought upgrade safety by
// turning every typo in `plugins:` into a warning nobody reads.
func TestUnknownPluginKeyIsStillAnError(t *testing.T) {
	yaml := strings.Replace(string(keeperYAMLWithRemovedKey(t)), "cloud_drivers:", "sould_modules:", 1)
	_, _, ds, err := LoadKeeperFromBytes("keeper.yml", []byte(yaml), ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadKeeperFromBytes: %v", err)
	}
	for i := range ds {
		if ds[i].Code == "unknown_key" && strings.Contains(ds[i].Message, "sould_modules") {
			return
		}
	}
	t.Errorf("a misspelled key under plugins: produced no unknown_key error: %+v", ds)
}
