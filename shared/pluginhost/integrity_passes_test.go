package pluginhost

import (
	"context"
	"crypto/ed25519"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// How many bytes of the artifact one Spawn is allowed to read (NIM-818).
//
// The measurement is `rchar` from /proc/self/io — bytes this process has passed to
// read(2), counted whether or not the page cache served them, which is exactly the
// question ("how many passes over the file") and not a proxy for it. Linux-only, so
// the test skips elsewhere; the gate runs on Linux.
//
// The subject: verifySigilAndSeal computes the artifact's digest once (step 1) and used
// to recompute it a second time — in verifyDigest on the sidecar-present branch, in
// sealDigest on the sidecar-absent branch — for a comparison whose first operand was
// already in a local variable. Every plugin step is a Spawn (one-shot per ADR-020(d)),
// so a 15-step destiny read a 25 MiB artifact 750 MiB worth instead of 375 MiB.
//
// The ceiling is 1.5 passes rather than 1.0: the trailer read, the sidecar and the
// directory listing are real reads too, and a threshold at exactly one pass would fail
// on those. Two passes is 2.0 and lands well outside — put the second
// computeFileDigest back and this test is what goes red.
const maxArtifactPassesPerSpawn = 1.5

// artifactBytesForPassCount — big enough that one pass dwarfs the fixed cost of the
// trailer, the sidecar and the test framework's own reads, small enough to stay fast.
const artifactBytesForPassCount = 8 << 20

// readChars returns this process's rchar counter: total bytes returned by read(2)
// syscalls so far, page-cache hits included.
func readChars(t testing.TB) int64 {
	t.Helper()
	b, err := os.ReadFile("/proc/self/io")
	if err != nil {
		t.Skipf("/proc/self/io unavailable: %v", err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		rest, ok := strings.CutPrefix(line, "rchar:")
		if !ok {
			continue
		}
		n, perr := strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
		if perr != nil {
			t.Fatalf("parse rchar %q: %v", line, perr)
		}
		return n
	}
	t.Fatal("/proc/self/io has no rchar line")
	return 0
}

// One Spawn reads the artifact ONCE — on both branches of the seal step, because both
// used to add a full second pass.
//
// This is the cost claim of NIM-818 made checkable. It is not a claim about security:
// the control is step 4, the comparison of the artifact's digest against the
// operator-approved hash for this platform, and that comparison runs on the digest this
// measures. Reusing the value makes "the bytes that were checked" and "the bytes that
// were sealed" the same bytes by construction; a second read could only ever observe
// bytes that [Host.Spawn] is about to exec anyway, since the exec comes after all of
// this.
func TestVerifySigilAndSeal_ReadsTheArtifactOncePerSpawn(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("read-byte accounting comes from /proc/self/io")
	}
	e := setupSigilEnvPadded(t, artifactBytesForPassCount)
	st, err := os.Stat(e.binPath)
	if err != nil {
		t.Fatalf("stat artifact: %v", err)
	}
	size := st.Size()
	if size < artifactBytesForPassCount {
		t.Fatalf("fixture artifact is %d bytes, want at least %d", size, artifactBytesForPassCount)
	}
	anchors := []ed25519.PublicKey{e.pub}
	look := lookupStub{testAlias: e.rec}
	sidecar := filepath.Join(e.dir, DigestSidecarName)

	// Branch 1: no sidecar yet — verify passes and seals. The seal used to re-hash.
	if _, serr := os.Stat(sidecar); serr == nil {
		t.Fatal("fixture already carries a sidecar; the seal branch would not be exercised")
	}
	before := readChars(t)
	if verr := verifySigilAndSeal(context.Background(), e.dir, e.binPath, testAlias, anchors, look); verr != nil {
		t.Fatalf("verifySigilAndSeal (seal branch): %v", verr)
	}
	sealPasses := float64(readChars(t)-before) / float64(size)
	if sealPasses > maxArtifactPassesPerSpawn {
		t.Errorf("seal branch read the artifact %.2f times, want at most %.2f", sealPasses, maxArtifactPassesPerSpawn)
	}

	// Branch 2: the sidecar is now there — verify compares instead of writing. The
	// comparison used to re-hash for a value already held in binDigestHex.
	if _, serr := os.Stat(sidecar); serr != nil {
		t.Fatalf("sidecar not sealed by the first call: %v", serr)
	}
	before = readChars(t)
	if verr := verifySigilAndSeal(context.Background(), e.dir, e.binPath, testAlias, anchors, look); verr != nil {
		t.Fatalf("verifySigilAndSeal (compare branch): %v", verr)
	}
	comparePasses := float64(readChars(t)-before) / float64(size)
	if comparePasses > maxArtifactPassesPerSpawn {
		t.Errorf("compare branch read the artifact %.2f times, want at most %.2f", comparePasses, maxArtifactPassesPerSpawn)
	}

	t.Logf("artifact %d bytes: seal branch %.2f passes, compare branch %.2f passes", size, sealPasses, comparePasses)
}

// Fewer passes must not mean a weaker check: a sidecar that disagrees with the artifact
// is still a mismatch, and the value compared against it is still derived from the file
// on this call rather than remembered from a previous one.
func TestVerifySigilAndSeal_SidecarMismatchStillFails(t *testing.T) {
	e := setupSigilEnv(t)
	anchors := []ed25519.PublicKey{e.pub}
	look := lookupStub{testAlias: e.rec}

	sidecar := filepath.Join(e.dir, DigestSidecarName)
	if err := os.WriteFile(sidecar, []byte(strings.Repeat("0", 64)), 0o600); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}
	err := verifySigilAndSeal(context.Background(), e.dir, e.binPath, testAlias, anchors, look)
	if err == nil {
		t.Fatal("a sidecar that does not match the artifact must fail the spawn")
	}
	if !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("err = %v, want a digest mismatch", err)
	}
}

