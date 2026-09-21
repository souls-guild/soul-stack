package pluginhost

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"
)

// artifactOf builds a one-row list with a digest derived from seed, for the tests that
// only need the block to have SOME valid artifact in it.
func artifactOf(goos, goarch, path string, seed byte) SigilArtifact {
	sum := sha256.Sum256([]byte{seed})
	return SigilArtifact{OS: goos, Arch: goarch, Path: path, SHA256: hex.EncodeToString(sum[:])}
}

// mustBlock fails the test if the list could not be signed over — for the cases where
// an unsignable list is not what is under test.
func mustBlock(t *testing.T, source, kind, ref string, schema []byte, artifacts []SigilArtifact) []byte {
	t.Helper()
	block, err := BuildSigilBlock(source, kind, ref, schema, artifacts)
	if err != nil {
		t.Fatalf("BuildSigilBlock: %v", err)
	}
	return block
}

func TestSchemaDigestIsPlainSHA256(t *testing.T) {
	doc := []byte(`{"kind":"soul_module","protocol_version":1}`)
	want := sha256.Sum256(doc)
	if got := SchemaDigest(doc); got != want {
		t.Fatalf("SchemaDigest = %x, want %x", got, want)
	}
}

// The document is byte-deterministic by construction, so nothing is normalized away:
// two documents that differ by one byte hash differently, and a host cannot be talked
// into treating a modified document as the approved one.
func TestSchemaDigestDoesNotNormalize(t *testing.T) {
	base := []byte(`{"kind":"soul_module","protocol_version":1}`)
	for _, variant := range [][]byte{
		append(append([]byte{}, base...), '\n'),
		append([]byte{0xEF, 0xBB, 0xBF}, base...),
		bytes.ReplaceAll(base, []byte(`,`), []byte(`, `)),
	} {
		if SchemaDigest(variant) == SchemaDigest(base) {
			t.Errorf("variant %q hashes the same as the canonical document", variant)
		}
	}
}

func TestBuildSigilBlock_Deterministic(t *testing.T) {
	doc := bytes.Repeat([]byte{0x02}, 32)
	arts := []SigilArtifact{
		artifactOf("linux", "amd64", "redis_linux_amd64", 1),
		artifactOf("linux", "arm64", "redis_linux_arm64", 2),
	}
	a := mustBlock(t, "https://nexus.internal/plugins/redis", sharedplugin.SourceKindArtifact, "v1.0.0", doc, arts)
	b := mustBlock(t, "https://nexus.internal/plugins/redis", sharedplugin.SourceKindArtifact, "v1.0.0", doc, arts)
	if !bytes.Equal(a, b) {
		t.Fatalf("BuildSigilBlock not deterministic:\n a=%x\n b=%x", a, b)
	}
}

// The list is canonicalized inside the builder, so the ORDER the caller holds the rows
// in cannot change the block. This is what lets Keeper store them in catalog order and
// a Soul receive them in proto order and still hash the same bytes.
func TestBuildSigilBlock_OrderIndependent(t *testing.T) {
	doc := bytes.Repeat([]byte{0x02}, 32)
	amd := artifactOf("linux", "amd64", "redis_linux_amd64", 1)
	arm := artifactOf("linux", "arm64", "redis_linux_arm64", 2)

	a := mustBlock(t, "src", sharedplugin.SourceKindArtifact, "v1", doc, []SigilArtifact{amd, arm})
	b := mustBlock(t, "src", sharedplugin.SourceKindArtifact, "v1", doc, []SigilArtifact{arm, amd})
	if !bytes.Equal(a, b) {
		t.Fatal("block depends on the order the caller held the artifacts in")
	}
}

// The DST is v3: v1 keyed on (namespace, name), v2 on a single binary digest, and
// neither shape exists any more. An old signature must not verify against the new
// block, and the tag is what makes that break explicit rather than silent.
func TestBuildSigilBlock_HasVersionedDST(t *testing.T) {
	block := mustBlock(t, "source", sharedplugin.SourceKindGit, "ref", []byte("doc"),
		[]SigilArtifact{artifactOf(AnyPlatform, AnyPlatform, "", 7)})
	dst := []byte("soul-stack/sigil/v3")
	if !bytes.HasPrefix(block, dst) {
		t.Fatalf("block does not start with DST %q; block=%x", dst, block)
	}
}

