package pluginhost

import (
	"bytes"
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
	sharedhost "github.com/souls-guild/soul-stack/shared/pluginhost"
)

// ErrSlotNotFound indicates the cache has no slot `<cacheRoot>/<alias>/`, or the slot
// holds no readable artifact. Returned by [ReadSlot] when the registration alias
// resolves to nothing usable; sigil.Service maps it to ErrPluginNotInCache → 404.
var ErrSlotNotFound = errors.New("pluginhost: plugin slot not found in cache")

// CurrentLink is the name of the symlink to the active commit_sha-slot in the R-nested
// layout (ADR-026 F-fetch, A1-S1): `<cacheRoot>/<alias>/current → <commit_sha>`.
// The resolver ([plugingit.Resolver]) updates it atomically when it populates the cache.
const CurrentLink = "current"

// SlotArtifact is one artifact inside a slot: which platform it serves, where it was
// published, its digest, and where it now sits on this host.
//
// OS/Arch/Path/SHA256 are the fields that will be signed into the grant; BinaryPath is
// local and is not. A git slot yields exactly one of these with an empty platform and
// an empty path ([sharedhost.AnyPlatform]) — that source declares neither, and the
// grant says so rather than inventing values.
type SlotArtifact struct {
	OS         string
	Arch       string
	Path       string
	SHA256     string
	BinaryPath string
}

// Sigil projects the row into the grant's artifact type, dropping the local path.
func (a SlotArtifact) Sigil() sharedhost.SigilArtifact {
	return sharedhost.SigilArtifact{OS: a.OS, Arch: a.Arch, Path: a.Path, SHA256: a.SHA256}
}

// SlotContents is what one slot holds, read by REGISTRATION ALIAS: the release's
// artifacts and the canonical schema document stamped into them.
//
// This is the input to the Sigil signature (ADR-026): Keeper reads the ACTIVE release
// of `<cacheRoot>/<alias>/current/` (R-nested layout, A1-S1: `current` is a symlink to
// the immutable slot a provider populated).
//
// The alias is the ONLY lookup key. The artifact carries no self-name since NIM-377 —
// no namespace, no name, no filename convention — so there is nothing else a slot
// could be found by, and the alias is what the grant, the slot and every address
// derived from it agree on. `ref` is an operator-asserted label on the grant and takes
// no part in the lookup.
type SlotContents struct {
	// Kind is the source kind that produced this slot ([sharedplugin.SourceKindGit] /
	// …Artifact). It rides into the grant, where it tells a Soul how to reach the
	// bytes.
	Kind string
	// Source is the address the resolver actually fetched this release from, when the
	// slot records one. `plugin.allow` checks the operator's asserted `source` against
	// it before signing, so a grant cannot claim an origin the Keeper never reached.
	//
	// EMPTY for a git slot, and that is a statement rather than a gap: a git slot is
	// the pre-NIM-793 layout, carries no descriptor, and there is nowhere in it that
	// records the remote. The git kind therefore keeps the operator-asserted `source`
	// it always had — unchanged behaviour, as it must be — and only the artifact kind
	// gains the check.
	Source string
	// Artifacts are the release's files, in canonical order. Never empty when
	// ReadSlot returns no error.
	Artifacts []SlotArtifact
	// SchemaBytes are the canonical schema-document bytes read from the artifact's
	// TRAILER — byte-exact, as [sharedhost.SchemaDigest] will hash them. Reading them
	// from a sibling `schema.json` instead would let the signed disclosure differ from
	// the one inside the bytes being approved.
	//
	// One document for the whole release. A release whose platform builds disclose
	// DIFFERENT documents is refused rather than resolved to one of them: the
	// disclosure is what the Archon approves, and there is no honest way to approve
	// two of them with one signature.
	SchemaBytes []byte
	// Doc is SchemaBytes parsed and validated, for callers that need the kind or the
	// module set without re-parsing. Never nil when ReadSlot returns no error.
	Doc *sharedplugin.Document
}

// SigilArtifacts projects the slot's rows into the grant's artifact list.
func (s *SlotContents) SigilArtifacts() []sharedhost.SigilArtifact {
	out := make([]sharedhost.SigilArtifact, 0, len(s.Artifacts))
	for _, a := range s.Artifacts {
		out = append(out, a.Sigil())
	}
	return out
}

// ReadSlot reads the ACTIVE release of the slot registered under alias, through the
// current-symlink `<cacheRoot>/<alias>/current/` (R-nested layout, A1-S1).
//
// Steps:
//  1. active slot `<cacheRoot>/<alias>/current/` (a symlink to an immutable slot
//     directory); missing / broken symlink → [ErrSlotNotFound];
//  2. the release descriptor ([ReadRelease]). Absent → this is a git slot and step 3
//     reads it the way it always did; present → an artifact release and step 4 reads
//     its platform directories;
//  3. git: the slot's SINGLE executable — there is no filename to look up, so "exactly
//     one" is the rule and zero or several is an error, never a first-match guess;
//  4. artifact: one file per declared platform, each re-digested and checked against
//     the digest the descriptor recorded (a disagreement → [ErrReleaseUnreadable]);
//  5. the canonical schema document from the artifact trailers — identical across the
//     release or fail-closed;
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

	return ReadSlotDir(dir)
}

