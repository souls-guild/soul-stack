package pluginhost

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"
	sharedhost "github.com/souls-guild/soul-stack/shared/pluginhost"
)

// The artifact-kind slot layout (NIM-793), beside the git one it does NOT replace.
//
//	<cacheRoot>/
//	  <alias>/
//	    current -> <release_id>            # symlink to the active slot (atomic)
//	    <release_id>/                      # immutable slot
//	      .release.json                    # what this release is: kind + the file list
//	      <os>-<arch>/
//	        <alias>                        # that platform's artifact
//
// A git slot keeps its shape exactly — `current -> <commit_sha>` over a directory
// holding one executable — and is told apart from an artifact slot by the presence of
// the descriptor, not by a flag stored elsewhere. That is the whole compatibility
// story: a slot written before this change reads as a git slot because it has no
// descriptor, which is what it is.
//
// # Why a descriptor exists at all
//
// The resolver knows three things about each file that live nowhere else once the
// bytes are on disk: which platform it is for, which published path it came from, and
// which digest the operator declared. The layout can carry the platform (it is the
// directory name) but not the path, and `plugin.allow` needs all three to build the
// grant. So the slot states them.
//
// It is not a second copy of anything signed. The digests it records are RE-DERIVED
// from the files at read time and a disagreement is fail-closed ([ReadSlot]), so the
// descriptor cannot drift into asserting something the bytes do not — which is the
// property that keeps it from becoming the schema-beside-the-artifact this package
// refuses to write.

// ReleaseFileName is the slot descriptor of an artifact-kind release. Dot-prefixed so
// [SingleArtifactIn] and discovery skip it the same way they skip the digest sidecar.
const ReleaseFileName = ".release.json"

// releaseFileMode — the descriptor is written read-only: it is fixed when the
// immutable slot is created and never edited afterwards.
const releaseFileMode = 0o444

// ErrReleaseUnreadable indicates a slot that claims to be an artifact release but
// cannot be read as one: a malformed descriptor, a missing platform directory, or an
// artifact whose bytes no longer match the digest the descriptor recorded.
//
// Fail-closed, and deliberately NOT folded into [ErrSlotNotFound]: "there is nothing
// here" and "what is here is broken" are different facts for an operator, and only the
// second one means somebody should look at the cache.
var ErrReleaseUnreadable = errors.New("pluginhost: artifact release slot is unreadable")

// ReleaseArtifact is one platform's file in a release descriptor. The JSON field names
// are the catalog's, so an operator reading the descriptor sees the rows they wrote.
type ReleaseArtifact struct {
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// Dir is the platform's subdirectory name inside the slot. The tokens are validated
// as lower-alphanumeric before a release is ever written ([sharedhost.CanonicalArtifacts]),
// so this is a name and never a path.
func (a ReleaseArtifact) Dir() string { return a.OS + "-" + a.Arch }

// Release is the slot descriptor: what kind of source produced this slot, WHERE it was
// fetched from, and which files it holds.
//
// Source is the publication root the resolver actually reached — not a copy of the
// catalog for its own sake. `plugin.allow` signs an operator-asserted `source`, and
// without this the Keeper would have no way to tell that the address it is about to
// put a signature on is the address it verified the digests at. With it, the two are
// compared and a mismatch is refused ([sigil.Service.Allow]).
type Release struct {
	Kind      string            `json:"kind"`
	Source    string            `json:"source"`
	Artifacts []ReleaseArtifact `json:"artifacts"`
}

// ReleaseID is the immutable name of the slot directory a release materializes into:
// the SHA-256 of its canonical descriptor bytes.
//
// It plays the role commit_sha plays for a git slot — one release, one directory, and
// re-resolving an unchanged release is a no-op rather than a rewrite. Derived rather
// than taken from the ref: a tag can be moved, and a slot named by a movable label
// would stop being immutable the first time somebody moved one.
func ReleaseID(r Release) (string, error) {
	canon, err := MarshalRelease(r)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:]), nil
}