// LP boundary: ("ab","c") and ("a","bc") yield DIFFERENT blocks. Without a
// length-prefix they would be identical — the core field-boundary invariant.
func TestBuildSigilBlock_LengthPrefixBoundary(t *testing.T) {
	h := bytes.Repeat([]byte{0x00}, 32)
	one := []SigilArtifact{artifactOf(AnyPlatform, AnyPlatform, "", 1)}

	x := mustBlock(t, "ab", sharedplugin.SourceKindGit, "c", h, one)
	y := mustBlock(t, "a", sharedplugin.SourceKindGit, "bc", h, one)
	if bytes.Equal(x, y) {
		t.Fatal("LP boundary broken: (\"ab\",\"c\") == (\"a\",\"bc\")")
	}

	// Same across the artifact's own adjacent fields: move a byte over the os/arch
	// boundary and the block must change.
	p := mustBlock(t, "src", sharedplugin.SourceKindArtifact, "r", h,
		[]SigilArtifact{artifactOf("linux", "amd64", "ab", 1)})
	q := mustBlock(t, "src", sharedplugin.SourceKindArtifact, "r", h,
		[]SigilArtifact{artifactOf("linux", "amd64", "a", 1)})
	if bytes.Equal(p, q) {
		t.Fatal("LP boundary broken across the artifact path field")
	}
}

// Exact block layout: DST || LP(source) || LP(kind) || LP(ref) || LP(schema) || U32(n)
// || per artifact LP(os) LP(arch) LP(path) LP(sha256), with field order fixed and raw
// (not hex) hashes.
func TestBuildSigilBlock_ExactLayoutAndFieldOrder(t *testing.T) {
	source, ref := "https://nexus.internal/plugins/redis", "v1"
	doc := []byte{0xCC, 0xDD, 0xEE}
	amd := artifactOf("linux", "amd64", "redis_linux_amd64", 1)
	arm := artifactOf("linux", "arm64", "redis_linux_arm64", 2)

	got := mustBlock(t, source, sharedplugin.SourceKindArtifact, ref, doc, []SigilArtifact{arm, amd})

	lp := func(w *bytes.Buffer, f []byte) {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(f)))
		w.Write(n[:])
		w.Write(f)
	}
	var want bytes.Buffer
	want.WriteString("soul-stack/sigil/v3")
	lp(&want, []byte(source))
	lp(&want, []byte(sharedplugin.SourceKindArtifact))
	lp(&want, []byte(ref))
	lp(&want, doc)
	var count [4]byte
	binary.BigEndian.PutUint32(count[:], 2)
	want.Write(count[:])
	// Canonical order is (os, arch, path): amd64 before arm64, whatever order the
	// caller passed.
	for _, a := range []SigilArtifact{amd, arm} {
		raw, err := hex.DecodeString(a.SHA256)
		if err != nil {
			t.Fatalf("fixture digest is not hex: %v", err)
		}
		lp(&want, []byte(a.OS))
		lp(&want, []byte(a.Arch))
		lp(&want, []byte(a.Path))
		lp(&want, raw)
	}
	if !bytes.Equal(got, want.Bytes()) {
		t.Fatalf("block layout mismatch:\n got=%x\nwant=%x", got, want.Bytes())
	}

	// Raw hashes, not hex: the digest sits in the block as 32 bytes, not as 64 ASCII.
	raw, _ := hex.DecodeString(amd.SHA256)
	if !bytes.Contains(got, raw) {
		t.Error("artifact digest bytes not present raw in block")
	}
	if bytes.Contains(got, []byte(amd.SHA256)) {
		t.Error("artifact digest present as hex text in block")
	}
}

// Swapping source and ref yields a different block (field order is fixed).
func TestBuildSigilBlock_FieldOrderMatters(t *testing.T) {
	h := bytes.Repeat([]byte{0x00}, 32)
	one := []SigilArtifact{artifactOf(AnyPlatform, AnyPlatform, "", 1)}
	a := mustBlock(t, "x", sharedplugin.SourceKindGit, "y", h, one)
	b := mustBlock(t, "y", sharedplugin.SourceKindGit, "x", h, one)
	if bytes.Equal(a, b) {
		t.Fatal("swapping source/ref produced identical block")
	}
}

// The source kind is IN the block. It decides where a Soul fetches from, so a catalog
// that flipped it after an approval must break the signature rather than redirect the
// download.
func TestBuildSigilBlock_KindIsSigned(t *testing.T) {
	h := bytes.Repeat([]byte{0x00}, 32)
	arts := []SigilArtifact{artifactOf("linux", "amd64", "p", 1)}
	git := mustBlock(t, "src", sharedplugin.SourceKindGit, "v1", h, arts)
	art := mustBlock(t, "src", sharedplugin.SourceKindArtifact, "v1", h, arts)
	if bytes.Equal(git, art) {
		t.Fatal("the source kind does not reach the signed block")
	}
}