// ReadSlotDir reads one slot directory directly, without going through the alias and
// the `current` symlink — steps 2 to 5 of [ReadSlot].
//
// Exported for the resolver, which has just materialized a specific immutable slot and
// wants to read back exactly THAT one. Going through `current` would work today and
// answer about a different slot the moment two resolves interleave; and reading back
// through the same code the allow path uses is what keeps "what was cached" and "what
// can be approved" from being two different notions of a valid slot.
func ReadSlotDir(dir string) (*SlotContents, error) {
	release, err := ReadRelease(dir)
	if err != nil {
		return nil, err
	}
	if release == nil {
		return readGitSlot(dir)
	}
	return readReleaseSlot(dir, release)
}

// readGitSlot reads the historical single-artifact slot. Unchanged behaviour: one
// executable, its trailer, its digest — expressed as a one-row unplatformed release so
// everything downstream sees one shape.
func readGitSlot(dir string) (*SlotContents, error) {
	binPath, err := SingleArtifactIn(dir)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrSlotNotFound, dir, err)
	}

	schemaBytes, doc, err := readSlotSchema(binPath)
	if err != nil {
		return nil, err
	}
	digest, err := fileDigest(binPath)
	if err != nil {
		return nil, err
	}

	return &SlotContents{
		Kind: sharedplugin.SourceKindGit,
		Artifacts: []SlotArtifact{{
			OS:         sharedhost.AnyPlatform,
			Arch:       sharedhost.AnyPlatform,
			SHA256:     digest,
			BinaryPath: binPath,
		}},
		SchemaBytes: schemaBytes,
		Doc:         doc,
	}, nil
}

// readReleaseSlot reads a multi-platform slot against its descriptor.
//
// Every file is re-digested rather than trusted from the descriptor. That is what
// keeps the descriptor from being a claim: it records what the resolver fetched, and
// if the bytes on disk have since changed, the slot is refused instead of approving a
// digest nobody re-checked.
//
// The schema is read from EVERY artifact and all of them must be byte-identical. A
// release whose platform builds disclose different things has no single disclosure to
// approve, and picking one — the first, or this host's — would sign a document the
// other platforms do not carry.
func readReleaseSlot(dir string, release *Release) (*SlotContents, error) {
	var (
		artifacts   []SlotArtifact
		schemaBytes []byte
		doc         *sharedplugin.Document
		schemaFrom  string
	)
	for _, want := range release.Artifacts {
		platformDir := filepath.Join(dir, want.Dir())
		binPath, aerr := SingleArtifactIn(platformDir)
		if aerr != nil {
			return nil, fmt.Errorf("%w: %s: %v", ErrReleaseUnreadable, platformDir, aerr)
		}
		digest, derr := fileDigest(binPath)
		if derr != nil {
			return nil, derr
		}
		if digest != want.SHA256 {
			return nil, fmt.Errorf("%w: %s is %s, the release records %s",
				ErrReleaseUnreadable, binPath, digest, want.SHA256)
		}

		gotSchema, gotDoc, serr := readSlotSchema(binPath)
		if serr != nil {
			return nil, serr
		}
		if schemaBytes == nil {
			schemaBytes, doc, schemaFrom = gotSchema, gotDoc, binPath
		} else if !bytes.Equal(schemaBytes, gotSchema) {
			return nil, fmt.Errorf("%w: %s discloses a different schema document than %s — one release cannot have two disclosures",
				ErrReleaseUnreadable, binPath, schemaFrom)
		}

		artifacts = append(artifacts, SlotArtifact{
			OS: want.OS, Arch: want.Arch, Path: want.Path, SHA256: digest, BinaryPath: binPath,
		})
	}
	if len(artifacts) == 0 {
		// ReadRelease already refuses an empty list — defensive.
		return nil, fmt.Errorf("%w: %s holds no artifacts", ErrReleaseUnreadable, dir)
	}
	return &SlotContents{
		Kind:        release.Kind,
		Source:      release.Source,
		Artifacts:   artifacts,
		SchemaBytes: schemaBytes,
		Doc:         doc,
	}, nil
}

// readSlotSchema reads and validates the canonical schema document out of an
// artifact's trailer, WITHOUT executing it: at this point the binary is precisely what
// is not yet approved.
func readSlotSchema(binPath string) ([]byte, *sharedplugin.Document, error) {
	schemaBytes, err := schema.ReadTrailerFile(binPath)
	if err != nil {
		return nil, nil, fmt.Errorf("pluginhost: read schema of %q: %w", binPath, err)
	}
	doc, diags := sharedplugin.ParseDocument(binPath, schemaBytes)
	if derr := sharedplugin.FirstError(diags); derr != nil {
		return nil, nil, fmt.Errorf("pluginhost: invalid schema document in %q: %w", binPath, derr)
	}
	if doc == nil {
		return nil, nil, fmt.Errorf("pluginhost: artifact %q carries no readable schema document", binPath)
	}
	return schemaBytes, doc, nil
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
