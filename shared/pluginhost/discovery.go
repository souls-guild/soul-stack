package pluginhost

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/souls-guild/soul-stack/shared/diag"
	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"
)

// Discovered is one plugin found in the host cache. The binary and manifest
// live in the same directory (ADR-020(a): "manifest.yaml … next to the binary
// in the host cache").
type Discovered struct {
	// Manifest is the parsed and validated manifest.yaml.
	Manifest *sharedplugin.Manifest
	// BinaryPath is the absolute path to the plugin executable.
	BinaryPath string
	// Dir is the directory holding manifest.yaml and the binary (for logs).
	Dir string
	// Digest is the binary's SHA-256 (hex), computed during Discover. Used for
	// logs/OTel attributes; the authoritative integrity check is done in
	// [Host.Spawn] against the sidecar (security fix H2). An empty string means
	// the binary could not be read for the digest (goes into warnings, the
	// plugin is skipped).
	Digest string
}

// Discover looks for plugins in the root directory cacheRoot.
//
// Host cache layout (docs/soul/modules.md, docs/keeper/plugins.md):
//
//	<cacheRoot>/
//	  <namespace>-<name>/
//	    manifest.yaml
//	    soul-mod-<name>         # for kind=soul_module
//	    soul-cloud-<name>       # for kind=cloud_driver
//	    soul-ssh-<name>         # for kind=ssh_provider
//
// Discover **does not filter by kind** — that is the caller's job (soul-host
// accepts only soul_module, keeper-host only cloud_driver and ssh_provider).
// See [FilterByKinds].
//
// The binary name follows the [sharedplugin.Manifest.BinaryName] convention;
// directories where the binary is missing or lacks +x go into warnings but do
// not stop the walk.
//
// Read errors on individual directories do not stop the walk — they are
// collected into warnings, and Discover returns whatever it found. The only
// fatal error is failing to read cacheRoot itself (e.g. ENOENT).
func Discover(cacheRoot string) ([]Discovered, []string, error) {
	entries, err := os.ReadDir(cacheRoot)
	if err != nil {
		return nil, nil, fmt.Errorf("pluginhost: read plugin cache root %q: %w", cacheRoot, err)
	}
	var (
		out      []Discovered
		warnings []string
	)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		found, warns := DiscoverSlot(filepath.Join(cacheRoot, e.Name()))
		out = append(out, found...)
		warnings = append(warnings, warns...)
	}
	return out, warnings, nil
}

// DiscoverSlot reads a plugin from a SINGLE slot directory `dir` (manifest.yaml
// plus the binary by BinaryName convention next to it). Returns (0..1
// Discovered, warnings): an invalid manifest / a missing or non-executable
// binary → empty result + warning.
//
// Split out of [Discover] for the Keeper-host nested layout (A1-S1): keeper
// discovers via a current-symlink (`<ns>-<name>/current`) pointing at the
// active slot directory rather than the cache root.
func DiscoverSlot(dir string) ([]Discovered, []string) {
	manifestPath := filepath.Join(dir, sharedplugin.FileName)
	m, diags, ioErr := sharedplugin.Load(manifestPath)
	if ioErr != nil {
		return nil, []string{fmt.Sprintf("skip %s: %v", dir, ioErr)}
	}
	if err := firstDiagError(diags); err != nil {
		return nil, []string{fmt.Sprintf("skip %s: %v", dir, err)}
	}
	binName := m.BinaryName()
	if binName == "" {
		return nil, []string{fmt.Sprintf("skip %s: no binary convention for kind=%q", dir, m.Kind)}
	}
	binPath := filepath.Join(dir, binName)
	st, err := os.Stat(binPath)
	if err != nil {
		return nil, []string{fmt.Sprintf("skip %s: binary %s not found: %v", dir, binName, err)}
	}
	if st.IsDir() {
		return nil, []string{fmt.Sprintf("skip %s: %s is a directory", dir, binName)}
	}
	if st.Mode().Perm()&0o111 == 0 {
		return nil, []string{fmt.Sprintf("skip %s: %s is not executable (mode %o)", dir, binName, st.Mode().Perm())}
	}
	digest, err := computeFileDigest(binPath)
	if err != nil {
		return nil, []string{fmt.Sprintf("skip %s: digest %s: %v", dir, binName, err)}
	}
	return []Discovered{{
		Manifest:   m,
		BinaryPath: binPath,
		Dir:        dir,
		Digest:     digest,
	}}, nil
}

// FilterByKinds keeps in discovered only plugins whose manifest.kind is in
// allowedKinds. Rejected entries go into warnings with a human-readable
// message. Returns (filtered list, warnings).
//
// Convenient right after [Discover]:
//
//	found, w1, err := pluginhost.Discover(root)
//	found, w2 := pluginhost.FilterByKinds(found, []string{sharedplugin.KindSoulModule})
//	warnings := append(w1, w2...)
func FilterByKinds(discovered []Discovered, allowedKinds []string) ([]Discovered, []string) {
	if len(allowedKinds) == 0 {
		return discovered, nil
	}
	allowed := make(map[string]struct{}, len(allowedKinds))
	for _, k := range allowedKinds {
		allowed[k] = struct{}{}
	}
	var (
		out      = make([]Discovered, 0, len(discovered))
		warnings []string
	)
	for _, d := range discovered {
		if _, ok := allowed[d.Manifest.Kind]; ok {
			out = append(out, d)
			continue
		}
		warnings = append(warnings, fmt.Sprintf("skip %s: kind=%q not allowed on this host (want %v)",
			d.Dir, d.Manifest.Kind, allowedKinds))
	}
	return out, warnings
}

// firstDiagError joins all error-level diag records into one error with the
// separator `; `. Returns nil if the diagnostics are empty or contain only
// warning/hint. Duplicates [sharedplugin.Manifest.ValidateSimple] logic exactly
// because [sharedplugin.Load] returns errors structurally via diag, while the
// discovery callsite needs an `error` for its warning message.
func firstDiagError(ds []diag.Diagnostic) error {
	var msgs []string
	for _, d := range ds {
		if d.Level != diag.LevelError {
			continue
		}
		msgs = append(msgs, d.Code+": "+d.Message)
	}
	if len(msgs) == 0 {
		return nil
	}
	return errors.New(strings.Join(msgs, "; "))
}
