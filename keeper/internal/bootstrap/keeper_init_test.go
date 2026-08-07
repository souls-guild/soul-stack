package bootstrap

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// Pure unit tests — do not require PG/Vault. Logic that fully depends on
// pgxpool.Pool / VaultClient.ReadKV (advisory lock, Insert, ReadKV
// round-trip) is covered by integration_test.go under
// `//go:build integration`.

// ParseRef tests moved to `keeper/internal/vault/parseref_test.go` after
// the parser was extracted into the shared keeper-vault helper (M0.5d).

func TestExtractSigningKey_Base64String(t *testing.T) {
	raw := []byte("0123456789abcdef0123456789abcdef") // 32 bytes
	encoded := base64.StdEncoding.EncodeToString(raw)
	got, err := extractSigningKey(map[string]any{"signing_key": encoded})
	if err != nil {
		t.Fatalf("extractSigningKey: %v", err)
	}
	if string(got) != string(raw) {
		t.Errorf("decoded = %q, want %q", got, raw)
	}
}

func TestExtractSigningKey_NonBase64StringFallback(t *testing.T) {
	// If the value isn't base64 — return raw bytes (32 ASCII bytes).
	raw := "0123456789abcdef0123456789abcdef!" // 33 bytes, not valid base64
	got, err := extractSigningKey(map[string]any{"signing_key": raw})
	if err != nil {
		t.Fatalf("extractSigningKey: %v", err)
	}
	if string(got) != raw {
		t.Errorf("got = %q, want raw %q", got, raw)
	}
}

func TestExtractSigningKey_Bytes(t *testing.T) {
	raw := []byte("0123456789abcdef0123456789abcdef")
	got, err := extractSigningKey(map[string]any{"signing_key": raw})
	if err != nil {
		t.Fatalf("extractSigningKey: %v", err)
	}
	if string(got) != string(raw) {
		t.Errorf("got = %q, want %q", got, raw)
	}
}

func TestExtractSigningKey_Missing(t *testing.T) {
	if _, err := extractSigningKey(map[string]any{"other": "x"}); !errors.Is(err, ErrSigningKeyMissing) {
		t.Errorf("err = %v, want ErrSigningKeyMissing", err)
	}
}

func TestExtractSigningKey_EmptyString(t *testing.T) {
	if _, err := extractSigningKey(map[string]any{"signing_key": ""}); !errors.Is(err, ErrSigningKeyMissing) {
		t.Errorf("err = %v, want ErrSigningKeyMissing", err)
	}
}

func TestExtractSigningKey_UnsupportedType(t *testing.T) {
	if _, err := extractSigningKey(map[string]any{"signing_key": 42}); err == nil {
		t.Errorf("unsupported type: expected error, got nil")
	}
}