// Tampering with ONE row changes the block. This is the whole reason the signature is
// over the list: a release approves every platform at once, so an attacker swapping the
// arm64 digest must not leave the amd64 approval verifiable.
func TestBuildSigilBlock_CoversEveryArtifact(t *testing.T) {
	h := bytes.Repeat([]byte{0x00}, 32)
	amd := artifactOf("linux", "amd64", "redis_linux_amd64", 1)
	arm := artifactOf("linux", "arm64", "redis_linux_arm64", 2)

	honest := mustBlock(t, "src", sharedplugin.SourceKindArtifact, "v1", h, []SigilArtifact{amd, arm})

	for name, tampered := range map[string][]SigilArtifact{
		"digest swapped on the second row": {amd, artifactOf("linux", "arm64", "redis_linux_arm64", 3)},
		"path swapped on the second row":   {amd, artifactOf("linux", "arm64", "evil", 2)},
		"second row dropped":               {amd},
		"a third row appended": {amd, arm,
			artifactOf("darwin", "arm64", "redis_darwin_arm64", 4)},
	} {
		got := mustBlock(t, "src", sharedplugin.SourceKindArtifact, "v1", h, tampered)
		if bytes.Equal(got, honest) {
			t.Errorf("%s: block unchanged — the signature would still verify", name)
		}
	}
}

// An unsignable list never becomes a block. Each of these would leave a grant whose
// approved bytes depend on something other than the operator's decision.
func TestCanonicalArtifacts_RefusesAmbiguousLists(t *testing.T) {
	good := artifactOf("linux", "amd64", "p", 1)
	cases := map[string][]SigilArtifact{
		"empty list": {},
		"duplicate platform": {good,
			artifactOf("linux", "amd64", "other", 2)},
		"half-stated platform": {
			{OS: "linux", Arch: "", Path: "p", SHA256: good.SHA256}},
		"unplatformed row beside a platform row": {good,
			artifactOf(AnyPlatform, AnyPlatform, "", 2)},
		"digest is not 64 lower-hex": {
			{OS: "linux", Arch: "amd64", Path: "p", SHA256: "ABC"}},
		"digest is uppercase hex": {
			{OS: "linux", Arch: "amd64", Path: "p", SHA256: strings.ToUpper(good.SHA256)}},
		"platform token is a path segment": {
			{OS: "..", Arch: "amd64", Path: "p", SHA256: good.SHA256}},
		"platform token carries a slash": {
			{OS: "linux", Arch: "a/b", Path: "p", SHA256: good.SHA256}},
	}
	for name, list := range cases {
		if _, err := CanonicalArtifacts(list); !errors.Is(err, ErrSigilArtifacts) {
			t.Errorf("%s: err = %v, want ErrSigilArtifacts", name, err)
		}
		if _, err := BuildSigilBlock("src", sharedplugin.SourceKindArtifact, "v1", []byte("doc"), list); err == nil {
			t.Errorf("%s: BuildSigilBlock accepted a list no signature should exist over", name)
		}
	}
}

// A duplicate platform is refused even when the two rows are IDENTICAL. The reason is
// not that the bytes disagree — it is that a list holding one platform twice has no
// single answer to "which row is this host's", and that ambiguity must not be settled
// by iteration order.
func TestCanonicalArtifacts_RefusesIdenticalDuplicate(t *testing.T) {
	a := artifactOf("linux", "amd64", "p", 1)
	if _, err := CanonicalArtifacts([]SigilArtifact{a, a}); !errors.Is(err, ErrSigilArtifacts) {
		t.Fatalf("err = %v, want ErrSigilArtifacts", err)
	}
}

func TestSelectArtifact(t *testing.T) {
	amd := artifactOf("linux", "amd64", "redis_linux_amd64", 1)
	arm := artifactOf("linux", "arm64", "redis_linux_arm64", 2)
	any := artifactOf(AnyPlatform, AnyPlatform, "", 3)

	if got := SelectArtifact([]SigilArtifact{amd, arm}, "linux", "amd64"); got == nil || got.SHA256 != amd.SHA256 {
		t.Errorf("exact match not selected: %+v", got)
	}
	// A release published for two platforms is simply not approved on a third. There
	// is nothing safe to substitute, so the answer is "no row".
	if got := SelectArtifact([]SigilArtifact{amd, arm}, "darwin", "arm64"); got != nil {
		t.Errorf("unapproved platform selected %+v, want nil", got)
	}
	// The git kind's single unplatformed artifact answers for every platform — which
	// is exactly what it did before grants carried a list.
	if got := SelectArtifact([]SigilArtifact{any}, "darwin", "arm64"); got == nil || got.SHA256 != any.SHA256 {
		t.Errorf("unplatformed artifact not selected: %+v", got)
	}
}
