package pluginhost

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestNormalizeManifestBytes(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
		want []byte
	}{
		{"strip BOM", []byte{0xEF, 0xBB, 0xBF, 'a', ':', ' ', '1', '\n'}, []byte("a: 1\n")},
		{"CRLF to LF", []byte("a: 1\r\nb: 2\r\n"), []byte("a: 1\nb: 2\n")},
		{"lone CR to LF", []byte("a: 1\rb: 2"), []byte("a: 1\nb: 2\n")},
		{"no trailing newline added", []byte("a: 1"), []byte("a: 1\n")},
		{"multiple trailing newlines collapsed", []byte("a: 1\n\n\n"), []byte("a: 1\n")},
		{"single trailing newline preserved", []byte("a: 1\n"), []byte("a: 1\n")},
		{"CRLF trailing collapsed to one LF", []byte("a: 1\r\n\r\n"), []byte("a: 1\n")},
		{"empty input becomes single newline", []byte(""), []byte("\n")},
		{"BOM + CRLF + missing trailing", []byte{0xEF, 0xBB, 0xBF, 'x', '\r', '\n', 'y'}, []byte("x\ny\n")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NormalizeManifestBytes(tt.in)
			if !bytes.Equal(got, tt.want) {
				t.Errorf("NormalizeManifestBytes(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestBuildSigilBlock_Deterministic(t *testing.T) {
	bin := bytes.Repeat([]byte{0x01}, 32)
	man := bytes.Repeat([]byte{0x02}, 32)
	a := BuildSigilBlock("cloud", "hetzner", "v1.0.0", bin, man)
	b := BuildSigilBlock("cloud", "hetzner", "v1.0.0", bin, man)
	if !bytes.Equal(a, b) {
		t.Fatalf("BuildSigilBlock not deterministic:\n a=%x\n b=%x", a, b)
	}
}

func TestBuildSigilBlock_HasDST(t *testing.T) {
	block := BuildSigilBlock("ns", "name", "ref", []byte("bin"), []byte("man"))
	dst := []byte("soul-stack/sigil/v1")
	if !bytes.HasPrefix(block, dst) {
		t.Fatalf("block does not start with DST %q; block=%x", dst, block)
	}
}

// LP boundary: ("ab","c") and ("a","bc") yield DIFFERENT blocks. Without a
// length-prefix they would be identical — the core field-boundary invariant.
func TestBuildSigilBlock_LengthPrefixBoundary(t *testing.T) {
	h := bytes.Repeat([]byte{0x00}, 32)
	x := BuildSigilBlock("ab", "c", "ref", h, h)
	y := BuildSigilBlock("a", "bc", "ref", h, h)
	if bytes.Equal(x, y) {
		t.Fatal("LP boundary broken: (\"ab\",\"c\") == (\"a\",\"bc\")")
	}

	// Same for adjacent ref / binary-hash fields: move a byte across the boundary.
	p := BuildSigilBlock("ns", "name", "r", []byte("ab"), h)
	q := BuildSigilBlock("ns", "name", "ra", []byte("b"), h)
	if bytes.Equal(p, q) {
		t.Fatal("LP boundary broken across ref/binary fields")
	}
}

// Exact block layout: DST || LP(ns) || LP(name) || LP(ref) || LP(binary) ||
// LP(manifest), with field order and raw (not hex) hashes.
func TestBuildSigilBlock_ExactLayoutAndFieldOrder(t *testing.T) {
	ns, name, ref := "cloud", "hetzner", "v1"
	bin := []byte{0xAA, 0xBB}
	man := []byte{0xCC, 0xDD, 0xEE}

	got := BuildSigilBlock(ns, name, ref, bin, man)

	var want bytes.Buffer
	want.WriteString("soul-stack/sigil/v1")
	for _, f := range [][]byte{[]byte(ns), []byte(name), []byte(ref), bin, man} {
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

// Swapping fields yields a different block (field order is fixed).
func TestBuildSigilBlock_FieldOrderMatters(t *testing.T) {
	h := bytes.Repeat([]byte{0x00}, 32)
	a := BuildSigilBlock("x", "y", "ref", h, h)
	b := BuildSigilBlock("y", "x", "ref", h, h)
	if bytes.Equal(a, b) {
		t.Fatal("swapping namespace/name produced identical block")
	}
}