func TestWriteTokenFile_PermissionsAndContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	const token = "header.payload.signature"
	isStream, err := writeTokenFile(path, token)
	if err != nil {
		t.Fatalf("writeTokenFile: %v", err)
	}
	if isStream {
		t.Errorf("isStream = true for a regular file; the 0400 guarantee holds here and the caller must not warn")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != credentialFileMode {
		t.Errorf("file mode = %o, want %o", mode, credentialFileMode)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != token+"\n" {
		t.Errorf("content = %q, want %q", got, token+"\n")
	}
}

func TestWriteTokenFile_OverwritesAndChmodsBackTo0400(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	// Pre-create with 0644 — writeTokenFile must explicitly chmod to 0400.
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	isStream, err := writeTokenFile(path, "new")
	if err != nil {
		t.Fatalf("writeTokenFile: %v", err)
	}
	if isStream {
		t.Errorf("isStream = true for a regular file")
	}
	info, _ := os.Stat(path)
	if mode := info.Mode().Perm(); mode != credentialFileMode {
		t.Errorf("file mode after rewrite = %o, want %o", mode, credentialFileMode)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "new\n" {
		t.Errorf("content = %q, want \"new\\n\"", got)
	}
}

// NIM-420. A fifo stands in for `/dev/stdout`: not a regular file, and
// the old remove-then-create path died on it with `remove /dev/stdout:
// permission denied` — or, running as root, would have succeeded and
// destroyed the device node.
//
// The two assertions are separate on purpose. Content proves the token
// got through; the surviving fifo proves it got through the in-place
// branch and not by silently replacing the operator's stream with a file
// of the same name.
func TestWriteTokenFile_FifoIsWrittenInPlaceNotUnlinked(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stdout-stand-in")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo unsupported here: %v", err)
	}

	const token = "header.payload.signature"
	// The read end must already be open when writeTokenFile runs: an
	// O_WRONLY open on a readerless fifo now fails fast by design (see
	// TestWriteTokenFile_FifoWithNoReaderFailsInsteadOfHanging), so a
	// reader racing in from a goroutine would make this test flaky
	// rather than merely slow. Hence the synchronous open here, and
	// only the reading is backgrounded.
	r, dropWriterPin := openFifoReader(t, path)
	read := make(chan string, 1)
	go func() {
		b, err := io.ReadAll(r)
		if err != nil {
			read <- "read error: " + err.Error()
			return
		}
		read <- string(b)
	}()

	isStream, err := writeTokenFile(path, token)
	if err != nil {
		t.Fatalf("writeTokenFile on a fifo: %v", err)
	}
	if !isStream {
		t.Errorf("isStream = false for a fifo; nothing guarantees a mode here and the caller must warn")
	}
	// writeTokenFile has closed its write end; dropping ours is what
	// lets the reader reach EOF.
	dropWriterPin()

	select {
	case got := <-read:
		if got != token+"\n" {
			t.Errorf("fifo content = %q, want %q", got, token+"\n")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("nothing arrived on the fifo")
	}

	st, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("the fifo is gone — it was unlinked instead of written to: %v", err)
	}
	if st.Mode()&os.ModeNamedPipe == 0 {
		t.Errorf("path is now %v, want a fifo — the stream was replaced by a regular file", st.Mode())
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("fifo mode = %o, want 0600 — the 0400 chmod leaked onto a stream", perm)
	}
}

// A readerless fifo is the one destination that can hang, and it would
// hang at the worst possible moment: writeTokenFile runs after the
// Archon is committed and the audit event is written, and by then
// signal.NotifyContext owns SIGINT, so the operator cannot even Ctrl-C
// out. What they would be left with is a cluster that reports itself
// initialized and a bootstrap token that reached nobody — `keeper init`
// refuses to run twice, so there is no second chance at it.
//
// O_NONBLOCK turns that into an immediate ENXIO, which reaches the
// operator as ErrTokenFileWriteFailed with the token printed for
// recovery. Bare, that error reads `no such device or address` about a
// path they can see in front of them, so the cause is spelled out: this
// is the one refusal fixed by doing something on the other end.
func TestWriteTokenFile_FifoWithNoReaderFailsInsteadOfHanging(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nobody-listening")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo unsupported here: %v", err)
	}

	// Deliberately no reader. Without O_NONBLOCK the open below never
	// returns, so this has to be raced against a deadline rather than
	// simply called — a blocked goroutine is unkillable and lives until
	// the test binary exits.
	done := make(chan error, 1)
	go func() {
		_, err := writeTokenFile(path, "header.payload.signature")
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("writeTokenFile succeeded on a fifo nobody is reading")
		}
		if !strings.Contains(err.Error(), path) {
			t.Errorf("error %q does not name the destination", err)
		}
		if !errors.Is(err, syscall.ENXIO) {
			t.Errorf("error %q does not wrap ENXIO — the cause is no longer inspectable", err)
		}
		for _, want := range []string{"reading", "--credential-out=-"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not contain %q — `no such device or address` alone does not say a reader is missing", err, want)
			}
		}
	case <-time.After(10 * time.Second):
		t.Fatal("writeTokenFile blocked on a readerless fifo — after the Archon is already committed, this is an unrecoverable bootstrap")
	}
}

