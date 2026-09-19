package bootstrap

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
)

const pendingTestSID = "host.example.com"

// spkiOf is the client-side view of what the Keeper fingerprints: the DER
// SubjectPublicKeyInfo. Two CSRs are "the same key" exactly when these match,
// which is the whole condition the recovery path turns on (NIM-865).
func spkiOf(t *testing.T, csrPEM []byte) string {
	t.Helper()
	block, _ := pem.Decode(csrPEM)
	if block == nil {
		t.Fatal("csr is not PEM")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		t.Fatalf("ParseCertificateRequest: %v", err)
	}
	return string(csr.RawSubjectPublicKeyInfo)
}

// The property the server side depends on: a retry presents the SAME public
// key. Generating a fresh one per attempt — which is what this did before —
// makes every retry look to the Keeper like a second host claiming the SID,
// and a burned token refuses it forever.
func TestLoadOrCreatePendingKey_RetryPresentsTheSameKey(t *testing.T) {
	dir := t.TempDir()

	_, csr1, reused1, err := loadOrCreatePendingKey(dir, pendingTestSID)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if reused1 {
		t.Error("first attempt reported a reused key with nothing on disk")
	}

	_, csr2, reused2, err := loadOrCreatePendingKey(dir, pendingTestSID)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if !reused2 {
		t.Error("retry did not report reusing the pending key")
	}
	if spkiOf(t, csr1) != spkiOf(t, csr2) {
		t.Error("retry presented a different public key — the Keeper would refuse it as a second binding")
	}
}

// The pending key is scratch, not a credential: once the seed exists the retry
// it enables cannot be needed, and leaving a certificate-less private key
// lying about is exactly what the package header promises not to do.
func TestClearPendingKey_RemovesItAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	if _, _, _, err := loadOrCreatePendingKey(dir, pendingTestSID); err != nil {
		t.Fatalf("create: %v", err)
	}
	path := filepath.Join(dir, PendingKeyFile)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("pending key not written: %v", err)
	}

	if err := clearPendingKey(dir); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("pending key still present after clear: %v", err)
	}
	// A seed written by an older build leaves no pending key; clearing must
	// not turn that into a failed onboarding.
	if err := clearPendingKey(dir); err != nil {
		t.Errorf("clear on a missing file: %v, want nil", err)
	}
}

// A corrupt scratch file can only have come from an attempt that already
// failed. Refusing to onboard over it would convert a recoverable state into a
// permanently stuck one — the opposite of the point.
func TestLoadOrCreatePendingKey_ReplacesACorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, PendingKeyFile)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("not a key\n"), 0o400); err != nil {
		t.Fatalf("write junk: %v", err)
	}

	_, csr, reused, err := loadOrCreatePendingKey(dir, pendingTestSID)
	if err != nil {
		t.Fatalf("over a corrupt file: %v, want a fresh key", err)
	}
	if reused {
		t.Error("reported reusing a key it could not parse")
	}
	if len(csr) == 0 {
		t.Fatal("no CSR produced")
	}
	// Replaced on disk, so the NEXT retry has something to reuse — otherwise
	// every attempt starts over and the recovery never engages.
	if _, csr2, reused2, err := loadOrCreatePendingKey(dir, pendingTestSID); err != nil {
		t.Fatalf("second: %v", err)
	} else if !reused2 || spkiOf(t, csr) != spkiOf(t, csr2) {
		t.Error("the replacement key was not persisted")
	}
}

// ★ The key is bound to ONE SID, server-side and for good. A pending key that
// travels to a different host must therefore not be reused — otherwise a golden
// image snapshotted after a failed `soul init` carries one key into every
// clone, the first clone binds it, and every other clone is refused forever
// with nothing on the wire to say why.
func TestLoadOrCreatePendingKey_RegeneratesForADifferentSID(t *testing.T) {
	dir := t.TempDir()

	_, csr1, _, err := loadOrCreatePendingKey(dir, "clone-a.example.com")
	if err != nil {
		t.Fatalf("first host: %v", err)
	}

	_, csr2, reused, err := loadOrCreatePendingKey(dir, "clone-b.example.com")
	if err != nil {
		t.Fatalf("second host: %v", err)
	}
	if reused {
		t.Error("reused a key generated for another SID")
	}
	if spkiOf(t, csr1) == spkiOf(t, csr2) {
		t.Error("two hosts presented the same public key — only one of them could ever onboard")
	}

	// And the replacement belongs to the second host, so ITS retry still works.
	if _, csr3, reused3, err := loadOrCreatePendingKey(dir, "clone-b.example.com"); err != nil {
		t.Fatalf("second host retry: %v", err)
	} else if !reused3 || spkiOf(t, csr2) != spkiOf(t, csr3) {
		t.Error("the second host cannot reuse its own key")
	}
}

// A process killed between the write and the rename leaves a complete private
// key behind at 0400; nothing else would ever collect it, and — because
// `writePendingKey` writes to a FIXED name — the next attempt's `os.WriteFile`
// hits EACCES on it and the host can never onboard again.
//
// The straggler is therefore seeded at 0400 (what the code actually leaves) and
// under a name the happy path does NOT overwrite, or the sweep would be
// invisible: `pending-key.pem.tmp` is renamed away by the very write under test.
func TestPendingKeyTemps_AreSweptSoAReadOnlyLeftoverCannotWedgeOnboarding(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Two stragglers: the exact name the current code uses, and a differently
	// suffixed one so the sweep is shown to be a glob rather than one unlink.
	for _, name := range []string{PendingKeyFile + ".tmp", PendingKeyFile + ".914273"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("half-written key\n"), 0o400); err != nil {
			t.Fatalf("seed straggler %s: %v", name, err)
		}
	}

	if _, _, _, err := loadOrCreatePendingKey(dir, pendingTestSID); err != nil {
		t.Fatalf("create over read-only stragglers: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, PendingKeyFile+".914273")); !os.IsNotExist(err) {
		t.Errorf("straggler survived a fresh write: %v", err)
	}

	// Nothing is left over once the seed lands: the write swept the stragglers
	// and clear removes the key itself.
	if err := clearPendingKey(dir); err != nil {
		t.Fatalf("clear: %v", err)
	}
	left, err := filepath.Glob(filepath.Join(dir, PendingKeyFile+"*"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(left) != 0 {
		t.Errorf("files left after clear: %v", left)
	}
}

// Mode matters: this is the same secret seed.Write lays down at 0400, in the
// same directory, and a world-readable private key would be a real regression
// dressed as a scratch file.
func TestWritePendingKey_Mode0400(t *testing.T) {
	dir := t.TempDir()
	if _, _, _, err := loadOrCreatePendingKey(dir, pendingTestSID); err != nil {
		t.Fatalf("create: %v", err)
	}
	fi, err := os.Stat(filepath.Join(dir, PendingKeyFile))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o400 {
		t.Errorf("pending key mode = %04o, want 0400", got)
	}
}