// MarshalRelease renders a descriptor to its canonical bytes: the artifact list in
// [sharedhost.CanonicalArtifacts] order, so two resolves of one release produce the
// same bytes and therefore the same [ReleaseID].
func MarshalRelease(r Release) ([]byte, error) {
	if !sharedplugin.ValidSourceKind(r.Kind) {
		return nil, fmt.Errorf("%w: unknown source kind %q", ErrReleaseUnreadable, r.Kind)
	}
	canon, err := sharedhost.CanonicalArtifacts(SigilArtifactsOf(r.Artifacts))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrReleaseUnreadable, err)
	}
	if r.Source == "" {
		return nil, fmt.Errorf("%w: release descriptor states no source", ErrReleaseUnreadable)
	}
	ordered := Release{Kind: r.Kind, Source: r.Source, Artifacts: make([]ReleaseArtifact, 0, len(canon))}
	for _, a := range canon {
		ordered.Artifacts = append(ordered.Artifacts, ReleaseArtifact{
			OS: a.OS, Arch: a.Arch, Path: a.Path, SHA256: a.SHA256,
		})
	}
	// No indentation and no trailing newline: the bytes are hashed, so every one of
	// them is part of the slot's identity.
	return json.Marshal(ordered)
}

// SigilArtifactsOf projects descriptor rows into the grant's artifact type. One
// direction only: the descriptor is a local record of what was fetched, the grant is
// what gets signed, and they are kept as separate types so a change to one does not
// silently become a change to the other.
func SigilArtifactsOf(artifacts []ReleaseArtifact) []sharedhost.SigilArtifact {
	out := make([]sharedhost.SigilArtifact, 0, len(artifacts))
	for _, a := range artifacts {
		out = append(out, sharedhost.SigilArtifact{OS: a.OS, Arch: a.Arch, Path: a.Path, SHA256: a.SHA256})
	}
	return out
}

// ReleaseArtifactsOf is the other direction: grant rows into descriptor rows, for a
// provider that has just canonicalized a catalog's list and is about to write it.
func ReleaseArtifactsOf(artifacts []sharedhost.SigilArtifact) []ReleaseArtifact {
	out := make([]ReleaseArtifact, 0, len(artifacts))
	for _, a := range artifacts {
		out = append(out, ReleaseArtifact{OS: a.OS, Arch: a.Arch, Path: a.Path, SHA256: a.SHA256})
	}
	return out
}

// WriteRelease writes the descriptor into dir. Used by the artifact provider while
// building the staging directory, before the atomic rename into the immutable slot.
func WriteRelease(dir string, r Release) error {
	canon, err := MarshalRelease(r)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, ReleaseFileName)
	if err := os.WriteFile(path, canon, releaseFileMode); err != nil {
		return fmt.Errorf("pluginhost: write release descriptor %q: %w", path, err)
	}
	return nil
}

// ReadRelease reads the descriptor of the slot at dir. A slot with no descriptor is a
// git slot, reported as (nil, nil): absence is a shape, not a failure.
func ReadRelease(dir string) (*Release, error) {
	path := filepath.Join(dir, ReleaseFileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("%w: read %q: %v", ErrReleaseUnreadable, path, err)
	}
	var r Release
	if uerr := json.Unmarshal(raw, &r); uerr != nil {
		return nil, fmt.Errorf("%w: parse %q: %v", ErrReleaseUnreadable, path, uerr)
	}
	if !sharedplugin.ValidSourceKind(r.Kind) {
		return nil, fmt.Errorf("%w: %q declares unknown source kind %q", ErrReleaseUnreadable, path, r.Kind)
	}
	if r.Source == "" {
		return nil, fmt.Errorf("%w: %q states no source", ErrReleaseUnreadable, path)
	}
	if _, cerr := sharedhost.CanonicalArtifacts(SigilArtifactsOf(r.Artifacts)); cerr != nil {
		return nil, fmt.Errorf("%w: %q: %v", ErrReleaseUnreadable, path, cerr)
	}
	return &r, nil
}