// A socket answers the open with ENXIO too, for an unrelated reason:
// open(2) cannot open a socket at all, and no reader will ever change
// that. It must not be handed the fifo's advice — this is a path the
// operator did not mean, or one somebody else prepared, and "start the
// reader first" sends them looking for a process that does not exist.
func TestWriteTokenFile_SocketGetsNoFifoAdvice(t *testing.T) {
	path := shortSocketPath(t)
	ln, err := net.Listen("unix", path)
	if err != nil {
		// Only a platform without unix sockets is a reason to skip. Any
		// other failure means the test did not run for a reason worth
		// hearing about — see shortSocketPath.
		if errors.Is(err, syscall.EAFNOSUPPORT) || errors.Is(err, syscall.ENOSYS) {
			t.Skipf("unix sockets unsupported here: %v", err)
		}
		t.Fatalf("listen on %s (%d bytes): %v", path, len(path), err)
	}
	defer ln.Close() //nolint:errcheck // test cleanup

	_, err = writeTokenFile(path, "header.payload.signature")
	if err == nil {
		t.Fatal("writeTokenFile succeeded against a socket")
	}
	if strings.Contains(err.Error(), "reading") {
		t.Errorf("error %q offers the fifo remedy for a socket", err)
	}
	if st, serr := os.Lstat(path); serr != nil || st.Mode()&os.ModeSocket == 0 {
		t.Errorf("the socket is gone (mode=%v, err=%v) — it was unlinked and replaced", st, serr)
	}
}

