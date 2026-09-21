package pluginhost

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
)

// Assembly of the signed Sigil block (ADR-026, slice S3).
//
// This file is a shared helper imported by BOTH sides of the trust seal:
//   - keeper/internal/sigil (S3) — when signing an artifact;
//   - soul/internal/pluginhost (S6) — when verifying before seal/exec.
//
// Placed in shared/pluginhost because that package is already imported by both keeper
// and soul (see who-imports) and depends only on shared/plugin + grpc — no import
// cycle.
//
// # What the block identifies
//
// The artifact carries no self-name (NIM-377): no namespace, no name, no publisher.
// The only identity it can be signed under is the one the operator asserted when
// approving it — the SOURCE it came from, at a REF. The registration alias is
// deliberately NOT in the block: an alias is a local naming choice, so registering the
// same bytes a second time under a second alias must not require a second signature,
// and forging an alias must not be able to reach a signature at all.
//
// # One release, one approval (NIM-793)
//
// A source publishes one release under one ref, and that release is several BINARIES:
// linux/amd64 and linux/arm64 are different bytes with different digests. So the block
// covers a LIST of artifacts rather than one digest. Approving a release means
// approving every platform variant of it at once — the alternative, one grant per
// platform, is N Archon confirmations per release and buys no extra guarantee, since
// they would all be over the same ref anyway.
//
// # S3↔S6 invariant (normative)
//
// The schema-document bytes Keeper hashes at Sign MUST equal the bytes Soul re-hashes
// at verify. The guarantee is now structural rather than negotiated: the document is
// canonical JSON produced by one serializer (`sdk/schema`.Marshal — sorted keys, no
// insignificant whitespace), and it travels with the artifact. There is nothing left
// to normalize, so there is no normalization step to disagree about — both sides call
// [SchemaDigest] on the bytes they hold and get the same 32 bytes or a mismatch that
// means the bytes really differ.

// sigilDomainSeparator — domain-separation tag of the signed Sigil block.
//
// The version is mandatory: on a block-format change the tag moves, and old
// signatures stop verifying against the new code — an explicit, not silent,
// compatibility break. v1 keyed on (namespace, name); NIM-377 removed both from the
// artifact and re-keyed the block on the source. v3 (NIM-793) replaces the single
// binary digest with the artifact LIST and adds the source kind, so no v2 signature
// verifies here either. The tag is placed first in the block so a Sigil signature
// cannot be reused in another protocol (cross-protocol signature reuse).
const sigilDomainSeparator = "soul-stack/sigil/v3"

// AnyPlatform is the OS/Arch spelling of an artifact that declares no platform.
//
// It exists for one producer: the git source kind takes the single executable out of
// `dist/` and the repository says nothing about what it was built for. That artifact
// answers for every platform — which is exactly what it did before a grant carried a
// list at all — so the git kind keeps working without a catalog change.
//
// An unplatformed artifact may only appear ALONE ([CanonicalArtifacts] refuses a list
// that mixes it with platform rows): a row that matches everything sitting beside rows
// that match one thing would make selection depend on which check ran first.
const AnyPlatform = ""

// reArtifactSHA256 is the digest form every artifact row carries: 64 lowercase hex.
// Same shape as the plugin_sigils CHECK constraint and as keeper/internal/sigil's
// own guard — a digest that is not this is not a digest.
var reArtifactSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// rePlatformToken is the shape of an OS or Arch value. Every GOOS and GOARCH Go
// defines is lowercase alphanumeric, so this rejects nothing real — and it is what
// keeps a platform token usable as a directory name in the slot cache without a
// sanitizer between the two: a `/` or a `..` never reaches a path because it never
// reaches a signature.
var rePlatformToken = regexp.MustCompile(`^[a-z0-9]{1,32}$`)

// ErrSigilArtifacts is the sentinel behind every rejection of an artifact list. The
// list is refused BEFORE a signature exists over it, so nothing downstream has to cope
// with an ambiguous grant: an unsignable list is one that never becomes a grant.
var ErrSigilArtifacts = errors.New("pluginhost: invalid sigil artifact list")

