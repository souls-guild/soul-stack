package pluginhost

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/souls-guild/soul-stack/sdk/schema"
	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"
)

// ErrSlotNotFound indicates the cache has no slot `<cacheRoot>/<alias>/`, or the slot
// holds no readable artifact. Returned by [ReadSlot] when the registration alias
// resolves to nothing usable; sigil.Service maps it to ErrPluginNotInCache → 404.
var ErrSlotNotFound = errors.New("pluginhost: plugin slot not found in cache")

// CurrentLink is the name of the symlink to the active commit_sha-slot in the R-nested
// layout (ADR-026 F-fetch, A1-S1): `<cacheRoot>/<alias>/current → <commit_sha>`.
// The resolver ([plugingit.Resolver]) updates it atomically when it populates the cache.
const CurrentLink = "current"

// SlotContents is what one slot holds, read by REGISTRATION ALIAS: the artifact's
// path, the canonical schema document stamped into it, and the artifact's SHA-256.
//
// This is the input to the Sigil signature (ADR-026): Keeper reads the ACTIVE artifact
// of `<cacheRoot>/<alias>/current/` (R-nested layout, A1-S1: `current` is a symlink to
// the immutable commit_sha-slot the git resolver populated).
//
// The alias is the ONLY lookup key. The artifact carries no self-name since NIM-377 —
// no namespace, no name, no filename convention — so there is nothing else a slot
// could be found by, and the alias is what the grant, the slot and every address
// derived from it agree on. `ref` is an operator-asserted label on the grant and takes
// no part in the lookup.
type SlotContents struct {
	// BinaryPath is the absolute path of the artifact.
	BinaryPath string
	// SchemaBytes are the canonical schema-document bytes read from the artifact's
	// TRAILER — byte-exact, as [sharedhost.SchemaDigest] will hash them. Reading them
	// from a sibling `schema.json` instead would let the signed disclosure differ from
	// the one inside the bytes being approved.
	SchemaBytes []byte
	// Doc is SchemaBytes parsed and validated, for callers that need the kind or the
	// module set without re-parsing. Never nil when ReadSlot returns no error.
	Doc *sharedplugin.Document
	// BinarySHA256 is the artifact's SHA-256 (hex, lowercase, 64 chars). Passed to
	// Signer.Sign and stored in plugin_sigils.sha256.
	BinarySHA256 string
}

// ReadSlot reads the ACTIVE artifact of the slot registered under alias, through the
// current-symlink `<cacheRoot>/<alias>/current/` (R-nested layout, A1-S1).
//
// Steps:
//  1. active slot `<cacheRoot>/<alias>/current/` (a symlink to a commit_sha
//     directory); missing / broken symlink → [ErrSlotNotFound];
//  2. the slot's SINGLE executable — there is no filename to look up, so "exactly
//     one" is the rule and zero or several is an error, never a first-match guess;
//  3. the canonical schema document from the artifact's trailer;
//  4. streaming SHA-256 of the artifact.
//
// # Fail closed
//
// A missing trailer, a malformed trailer or a document that does not validate is an
// ERROR, not a warning: the schema is the disclosure the operator is about to approve,
// and there is no fallback to a sibling file and no empty-document default. An
// artifact whose disclosure cannot be read has not been approved.
//
// Read-only: ReadSlot does NOT fork the plugin and does NOT write the digest sidecar.
//
// os.Stat follows the current symlink, so the IsDir check covers the active slot too
// (a dangling `current` gives ENOENT → [ErrSlotNotFound]).
func ReadSlot(cacheRoot, alias string) (*SlotContents, error) {
	dir := filepath.Join(cacheRoot, alias, CurrentLink)
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

	binPath, err := SingleArtifactIn(dir)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrSlotNotFound, dir, err)
	}

	schemaBytes, err := schema.ReadTrailerFile(binPath)
	if err != nil {
		return nil, fmt.Errorf("pluginhost: read schema of %q: %w", binPath, err)
	}
	doc, diags := sharedplugin.ParseDocument(binPath, schemaBytes)
	if derr := sharedplugin.FirstError(diags); derr != nil {
		return nil, fmt.Errorf("pluginhost: invalid schema document in %q: %w", binPath, derr)
	}
	if doc == nil {
		return nil, fmt.Errorf("pluginhost: artifact %q carries no readable schema document", binPath)
	}

	digest, err := fileDigest(binPath)
	if err != nil {
		return nil, err
	}

	return &SlotContents{
		BinaryPath:   binPath,
		SchemaBytes:  schemaBytes,
		Doc:          doc,
		BinarySHA256: digest,
	}, nil
}