// sun_path caps a unix socket path at 108 bytes, and t.TempDir() derives
// from TMPDIR, which CI is free to make longer than that. The test above
// used to skip when the bind failed, so on such a runner it disappeared
// without saying so — a guard that evaporates with the environment reads
// as coverage while providing none.
func shortSocketPath(t *testing.T) string {
	t.Helper()
	if p := filepath.Join(t.TempDir(), "s"); len(p) < 100 {
		return p
	}
	dir, err := os.MkdirTemp("/tmp", "ss")
	if err != nil {
		t.Fatalf("no directory short enough for a unix socket: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "s")
}

// The fifo remedy is gated on two things — ENXIO, and the name having
// been a fifo — and only the second half was pinned. A fifo can fail to
// open for reasons no reader will fix; here it is one the operator
// cannot write to. Telling them to "start the reader first" sends them
// after a process that was never the problem, which is the same defect
// as the socket case above wearing different clothes.
func TestWriteTokenFile_UnopenableFifoGetsNoReaderAdvice(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root opens a mode-0000 fifo anyway, so there is no EACCES here to observe")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "forbidden")
	if err := syscall.Mkfifo(path, 0o000); err != nil {
		t.Skipf("mkfifo unsupported here: %v", err)
	}

	_, err := writeTokenFile(path, "header.payload.signature")
	if err == nil {
		t.Fatal("writeTokenFile succeeded on a fifo it cannot open")
	}
	// Asserted before the real check: if the open starts failing for some
	// other reason, this test stops exercising the non-ENXIO branch and
	// would otherwise pass while testing nothing.
	if !errors.Is(err, syscall.EACCES) {
		t.Fatalf("error %q does not wrap EACCES — this no longer reaches the branch it is testing", err)
	}
	if strings.Contains(err.Error(), "reading") {
		t.Errorf("error %q offers the fifo reader remedy for a permission failure", err)
	}
}

// The character-device half of the allowlist, run end to end instead of
// as a mode. streamTypeError is tested as a pure function because a
// block device cannot be created without CAP_MKNOD — but a char device
// needs no privilege at all, /dev/null being one already, and without
// this the only destination any test drives through the whole in-place
// path is a fifo.
func TestWriteTokenFile_CharDeviceIsWrittenInPlace(t *testing.T) {
	before, err := os.Stat(os.DevNull)
	if err != nil || before.Mode()&os.ModeCharDevice == 0 {
		t.Skipf("%s is not a character device here (err=%v)", os.DevNull, err)
	}

	isStream, err := writeTokenFile(os.DevNull, "header.payload.signature")
	if err != nil {
		t.Fatalf("writeTokenFile on %s: %v", os.DevNull, err)
	}
	if !isStream {
		t.Error("isStream = false for a character device; nothing guarantees a mode here and the caller must warn")
	}

	after, err := os.Stat(os.DevNull)
	if err != nil {
		t.Fatalf("%s is gone — it was unlinked instead of written through: %v", os.DevNull, err)
	}
	if after.Mode()&os.ModeCharDevice == 0 {
		t.Errorf("%s is now %v — the device node was replaced by a regular file", os.DevNull, after.Mode())
	}
	if after.Mode().Perm() != before.Mode().Perm() {
		t.Errorf("%s mode %o → %o — the 0400 chmod leaked onto a device node",
			os.DevNull, before.Mode().Perm(), after.Mode().Perm())
	}
}

// The `/dev/stdout` shape exactly: a symlink whose target is a stream.
// It must be followed and written through, and the link itself must
// survive — the regular-file branch would unlink it and leave a plain
// file where the operator's device node used to be.
func TestWriteTokenFile_SymlinkToStreamIsFollowedNotReplaced(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target-fifo")
	link := filepath.Join(dir, "link")
	if err := syscall.Mkfifo(target, 0o600); err != nil {
		t.Skipf("mkfifo unsupported here: %v", err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}

	const token = "header.payload.signature"
	r, dropWriterPin := openFifoReader(t, target)
	read := make(chan string, 1)
	go func() {
		b, err := io.ReadAll(r)
		if err != nil {
			read <- "read error: " + err.Error()
			return
		}
		read <- string(b)
	}()

	isStream, err := writeTokenFile(link, token)
	if err != nil {
		t.Fatalf("writeTokenFile through a symlink to a fifo: %v", err)
	}
	if !isStream {
		t.Errorf("isStream = false; nothing guarantees a mode on the other side of a fifo")
	}
	dropWriterPin()

	select {
	case got := <-read:
		if got != token+"\n" {
			t.Errorf("fifo content = %q, want %q", got, token+"\n")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("nothing arrived through the symlink")
	}

	st, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("Lstat link: %v", err)
	}
	if st.Mode()&os.ModeSymlink == 0 {
		t.Errorf("link is now %v, want a symlink — it was replaced instead of followed", st.Mode())
	}
}

// The security guard, and the os.Lstat-not-os.Stat guard with it.
//
// os.Lstat can only classify the *name*. A symlink sitting at the
// credential path may lead anywhere, and whoever can create a name in
// that directory chooses where: writing through it puts a cluster-admin
// JWT into a file of their choosing, and `keeper init` runs as root
// often enough for that to matter. So the destination is re-checked by
// fstat once open, and a regular file is refused.
//
// `--credential-out=/dev/stdout >file` arrives at that same branch and
// is refused with it, which is not collateral damage but the reason the
// refusal can be unconditional: an fstat cannot tell the operator's own
// redirect from somebody else's planted link. Nothing subtler is
// available, so neither is allowed, and `--credential-out=-` serves the
// redirect without resolving a path at all.
//
// Under os.Stat instead of os.Lstat this reaches writeTokenRegularFile,
// which succeeds: the symlink is unlinked and replaced by a 0400 file.
// Both the error and the surviving link below fail in that case.
func TestWriteTokenFile_SymlinkToRegularFileIsRefused(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	link := filepath.Join(dir, "link")

	seed := strings.Repeat("x", 200)
	if err := os.WriteFile(target, []byte(seed), 0o644); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}

	_, err := writeTokenFile(link, "header.payload.signature")
	if err == nil {
		t.Fatal("writeTokenFile wrote the bootstrap token through a symlink into a regular file")
	}
	if !strings.Contains(err.Error(), "--credential-out=-") {
		t.Errorf("error %q does not point at the supported way to do this", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile target: %v", err)
	}
	if string(got) != seed {
		t.Errorf("target content = %q, want it untouched — the token was written into a file chosen by whoever planted the link", got)
	}

	st, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("Lstat link: %v", err)
	}
	if st.Mode()&os.ModeSymlink == 0 {
		t.Errorf("link is now %v, want a symlink — os.Stat semantics are back and the link was unlinked", st.Mode())
	}
	if perm := st.Mode().Perm(); perm == 0o400 {
		t.Errorf("a 0400 file replaced the link")
	}
}

// A dangling symlink is where this deliberately gives up something the
// old code handled: os.Remove used to drop the link and O_EXCL create a
// fresh 0400 file, so `keeper init` succeeded. The in-place open has no
// O_CREATE and fails with ENOENT instead.
//
// Keeping it that way is the point, and the reason is the same one that
// motivates the whole check: a symlink to a path that does not exist yet
// is exactly what gets planted to have a privileged process create a
// file somewhere it should not. Following it with O_CREATE would be the
// obvious "fix" and would hand that back. The failure is recoverable —
// ErrTokenFileWriteFailed puts the token on stderr — so this pins the
// refusal, not a bug.
func TestWriteTokenFile_DanglingSymlinkIsRefusedAndNotFollowed(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "does-not-exist")
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}

	if _, err := writeTokenFile(link, "header.payload.signature"); err == nil {
		t.Fatal("writeTokenFile succeeded through a dangling symlink")
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Errorf("%s was created (err=%v) — the link was followed with O_CREATE", target, err)
	}

	st, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("the link is gone: %v", err)
	}
	if st.Mode()&os.ModeSymlink == 0 {
		t.Errorf("link is now %v, want a symlink — it was replaced by a file", st.Mode())
	}
}

