package pluginhost

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/souls-guild/soul-stack/shared/diag"
	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"
)

// ErrSlotNotFound indicates cache has no slot `<cacheRoot>/<ns>-<name>/` or it has no
// valid manifest.yaml / binary. Returned by [ReadSlot] when plugin by
// key (namespace, name) not found in host's cache; sigil.Service maps it to
// ErrPluginNotInCache → 404.
var ErrSlotNotFound = errors.New("pluginhost: plugin slot not found in cache")

// CurrentLink is the name of symlink to active commit_sha-slot in R-nested layout
// (ADR-026 F-fetch, A1-S1): `<cacheRoot>/<ns>-<name>/current → <commit_sha>`.
// Resolver ([plugingit.Resolver]) updates it atomically when populating cache.
const CurrentLink = "current"

// SlotContents is the contents of plugin slot in cache, read by key
// (namespace, name): binary path + raw manifest.yaml bytes + SHA-256
// of binary (hex, lowercase).
//
// This is input for Sigil signature (ADR-026): Keeper reads ACTIVE binary+manifest
// of slot `<cacheRoot>/<ns>-<name>/current/` (R-nested layout, A1-S1: `current` is
// symlink to immutable commit_sha-slot populated by git-resolver).
// `ref` in allow-record is operator-asserted label, not used in slot lookup.
type SlotContents struct {
	// BinaryPath is absolute path to plugin executable file.
	BinaryPath string
	// ManifestBytes are RAW manifest.yaml bytes as on disk (without
	// canonicalization: done by sigil.Signer before hashing — S3↔S6 invariant).
	ManifestBytes []byte
	// BinarySHA256 is SHA-256 of binary (hex, lowercase, 64 chars). Passed to
	// Signer.Sign and stored in plugin_sigils.sha256.
	BinarySHA256 string
}

// ReadSlot reads binary+manifest of ACTIVE plugin slot via current-symlink
// `<cacheRoot>/<namespace>-<name>/current/` (R-nested layout, A1-S1). `ref` not
// used in slot lookup — integrity authority = sha256 + Sigil signature.
//
// Steps:
//  1. active slot `<cacheRoot>/<ns>-<name>/current/` (symlink to commit_sha-
//     directory); missing / broken symlink → [ErrSlotNotFound];
//  2. reads manifest.yaml as raw bytes and parses it (needs kind → binary name convention
//     [sharedplugin.Manifest.BinaryName]); invalid manifest → validation error;
//  3. binary by convention next to manifest; missing / not executable →
//     [ErrSlotNotFound];
//  4. streaming SHA-256 of binary.
//
// Read-only: ReadSlot does NOT fork plugin, does NOT touch handshake/Discover,
// does NOT write sidecar. Contract of [Discover]/[Host.Spawn] unaffected.
//
// os.Stat follows current symlink, so st.IsDir() check works for active slot too
// (broken/dangling current gives ENOENT → [ErrSlotNotFound]).
func ReadSlot(cacheRoot, namespace, name string) (*SlotContents, error) {
	dir := filepath.Join(cacheRoot, namespace+"-"+name, CurrentLink)
	st, err := os.Stat(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrSlotNotFound, dir)
		}
		return nil, fmt.Errorf("pluginhost: stat plugin slot %q: %w", dir, err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("%w: %s is not a directory", ErrSlotNotFound, dir)
	}

	manifestPath := filepath.Join(dir, sharedplugin.FileName)
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: no %s in %s", ErrSlotNotFound, sharedplugin.FileName, dir)
		}
		return nil, fmt.Errorf("pluginhost: read %q: %w", manifestPath, err)
	}

	m, diags := sharedplugin.LoadFromBytes(manifestPath, manifestBytes)
	if err := firstManifestError(diags); err != nil {
		return nil, fmt.Errorf("pluginhost: invalid manifest %q: %w", manifestPath, err)
	}
	binName := m.BinaryName()
	if binName == "" {
		return nil, fmt.Errorf("pluginhost: manifest %q has no binary convention for kind=%q", manifestPath, m.Kind)
	}

	binPath := filepath.Join(dir, binName)
	bst, err := os.Stat(binPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: binary %s missing in %s", ErrSlotNotFound, binName, dir)
		}
		return nil, fmt.Errorf("pluginhost: stat binary %q: %w", binPath, err)
	}
	if bst.IsDir() {
		return nil, fmt.Errorf("%w: %s is a directory in %s", ErrSlotNotFound, binName, dir)
	}

	digest, err := fileDigest(binPath)
	if err != nil {
		return nil, err
	}

	return &SlotContents{
		BinaryPath:    binPath,
		ManifestBytes: manifestBytes,
		BinarySHA256:  digest,
	}, nil
}

// SlotCommitSHA reads commit_sha of ACTIVE plugin slot (namespace, name) —
// directory name that symlink `<cacheRoot>/<ns>-<name>/current` points to
// (R-nested layout, A1-S1). commit_sha is audit tag for binary origin,
// filled in plugin_sigils on allow (ADR-026(g), outside signature).
//
// Reads ONLY target of symlink (os.Readlink, without following it): target is
// relative target `<commit_sha>` (see [plugingit.updateCurrentSymlink]),
// so basic name of that is returned. Reading just target not stat of slot
// makes helper cheap and independent of binary/manifest presence (their validity
// already checked by [ReadSlot] at allow step).
//
// fail-closed:
//   - missing directory `<ns>-<name>/` or missing/broken `current` symlink (legacy slot
//     without current, dangling link) → [ErrSlotNotFound];
//   - `current` exists but is not symlink → [ErrSlotNotFound] (R-nested invariant
//     broken: current must be symlink to commit_sha directory).
//
// Returns basic name of target as-is (without 40-hex validation): commit_sha validity
// guaranteed by git-resolver when populating cache; here only reading
// already-fixed value.
func SlotCommitSHA(cacheRoot, namespace, name string) (string, error) {
	link := filepath.Join(cacheRoot, namespace+"-"+name, CurrentLink)
	target, err := os.Readlink(link)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w: %s", ErrSlotNotFound, link)
		}
		// EINVAL (current is not symlink) and other errors — legacy/corrupted slot:
		// commit_sha cannot be reliably extracted, fail-closed.
		return "", fmt.Errorf("%w: read current symlink %q: %v", ErrSlotNotFound, link, err)
	}
	commitSHA := filepath.Base(target)
	if commitSHA == "" || commitSHA == "." || commitSHA == string(filepath.Separator) {
		return "", fmt.Errorf("%w: empty commit_sha in current symlink %q", ErrSlotNotFound, link)
	}
	return commitSHA, nil
}

// fileDigest computes SHA-256 of file streaming (plugin binaries are tens of MB).
// Duplicate of computeFileDigest from shared/pluginhost (unexported there); local
// copy avoids expanding shared's public surface just for reading slot.
func fileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("pluginhost: open binary for digest %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("pluginhost: read binary for digest %q: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// firstManifestError returns first error-level diagnostic from diags
// (diagnostics below error level — warning/hint — ignored:
// manifest valid for signing). nil means no fatal errors.
func firstManifestError(diags []diag.Diagnostic) error {
	for _, d := range diags {
		if d.Level == diag.LevelError {
			return errors.New(d.Code + ": " + d.Message)
		}
	}
	return nil
}