// SlotCommitSHA reads the commit_sha of the ACTIVE slot registered under alias — the
// directory name the symlink `<cacheRoot>/<alias>/current` points at (R-nested layout,
// A1-S1). commit_sha is the audit tag for the artifact's origin, written to
// plugin_sigils on allow (ADR-026(g), outside the signature).
//
// Reads ONLY the symlink target (os.Readlink, without following it): the target is the
// relative `<commit_sha>` (see [plugingit.updateCurrentSymlink]), so its base name is
// what comes back. Reading the target rather than stat-ing the slot keeps the helper
// cheap and independent of whether the artifact is readable — [ReadSlot] already
// settled that at the allow step.
//
// fail-closed:
//   - missing `<alias>/` directory, or a missing/broken `current` symlink → [ErrSlotNotFound];
//   - `current` exists but is not a symlink → [ErrSlotNotFound] (the R-nested invariant
//     is broken: current must be a symlink to a commit_sha directory).
//
// The target's base name comes back as-is (no 40-hex check): the git resolver
// guarantees its validity when populating the cache, and this only reads what is
// already fixed.
func SlotCommitSHA(cacheRoot, alias string) (string, error) {
	link := filepath.Join(cacheRoot, alias, CurrentLink)
	target, err := os.Readlink(link)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w: %s", ErrSlotNotFound, link)
		}
		// EINVAL (current is not a symlink) and anything else — a corrupted slot:
		// commit_sha cannot be extracted reliably, so fail closed.
		return "", fmt.Errorf("%w: read current symlink %q: %v", ErrSlotNotFound, link, err)
	}
	commitSHA := filepath.Base(target)
	if commitSHA == "" || commitSHA == "." || commitSHA == string(filepath.Separator) {
		return "", fmt.Errorf("%w: empty commit_sha in current symlink %q", ErrSlotNotFound, link)
	}
	return commitSHA, nil
}

// SingleArtifactIn returns the one executable in dir.
//
// There is no name to look up — the artifact declares none since NIM-377 — so the rule
// is arithmetic: EXACTLY one executable. None is an empty directory; more than one is
// ambiguous, and taking the first would let directory listing order decide which code
// gets signed and later executed. Both are errors, never a guess.
//
// Dot-files are ignored: that is the digest sidecar and the temp files an atomic write
// leaves behind. Non-executables are ignored too, which is what lets `dist/` carry the
// published `schema.json` next to the artifact without becoming ambiguous.
//
// os.Stat rather than the DirEntry: a slot may reach its artifact through a symlink,
// and lstat would report the link instead of what it points at.
func SingleArtifactIn(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var found []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		st, err := os.Stat(filepath.Join(dir, name))
		if err != nil || st.IsDir() || st.Mode().Perm()&0o111 == 0 {
			continue
		}
		found = append(found, name)
	}
	switch len(found) {
	case 1:
		return filepath.Join(dir, found[0]), nil
	case 0:
		return "", errors.New("no executable artifact in the directory")
	default:
		sort.Strings(found)
		return "", fmt.Errorf("directory holds %d executables (%s), exactly one artifact is expected",
			len(found), strings.Join(found, ", "))
	}
}

// fileDigest computes a file's SHA-256 streaming (plugin artifacts are tens of MB).
// Duplicate of computeFileDigest in shared/pluginhost (unexported there); the local
// copy avoids widening shared's public surface just to read a slot.
func fileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("pluginhost: open artifact for digest %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("pluginhost: read artifact for digest %q: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
