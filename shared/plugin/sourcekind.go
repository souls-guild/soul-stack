package plugin

// Source kinds — HOW an artifact's bytes are reached (NIM-793).
//
// This is a different axis from [Document.Kind] (soul_module / cloud_driver /
// ssh_provider), which says WHAT an artifact is. The two share the word `kind`
// because the catalog entry and the schema document each call their own dimension
// that, and neither spelling is ours to change; in Go they are told apart by name
// (SourceKind* against Kind*).
//
// The source kind is signed into the Sigil block, so it is not a hint a catalog can
// change after an approval: it decides where a Soul goes for the bytes, and an
// unsigned answer to that question would let a rewritten catalog redirect a fetch.
const (
	// SourceKindGit — the artifact is built in a git repository and Keeper resolves
	// `ref` to a commit, taking the single executable from `dist/`. The historical
	// and default kind: an entry that names none is this one, so every keeper.yml
	// written before NIM-793 keeps its meaning.
	SourceKindGit = "git"

	// SourceKindArtifact — the artifact is published, already built, under a base URL
	// and reached over https. The catalog lists one row per platform with its own
	// digest, and one release is one approval.
	SourceKindArtifact = "artifact"
)

// DefaultSourceKind is what an entry with no `kind:` means. Not a fallback for an
// unknown value: an unrecognised kind is refused, only an ABSENT one defaults.
const DefaultSourceKind = SourceKindGit

// ResolveSourceKind maps an entry's `kind:` to the effective source kind: empty →
// [DefaultSourceKind], anything else through unchanged (validation refuses an
// unknown value elsewhere, and silently rewriting it here would hide that).
func ResolveSourceKind(kind string) string {
	if kind == "" {
		return DefaultSourceKind
	}
	return kind
}

// ValidSourceKind reports whether kind is one of the two known source kinds.
// The empty string is NOT valid here — callers that accept an omitted `kind:`
// run it through [ResolveSourceKind] first, so the two decisions stay separate.
func ValidSourceKind(kind string) bool {
	return kind == SourceKindGit || kind == SourceKindArtifact
}

// SourceKinds returns the closed set, for error messages that name what was expected.
func SourceKinds() []string { return []string{SourceKindGit, SourceKindArtifact} }
