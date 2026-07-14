package pluginhost

import (
	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"
)

// Canonicalization and assembly of the signed Sigil block (ADR-026, slice S3).
//
// This file is a shared helper imported by BOTH sides of the trust seal:
//   - keeper/internal/sigil (S3) — when signing a plugin binary;
//   - soul/internal/pluginhost (S6, future) — when verifying before seal/exec.
//
// Placed in shared/pluginhost because that package is already imported by both
// keeper and soul (see who-imports) and depends only on shared/plugin + grpc — no
// import cycle.
//
// S3↔S6 invariant (normative): the manifest.yaml bytes that Keeper hashes at Sign
// MUST equal the bytes that Soul re-hashes at verify. The guarantee rests on two
// things: (1) manifest and binary travel as one artifact stream (the same file
// reaches the host), (2) both sides run the raw bytes through
// [NormalizeManifestBytes] before hashing — this removes BOM / CRLF /
// trailing-newline differences between writes on different OSes. No YAML
// re-parse/re-emit: canonicalization is BYTE-level ONLY so the hash does not depend
// on the yaml emitter's version/settings.

// sigilDomainSeparator — domain-separation tag of the signed Sigil block.
//
// The `/v1` version is mandatory: on a block-format change (e.g. adding a field) the
// tag becomes `soul-stack/sigil/v2` and old signatures stop verifying against the
// new code — an explicit, not silent, compatibility break. The tag is placed first
// in the block so a Sigil signature cannot be reused in another protocol
// (cross-protocol signature reuse).
const sigilDomainSeparator = "soul-stack/sigil/v1"

// NormalizeManifestBytes brings raw manifest.yaml bytes to canonical form before
// hashing. The ONLY manifest canonicalization is byte-level:
//
//   - strip UTF-8 BOM (Windows editors sometimes add it);
//   - CRLF → LF (Windows line endings);
//   - exactly one trailing LF (several collapse to one; none is added if missing).
//
// NO YAML re-parse or re-emit: the hash must not depend on the yaml emitter's
// version/settings. Both sides (Sign on Keeper, verify on Soul) must call this exact
// function — otherwise the S3↔S6 byte invariant does not hold.
func NormalizeManifestBytes(raw []byte) []byte {
	b := sharedplugin.StripBOM(raw)

	// CRLF → LF. One pass with copy-on-shrink: the result is no longer than the
	// input, so allocate a buffer for the source size and truncate.
	out := make([]byte, 0, len(b)+1)
	for i := 0; i < len(b); i++ {
		if b[i] == '\r' {
			// \r\n → \n: skip \r, the \n is appended on the next iteration.
			// A lone \r (old Mac CR) → \n.
			if i+1 < len(b) && b[i+1] == '\n' {
				continue
			}
			out = append(out, '\n')
			continue
		}
		out = append(out, b[i])
	}

	// Normalize the trailing newline: collapse a run of \n into exactly one \n. An
	// empty input → one \n (a canonical non-empty result, so an empty and a
	// "whitespace-only" manifest do not hash the same as a real one).
	end := len(out)
	for end > 0 && out[end-1] == '\n' {
		end--
	}
	out = out[:end]
	out = append(out, '\n')
	return out
}

// BuildSigilBlock assembles the deterministic signed Sigil block from the fields of
// an allow-list entry (ADR-026(b)/(c)). Pure function: one input → one output, no
// proto-marshal (a SigilSignedBlock message is deliberately NOT introduced — it
// would reintroduce proto-serialization nondeterminism, R-det).
//
// Block form (field order is fixed, cannot change without bumping the DST to v2):
//
//	DST || LP(namespace) || LP(name) || LP(ref) || LP(binarySHA256Raw) || LP(manifestSHA256Raw)
//
// where:
//   - DST = the ASCII constant [sigilDomainSeparator] ("soul-stack/sigil/v1"),
//     added WITHOUT a length-prefix (a fixed known prefix);
//   - LP(x) = 4 bytes big-endian uint32 length of x, then the bytes of x. LP is
//     applied to EVERY variable field — this protects field boundaries: without a
//     length-prefix the concatenation ("ab","c") and ("a","bc") would yield the same
//     block, and a signature over one field set would fit another;
//   - hashes are stored as RAW bytes (32 bytes for SHA-256), NOT a hex string.
//
// Field order is exactly: namespace, name, ref, binary_sha256, manifest_sha256.
func BuildSigilBlock(namespace, name, ref string, binarySHA256Raw, manifestSHA256Raw []byte) []byte {
	const lpSize = 4
	dst := []byte(sigilDomainSeparator)

	total := len(dst) +
		lpSize + len(namespace) +
		lpSize + len(name) +
		lpSize + len(ref) +
		lpSize + len(binarySHA256Raw) +
		lpSize + len(manifestSHA256Raw)

	block := make([]byte, 0, total)
	block = append(block, dst...)
	block = appendLP(block, []byte(namespace))
	block = appendLP(block, []byte(name))
	block = appendLP(block, []byte(ref))
	block = appendLP(block, binarySHA256Raw)
	block = appendLP(block, manifestSHA256Raw)
	return block
}

// appendLP appends a length-prefixed field: uint32 big-endian length + bytes.
func appendLP(dst, field []byte) []byte {
	n := uint32(len(field))
	dst = append(dst, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	return append(dst, field...)
}