// The type check is an allowlist, and the case that forces it is the one
// no test can set up: mknod needs CAP_MKNOD, and pointing the writer at a
// real /dev/sda to prove it is refused would cost a disk the first time
// the guard regressed. Hence a pure function over a mode.
//
// `IsRegular()` alone looks like it covers this — it is false for a block
// device — but it is a denylist of exactly one type (`mode&ModeType == 0`)
// and everything else falls through to the write. Of that everything
// else, a block device is the one that survives: a directory is EISDIR
// and a socket ENXIO at the open, while /dev/sda opens cleanly and takes
// the bootstrap JWT at offset zero, over the partition table. Root
// `keeper init` plus a typo, or plus a link planted where the credential
// was going, is the whole distance to that.
func TestStreamTypeError_AdmitsStreamsAndRefusesTheRest(t *testing.T) {
	t.Parallel()
	// The wanted substrings have to separate the branches, not merely
	// prove that something was refused: "refusing to write" appears in
	// three of the four refusals, so asserting on it would let a mutation
	// that routes every type into one branch pass.
	for _, tc := range []struct {
		name string
		mode os.FileMode
		want []string // substrings of the refusal, nil = must be admitted
	}{
		{"char device", os.ModeDevice | os.ModeCharDevice | 0o666, nil},
		{"fifo", os.ModeNamedPipe | 0o600, nil},
		{"regular file", 0o644, []string{"regular file", "--credential-out=-"}},
		{"block device", os.ModeDevice | 0o660, []string{"block device"}},
		// The rendered mode is asserted whole: a bare "d" would be
		// matched by "leads" and by "credential", proving nothing.
		{"socket", os.ModeSocket | 0o755, []string{"neither a stream nor a file", "S---------"}},
		{"directory", os.ModeDir | 0o755, []string{"neither a stream nor a file", "d---------"}},
	} {
		err := streamTypeError("/tmp/target", tc.mode)
		if tc.want == nil {
			if err != nil {
				t.Errorf("%s: refused with %v, want it written in place", tc.name, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: admitted — the bootstrap token would be written into it", tc.name)
			continue
		}
		for _, want := range tc.want {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: error %q does not contain %q", tc.name, err, want)
			}
		}
	}
}

// The regular-file refusal keeps its own branch and its own wording
// because its remedy differs: it is the one an operator can hit by
// accident, with `--credential-out=/dev/stdout >archon.jwt`, and the
// message has to name the form that works.
func TestStreamTypeError_RegularFileRefusalNamesTheAlternative(t *testing.T) {
	t.Parallel()
	err := streamTypeError("/tmp/target", 0o644)
	if err == nil {
		t.Fatal("a regular file behind a stream-looking name was admitted")
	}
	for _, want := range []string{"/tmp/target", "regular file", "--credential-out=-"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

// openFifoReader opens the read end of a fifo and pins a write end open
// beside it, returning the reader and the function that drops the pin.
//
// The read end is opened O_NONBLOCK because a plain O_RDONLY open waits
// for a writer, and the writer under test now waits for no reader, so
// opening both the obvious way deadlocks. The flag is cleared again
// straight away, since the reads themselves should wait.
//
// The pin is what makes reading deterministic, and it is not optional. A
// fifo read returns 0 — EOF, not "wait" — the moment no writer is left,
// so a goroutine that starts reading before writeTokenFile has opened
// its own write end gets an empty stream and a nil error, and the test
// reports "want the token, got nothing". Under `-race` that is the
// scheduling that actually happened. Holding one write end from the test
// means EOF cannot arrive until the test drops it.
func openFifoReader(t *testing.T, path string) (r *os.File, dropWriterPin func()) {
	t.Helper()
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatalf("open fifo read end: %v", err)
	}
	if err := syscall.SetNonblock(fd, false); err != nil {
		t.Fatalf("restore blocking mode on the fifo read end: %v", err)
	}
	f := os.NewFile(uintptr(fd), path)
	t.Cleanup(func() { _ = f.Close() })

	// Does not block: the read end above is already open.
	pin, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("pin a write end of the fifo open: %v", err)
	}
	var once sync.Once
	drop := func() { once.Do(func() { _ = pin.Close() }) }
	t.Cleanup(drop)
	return f, drop
}

// The stream form has to be byte-identical to the file form: the e2e
// harnesses read the credential file and TrimSpace it, and
// `keeper init --credential-out=- > archon.jwt` must be a drop-in for
// `keeper init --credential-out=archon.jwt`. Comparing the two against
// each other rather than against a literal keeps them from drifting one
// at a time.
func TestWriteTokenStream_ByteIdenticalToFile(t *testing.T) {
	const token = "header.payload.signature"

	var buf bytes.Buffer
	if err := writeTokenStream(&buf, token); err != nil {
		t.Fatalf("writeTokenStream: %v", err)
	}

	path := filepath.Join(t.TempDir(), "token")
	if _, err := writeTokenFile(path, token); err != nil {
		t.Fatalf("writeTokenFile: %v", err)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	if buf.String() != string(onDisk) {
		t.Errorf("stream wrote %q, file wrote %q — the two forms have drifted", buf.String(), onDisk)
	}
}

// A failing destination must surface, not be swallowed: Init maps the
// error to ErrTokenFileWriteFailed, which is what makes the caller print
// the token to stderr instead of losing it after the operator is already
// committed.
func TestWriteTokenStream_PropagatesWriteError(t *testing.T) {
	err := writeTokenStream(failingWriter{}, "header.payload.signature")
	if err == nil {
		t.Fatal("writeTokenStream returned nil on a writer that always fails")
	}
	if !strings.Contains(err.Error(), "no space left") {
		t.Errorf("error %q does not carry the underlying cause", err)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("no space left on device") }

func TestDefaultCredentialPath(t *testing.T) {
	got := defaultCredentialPath("archon-alice")
	// Moving away from `/tmp/` (review M0.5c #2): must go either through
	// os.UserCacheDir() (→ ending in `keeper/bootstrap-<aid>.token`), or
	// the fallback `/var/lib/keeper/bootstrap-<aid>.token`. Neither case
	// allows a `/tmp/` prefix.
	if strings.HasPrefix(got, "/tmp/") {
		t.Errorf("defaultCredentialPath = %q must not be under /tmp/ (world-readable predictable path)", got)
	}
	wantSuffix := filepath.Join("keeper", "bootstrap-archon-alice.token")
	if !strings.HasSuffix(got, wantSuffix) {
		t.Errorf("defaultCredentialPath = %q, want suffix %q", got, wantSuffix)
	}
}

func TestEnsureCredentialDir_CreatesParent(t *testing.T) {
	tmp := t.TempDir()
	target := filepath.Join(tmp, "nested", "keeper", "bootstrap.token")
	if err := ensureCredentialDir(target); err != nil {
		t.Fatalf("ensureCredentialDir: %v", err)
	}
	info, err := os.Stat(filepath.Dir(target))
	if err != nil {
		t.Fatalf("Stat parent: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("parent is not a directory")
	}
	if mode := info.Mode().Perm(); mode != 0o700 {
		t.Errorf("parent mode = %o, want 0700", mode)
	}
}

func TestEnsureCredentialDir_ExistingDirOK(t *testing.T) {
	tmp := t.TempDir()
	target := filepath.Join(tmp, "bootstrap.token")
	// tmp already exists — ensureCredentialDir must not error.
	if err := ensureCredentialDir(target); err != nil {
		t.Errorf("ensureCredentialDir on existing dir: %v", err)
	}
}

func TestEnsureCredentialDir_FileNotDir(t *testing.T) {
	tmp := t.TempDir()
	// Create a file instead of a directory.
	filePath := filepath.Join(tmp, "not-a-dir")
	if err := os.WriteFile(filePath, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// path under filePath — ensureCredentialDir will see filePath as the
	// "parent" and must return an error (not a directory).
	target := filepath.Join(filePath, "bootstrap.token")
	if err := ensureCredentialDir(target); err == nil {
		t.Error("ensureCredentialDir: expected error when parent is a regular file")
	}
}

// TestValidateConfig_RejectsBadInput is a series of validateConfig
// dry-runs without spinning up PG/Vault. Verifies that Init returns a
// clear error before any network call.
func TestValidateConfig_RejectsBadInput(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{"empty aid", Config{ArchonAID: ""}, "invalid ArchonAID"},
		{"invalid aid", Config{ArchonAID: ".alice"}, "invalid ArchonAID"},
		{"zero ttl", Config{ArchonAID: "archon-alice", TTLBootstrap: 0}, "TTLBootstrap"},
		{"negative ttl", Config{ArchonAID: "archon-alice", TTLBootstrap: -time.Second}, "TTLBootstrap"},
		{"nil pool", Config{ArchonAID: "archon-alice", TTLBootstrap: time.Hour}, "Pool is nil"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateConfig(tt.cfg)
			if err == nil {
				t.Fatalf("validateConfig(%+v): expected error", tt.cfg)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want substring %q", err, tt.want)
			}
		})
	}
}

// fakeIssuer is a JWTIssuer mock for Init unit tests.
type fakeIssuer struct {
	calls int
	token string
	err   error
}

func (f *fakeIssuer) Issue(_ string, _ []string, _ time.Duration, _ bool) (string, error) {
	f.calls++
	return f.token, f.err
}

// TestValidateConfig_FailsOnNilPoolFirst locks in the deterministic order
// of checks in validateConfig: after ArchonAID+TTL, the Pool check fails
// first. The remaining nil checks (VaultClient/IssuerFactory/AuditWriter/
// SigningKeyRef) require a non-nil *pgxpool.Pool, which cannot be
// constructed in a unit test without a running Postgres — that coverage
// lives in integration_test.go.
func TestValidateConfig_FailsOnNilPoolFirst(t *testing.T) {
	cfg := Config{
		ArchonAID:    "archon-alice",
		TTLBootstrap: time.Hour,
	}
	err := validateConfig(cfg)
	if err == nil {
		t.Fatal("validateConfig: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "Pool is nil") {
		t.Errorf("err = %v, want substring \"Pool is nil\"", err)
	}
}

// fakeAuditWriter is a Writer mock that captures the written event.
type fakeAuditWriter struct {
	events []*audit.Event
	err    error
}

func (f *fakeAuditWriter) Write(_ context.Context, ev *audit.Event) error {
	if f.err != nil {
		return f.err
	}
	f.events = append(f.events, ev)
	return nil
}
