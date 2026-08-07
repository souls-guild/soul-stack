package pluginhost

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"testing"
)

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
	bin := bytes.Repeat([]byte{0x01}, 32)
	doc := bytes.Repeat([]byte{0x02}, 32)
	a := BuildSigilBlock("https://github.com/souls-guild/soul-mod-redis", "v1.0.0", bin, doc)
	b := BuildSigilBlock("https://github.com/souls-guild/soul-mod-redis", "v1.0.0", bin, doc)
	if !bytes.Equal(a, b) {
		t.Fatalf("BuildSigilBlock not deterministic:\n a=%x\n b=%x", a, b)
	}
}

// The DST is v2: v1 keyed on (namespace, name), both of which the artifact no longer
// carries. A v1 signature must not verify against the new shape, and the tag is what
// makes that break explicit rather than silent.
func TestBuildSigilBlock_HasVersionedDST(t *testing.T) {
	block := BuildSigilBlock("source", "ref", []byte("bin"), []byte("doc"))
	dst := []byte("soul-stack/sigil/v2")
	if !bytes.HasPrefix(block, dst) {
		t.Fatalf("block does not start with DST %q; block=%x", dst, block)
	}
}

// LP boundary: ("ab","c") and ("a","bc") yield DIFFERENT blocks. Without a
// length-prefix they would be identical — the core field-boundary invariant.
func TestBuildSigilBlock_LengthPrefixBoundary(t *testing.T) {
	h := bytes.Repeat([]byte{0x00}, 32)
	x := BuildSigilBlock("ab", "c", h, h)
	y := BuildSigilBlock("a", "bc", h, h)
	if bytes.Equal(x, y) {
		t.Fatal("LP boundary broken: (\"ab\",\"c\") == (\"a\",\"bc\")")
	}

	// Same across the adjacent ref / binary-hash fields: move a byte over the boundary.
	p := BuildSigilBlock("src", "r", []byte("ab"), h)
	q := BuildSigilBlock("src", "ra", []byte("b"), h)
	if bytes.Equal(p, q) {
		t.Fatal("LP boundary broken across ref/binary fields")
	}
}

// Exact block layout: DST || LP(source) || LP(ref) || LP(binary) || LP(schema), with
// field order fixed and raw (not hex) hashes.
func TestBuildSigilBlock_ExactLayoutAndFieldOrder(t *testing.T) {
	source, ref := "https://example.com/soul-mod-redis.git", "v1"
	bin := []byte{0xAA, 0xBB}
	doc := []byte{0xCC, 0xDD, 0xEE}

	got := BuildSigilBlock(source, ref, bin, doc)

	var want bytes.Buffer
	want.WriteString("soul-stack/sigil/v2")
	for _, f := range [][]byte{[]byte(source), []byte(ref), bin, doc} {
		var lp [4]byte
		binary.BigEndian.PutUint32(lp[:], uint32(len(f)))
		want.Write(lp[:])
		want.Write(f)
	}
	if !bytes.Equal(got, want.Bytes()) {
		t.Fatalf("block layout mismatch:\n got=%x\nwant=%x", got, want.Bytes())
	}

	// Raw hashes, not hex: byte 0xAA sits in the block as 0xAA, not as "aa".
	if !bytes.Contains(got, bin) {
		t.Error("binary hash bytes not present raw in block")
	}
}

// Swapping source and ref yields a different block (field order is fixed).
func TestBuildSigilBlock_FieldOrderMatters(t *testing.T) {
	h := bytes.Repeat([]byte{0x00}, 32)
	a := BuildSigilBlock("x", "y", h, h)
	b := BuildSigilBlock("y", "x", h, h)
	if bytes.Equal(a, b) {
		t.Fatal("swapping source/ref produced identical block")
	}
}

// The registration alias is not a parameter of the block at all, so no test here can
// do more than restate the signature. The invariant that matters — one signature
// covering the same artifact under two aliases — is exercised end to end in
// TestSigilVerifySecondAliasNeedsNoNewSignature.