// The sealed sidecar records the digest the grant was checked against, byte for byte.
// The seal used to re-derive it from the path, which is the same value only for as long
// as nothing touches the file between the two reads.
func TestVerifySigilAndSeal_SealsTheVerifiedDigest(t *testing.T) {
	e := setupSigilEnv(t)
	anchors := []ed25519.PublicKey{e.pub}
	look := lookupStub{testAlias: e.rec}

	if err := verifySigilAndSeal(context.Background(), e.dir, e.binPath, testAlias, anchors, look); err != nil {
		t.Fatalf("verifySigilAndSeal: %v", err)
	}
	sealed, err := os.ReadFile(filepath.Join(e.dir, DigestSidecarName))
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	want := e.rec.Artifacts[0].SHA256
	if string(sealed) != want {
		t.Fatalf("sidecar = %q, want the approved digest %q", sealed, want)
	}
}

// BenchmarkVerifySigilAndSeal reports the spawn-path integrity gate in bytes of
// artifact per second. Both branches, because they had one extra pass each.
func BenchmarkVerifySigilAndSeal(b *testing.B) {
	for _, branch := range []string{"seal", "compare"} {
		b.Run(branch, func(b *testing.B) {
			e := setupSigilEnvPadded(b, artifactBytesForPassCount)
			st, err := os.Stat(e.binPath)
			if err != nil {
				b.Fatalf("stat artifact: %v", err)
			}
			anchors := []ed25519.PublicKey{e.pub}
			look := lookupStub{testAlias: e.rec}
			sidecar := filepath.Join(e.dir, DigestSidecarName)

			if branch == "compare" {
				if err := verifySigilAndSeal(context.Background(), e.dir, e.binPath, testAlias, anchors, look); err != nil {
					b.Fatalf("prime sidecar: %v", err)
				}
			}
			b.SetBytes(st.Size())
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if branch == "seal" {
					b.StopTimer()
					_ = os.Remove(sidecar)
					b.StartTimer()
				}
				if err := verifySigilAndSeal(context.Background(), e.dir, e.binPath, testAlias, anchors, look); err != nil {
					b.Fatalf("verifySigilAndSeal: %v", err)
				}
			}
		})
	}
}