// SigilArtifact is one approved artifact inside a grant: the platform it was built
// for, where it is fetched from relative to the grant's source, and its SHA-256.
//
// OS and Arch are Go's GOOS/GOARCH spellings (`linux`, `amd64`), because that is what
// both hosts can state about themselves without a mapping table in between. Both empty
// = [AnyPlatform]. One set and the other not is refused: a platform is a pair, and a
// half-stated one would silently match nothing.
//
// Path is relative to the grant's source and is SIGNED, which is what lets a Soul fetch
// without reading the operator's catalog: the catalog never reaches a Soul, the grant
// does. A rewritten catalog can therefore only produce a digest mismatch, never a
// redirect.
//
// SHA256 is the approved digest, 64 lowercase hex. This is the one real control on the
// spawn path — the operator approved these bytes, and no others get to exec.
type SigilArtifact struct {
	OS     string
	Arch   string
	Path   string
	SHA256 string
}

// Platformed reports whether the row names a concrete platform.
func (a SigilArtifact) Platformed() bool { return a.OS != AnyPlatform || a.Arch != AnyPlatform }

// SchemaDigest is the SHA-256 of a canonical schema document, and the ONE function
// both sides of the seal call.
//
// It takes the bytes as they are. The document is generated by a single serializer and
// is byte-deterministic by construction, so there is no BOM, no line-ending and no
// trailing-whitespace variance to absorb — and a "canonicalize before hashing" step
// would only create a place where the two sides could implement the same idea
// differently. Two documents that differ by one byte are two different documents.
func SchemaDigest(schemaDoc []byte) [32]byte { return sha256.Sum256(schemaDoc) }

// CanonicalArtifacts validates an artifact list and returns it in the ONE order a
// signature is ever computed over: sorted by (os, arch, path).
//
// Sorting here rather than trusting the caller is what makes sign and verify agree
// without a wire-order contract between them: Keeper may store the rows in catalog
// order and a Soul may receive them in proto order, and both still hash the same bytes.
//
// Refused, each because the alternative is an ambiguous approval:
//   - an empty list — a grant that approves no bytes cannot let anything run, and
//     signing one would produce a valid seal over nothing;
//   - a duplicate (os, arch) — two rows for one platform mean the selection picks by
//     list order, so the approved bytes would depend on how the list was written;
//   - a half-stated platform (os without arch or the reverse);
//   - an unplatformed row beside a platform row (see [AnyPlatform]);
//   - a digest that is not 64 lowercase hex.
//
// The input slice is not modified.
func CanonicalArtifacts(artifacts []SigilArtifact) ([]SigilArtifact, error) {
	if len(artifacts) == 0 {
		return nil, fmt.Errorf("%w: empty (a grant must approve at least one artifact)", ErrSigilArtifacts)
	}

	out := make([]SigilArtifact, len(artifacts))
	copy(out, artifacts)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].OS != out[j].OS {
			return out[i].OS < out[j].OS
		}
		if out[i].Arch != out[j].Arch {
			return out[i].Arch < out[j].Arch
		}
		return out[i].Path < out[j].Path
	})

	anyPlatform := false
	for i, a := range out {
		if (a.OS == AnyPlatform) != (a.Arch == AnyPlatform) {
			return nil, fmt.Errorf("%w: artifact %d states os=%q arch=%q — a platform is a pair",
				ErrSigilArtifacts, i, a.OS, a.Arch)
		}
		if !a.Platformed() {
			anyPlatform = true
		} else if !rePlatformToken.MatchString(a.OS) || !rePlatformToken.MatchString(a.Arch) {
			return nil, fmt.Errorf("%w: artifact %d platform (os=%q arch=%q) must be lower-alphanumeric GOOS/GOARCH tokens",
				ErrSigilArtifacts, i, a.OS, a.Arch)
		}
		if !reArtifactSHA256.MatchString(a.SHA256) {
			return nil, fmt.Errorf("%w: artifact %d (os=%q arch=%q) sha256 %q must be 64 lower-hex chars",
				ErrSigilArtifacts, i, a.OS, a.Arch, a.SHA256)
		}
		if i > 0 && out[i-1].OS == a.OS && out[i-1].Arch == a.Arch {
			return nil, fmt.Errorf("%w: duplicate platform (os=%q arch=%q)", ErrSigilArtifacts, a.OS, a.Arch)
		}
	}
	if anyPlatform && len(out) > 1 {
		return nil, fmt.Errorf("%w: an unplatformed artifact cannot share a grant with platform-specific ones",
			ErrSigilArtifacts)
	}
	return out, nil
}

// SelectArtifact picks the artifact a host on (goos, goarch) is approved to run:
// the exact platform match, or the single unplatformed row ([AnyPlatform]) when the
// grant has one. No match → nil, and every caller treats that as fail-closed.
//
// "No row for this platform" is a real and expected answer, not an error condition to
// route around: a release published for linux/amd64 only is simply not approved on
// arm64, and there is nothing safe to substitute.
//
// artifacts need not be canonical — selection is by content, not position.
func SelectArtifact(artifacts []SigilArtifact, goos, goarch string) *SigilArtifact {
	var fallback *SigilArtifact
	for i := range artifacts {
		a := &artifacts[i]
		if a.OS == goos && a.Arch == goarch {
			return a
		}
		if !a.Platformed() {
			fallback = a
		}
	}
	return fallback
}

// BuildSigilBlock assembles the deterministic signed Sigil block from the fields of an
// allow-list entry (ADR-026(b)/(c), NIM-793). Pure function of its inputs: one input →
// one output, no proto-marshal (a SigilSignedBlock message is deliberately NOT
// introduced — it would reintroduce proto-serialization nondeterminism, R-det).
//
// Block form (field order is fixed, cannot change without bumping the DST to v4):
//
//	DST || LP(source) || LP(kind) || LP(ref) || LP(schemaSHA256Raw) || U32(n)
//	    || for each artifact, in [CanonicalArtifacts] order:
//	           LP(os) || LP(arch) || LP(path) || LP(sha256Raw)
//
// where:
//   - DST = the ASCII constant [sigilDomainSeparator] ("soul-stack/sigil/v3"),
//     added WITHOUT a length-prefix (a fixed known prefix);
//   - source = the artifact source the operator approved (the git remote for the git
//     kind, the base URL for the artifact kind), and ref the revision within it.
//     Together they are the artifact's only signed identity;
//   - kind = how the bytes are reached ([plugin.SourceKindGit] / …Artifact). Signed
//     because it decides WHERE a Soul goes for the bytes, and an unsigned answer would
//     let a rewritten catalog redirect the fetch;
//   - LP(x) = 4 bytes big-endian uint32 length of x, then the bytes of x. LP is
//     applied to EVERY variable field — this protects field boundaries: without a
//     length-prefix the concatenation ("ab","c") and ("a","bc") would yield the same
//     block, and a signature over one field set would fit another;
//   - U32(n) = the artifact count, 4 bytes big-endian and NOT length-prefixed (a
//     fixed-width field needs no boundary marker). It is written even though the
//     per-field LPs already fix every boundary: the count says how many rows the
//     signer meant, so a truncated list is a different block rather than a shorter
//     read of the same one;
//   - hashes are stored as RAW bytes (32 bytes for SHA-256), NOT a hex string.
//     schemaSHA256Raw comes from [SchemaDigest].
//
// The list is canonicalized here rather than by the caller, so sign and verify cannot
// disagree about order; an unsignable list ([CanonicalArtifacts]) comes back as an
// error instead of a block.
func BuildSigilBlock(source, kind, ref string, schemaSHA256Raw []byte, artifacts []SigilArtifact) ([]byte, error) {
	canon, err := CanonicalArtifacts(artifacts)
	if err != nil {
		return nil, err
	}

	block := make([]byte, 0, 256)
	block = append(block, sigilDomainSeparator...)
	block = appendLP(block, []byte(source))
	block = appendLP(block, []byte(kind))
	block = appendLP(block, []byte(ref))
	block = appendLP(block, schemaSHA256Raw)
	block = appendU32(block, uint32(len(canon)))
	for _, a := range canon {
		raw, err := hex.DecodeString(a.SHA256)
		if err != nil {
			// CanonicalArtifacts already matched the hex form — defensive.
			return nil, fmt.Errorf("%w: decode sha256 of (os=%q arch=%q): %v", ErrSigilArtifacts, a.OS, a.Arch, err)
		}
		block = appendLP(block, []byte(a.OS))
		block = appendLP(block, []byte(a.Arch))
		block = appendLP(block, []byte(a.Path))
		block = appendLP(block, raw)
	}
	return block, nil
}

// appendLP appends a length-prefixed field: uint32 big-endian length + bytes.
func appendLP(dst, field []byte) []byte {
	return append(appendU32(dst, uint32(len(field))), field...)
}

// appendU32 appends a fixed-width big-endian uint32.
func appendU32(dst []byte, n uint32) []byte {
	return append(dst, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
}
