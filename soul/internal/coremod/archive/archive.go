// Package archive implements the `core.archive` core module ([ADR-015]).
//
// State:
//   - extracted: extract archive `path` into directory `dest`.
//
// Supported formats: tar / tar.gz (.tgz) / tar.bz2 (.tbz2) / zip. `format`
// is optional — auto-detected from the extension. Extraction happens
// in-process via Go stdlib (`archive/tar`, `archive/zip`, `compress/gzip`,
// `compress/bzip2`): no external tools (`tar`/`unzip`), no subprocesses.
// This drops the dependency on host binaries and gives per-entry security
// control (zip-slip / zip-bomb / symlink policy) that backend tools can't offer.
//
// Security invariants (hard, not configurable via flags):
//   - zip-slip: an entry with `..` or an absolute path that escapes `dest`
//     fails fast (the whole extraction aborts, no silent clamp).
//   - zip-bomb: total uncompressed size is capped by `max_size` (default
//     1 GiB), entry count by `max_entries` (default 100000), and the
//     uncompressed:compressed ratio by `max_ratio` (default 100). The ratio
//     limit catches bombs that dodge max_size with a tiny compressed size
//     (10 KiB → 10 GiB). Checked fail-fast DURING extraction.
//   - symlink: created only if the resolved target stays within `dest`.
//   - setuid/setgid/sticky bits from the archive are always stripped (anti-privesc).
//   - hardlink / devnode / char / fifo → error (unsupported in the MVP).
//   - owner/group from the archive are ignored — files get the soul process's owner.
//
// Idempotency: the source archive's SHA-256 is written to
// `<dest>/.soul-archive.sha256` after extraction. Reapplying is a no-op if
// the hash matches. This is a grounded check that "it's the same archive",
// not that "all files in dest are still present" (the latter needs a full
// walk + per-file checksum comparison — overkill for the MVP).
package archive

import (
	"archive/tar"
	"archive/zip"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	securejoin "github.com/cyphar/filepath-securejoin"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

const Name = "core.archive"

// MarkerFile is the marker filename for idempotency checks, placed in dest.
const MarkerFile = ".soul-archive.sha256"

// Default zip-bomb limits: total uncompressed size, entry count, and max
// compression ratio. Overridable via params max_size/max_entries/max_ratio.
// max_ratio=0 disables the check (escape hatch for legitimate highly
// compressible archives — text/logs can have ratios in the hundreds).
const (
	defaultMaxSize    int64 = 1 << 30 // 1 GiB
	defaultMaxEntries int64 = 100000
	defaultMaxRatio   int64 = 100
)

type Module struct{}

func New() *Module { return &Module{} }

// Validate is not fully delegated to util.ValidateAgainstManifest (unlike
// core.exec): core.archive also enforces an enum check on `format`
// (tar|tar.gz|tar.bz2|zip), which the manifest DSL can't express yet (enum
// support deferred, see H1), so this stays manual. known-state/required
// duplicate the manifest deliberately — no single source until enum support lands.
func (m *Module) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	var errs []string
	if req.State != "extracted" {
		errs = append(errs, fmt.Sprintf("unknown state %q (want extracted)", req.State))
	}
	if _, err := util.StringParam(req.Params, "path"); err != nil {
		errs = append(errs, err.Error())
	}
	if _, err := util.StringParam(req.Params, "dest"); err != nil {
		errs = append(errs, err.Error())
	}
	if fmtStr, err := util.OptStringParam(req.Params, "format"); err == nil && fmtStr != "" {
		if !knownFormat(fmtStr) {
			errs = append(errs, fmt.Sprintf("param %q: unknown format %q (want tar|tar.gz|tar.bz2|zip)", "format", fmtStr))
		}
	}
	return &pluginv1.ValidateReply{Ok: len(errs) == 0, Errors: errs}, nil
}

// PlanReadSafe declares core.archive.Plan as pure-read (ADR-031 Scry): it
// hashes src and reads the marker, never extracts (host-side default-deny marker).
func (m *Module) PlanReadSafe() {}

// Plan is a pure-read dry-run (ADR-031 Scry): hashes the source archive and
// compares against the marker file in dest — the same comparison Apply does
// before extracting. Never mutates: no MkdirAll, no extract, no marker write.
//
// Drift semantics mirror Apply idempotency:
//   - drift=false if marker exists and matches sha256(src);
//   - drift=true otherwise (no marker, hash mismatch, no dest).
//
// Same MVP limitation as Apply (ADR-015 "grounded check"): the marker proves
// "same archive", not "all files still present in dest". If a file was
// removed from dest by hand, the marker stays clean and drift=false — a full
// walk + per-file checksum is a separate slice.
func (m *Module) Plan(req *pluginv1.PlanRequest, stream grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	if req.State != "extracted" {
		return util.PlanFailed(fmt.Sprintf("unknown state %q", req.State))
	}
	src, err := util.StringParam(req.Params, "path")
	if err != nil {
		return util.PlanFailed(err.Error())
	}
	dest, err := util.StringParam(req.Params, "dest")
	if err != nil {
		return util.PlanFailed(err.Error())
	}
	format, err := util.OptStringParam(req.Params, "format")
	if err != nil {
		return util.PlanFailed(err.Error())
	}
	if format == "" {
		format = detectFormat(src)
		if format == "" {
			return util.PlanFailed(fmt.Sprintf("cannot auto-detect format for %q (use param `format`)", src))
		}
	}
	if !knownFormat(format) {
		return util.PlanFailed(fmt.Sprintf("unknown format %q", format))
	}

	srcHash, herr := hashFile(src)
	if herr != nil {
		return util.PlanFailed(herr.Error())
	}
	markerPath := filepath.Join(dest, MarkerFile)
	existing, rerr := os.ReadFile(markerPath)
	switch {
	case rerr == nil:
		return util.SendPlanFinal(stream, strings.TrimSpace(string(existing)) != srcHash)
	case errors.Is(rerr, fs.ErrNotExist):
		return util.SendPlanFinal(stream, true)
	default:
		return util.PlanFailed(fmt.Sprintf("read marker %s: %v", markerPath, rerr))
	}
}

func (m *Module) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	if req.State != "extracted" {
		return util.SendFailed(stream, fmt.Sprintf("unknown state %q", req.State))
	}
	src, err := util.StringParam(req.Params, "path")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	dest, err := util.StringParam(req.Params, "dest")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	format, err := util.OptStringParam(req.Params, "format")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if format == "" {
		format = detectFormat(src)
		if format == "" {
			return util.SendFailed(stream, fmt.Sprintf("cannot auto-detect format for %q (use param `format`)", src))
		}
	}
	if !knownFormat(format) {
		return util.SendFailed(stream, fmt.Sprintf("unknown format %q", format))
	}

	limits, err := parseLimits(req.Params)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}

	srcHash, err := hashFile(src)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}

	markerPath := filepath.Join(dest, MarkerFile)
	if existing, rerr := os.ReadFile(markerPath); rerr == nil {
		if strings.TrimSpace(string(existing)) == srcHash {
			return util.SendFinal(stream, false, map[string]any{
				"path":      src,
				"dest":      dest,
				"sha256":    srcHash,
				"extracted": true,
			})
		}
	} else if !errors.Is(rerr, fs.ErrNotExist) {
		return util.SendFailed(stream, fmt.Sprintf("read marker %s: %v", markerPath, rerr))
	}

	if err := os.MkdirAll(dest, 0o755); err != nil {
		return util.SendFailed(stream, fmt.Sprintf("mkdir %s: %v", dest, err))
	}
	if err := extract(format, src, dest, limits); err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if err := os.WriteFile(markerPath, []byte(srcHash+"\n"), 0o644); err != nil {
		return util.SendFailed(stream, fmt.Sprintf("write marker %s: %v", markerPath, err))
	}
	return util.SendFinal(stream, true, map[string]any{
		"path":      src,
		"dest":      dest,
		"sha256":    srcHash,
		"extracted": true,
	})
}

// limits is the zip-bomb budget for extraction. maxSize/maxEntries are hard
// caps; maxRatio caps the uncompressed:compressed ratio (0 = disabled).
// usedSize/entries/compressed accumulate during extraction. compressed counts
// COMPRESSED source bytes read: per-entry header (CompressedSize64) for zip,
// a countingReader on the source stream for tar.gz/tar.bz2; stays 0 for
// plain tar (an uncompressed format can't be a ratio bomb, so ratio isn't checked).
type limits struct {
	maxSize    int64
	maxEntries int64
	maxRatio   int64
	usedSize   int64
	entries    int64
	compressed int64
}

// countEntry counts one more entry against maxEntries.
func (l *limits) countEntry() error {
	l.entries++
	if l.entries > l.maxEntries {
		return fmt.Errorf("archive: exceeded max_entries (%d)", l.maxEntries)
	}
	return nil
}

// addSize counts n uncompressed bytes against maxSize.
func (l *limits) addSize(n int64) error {
	l.usedSize += n
	if l.usedSize > l.maxSize {
		return fmt.Errorf("archive: exceeded max_size (%d bytes)", l.maxSize)
	}
	return nil
}

// addCompressed counts n compressed bytes read (per-entry for zip).
func (l *limits) addCompressed(n int64) {
	l.compressed += n
}

// checkRatio checks the uncompressed:compressed ratio against maxRatio.
// Called AFTER accounting for each file (usedSize and compressed updated),
// so it's caught mid-extraction rather than at the end. maxRatio=0 disables
// the check. compressed=0 with nonzero usedSize (plain tar, or no compressed
// bytes read yet) is treated as "ratio undefined" and skipped: plain tar is
// uncompressed and can't be a ratio bomb, and max_size already bounds it.
func (l *limits) checkRatio() error {
	if l.maxRatio == 0 || l.compressed == 0 {
		return nil
	}
	if l.usedSize > l.compressed*l.maxRatio {
		return fmt.Errorf("archive: exceeded max_ratio (%d): %d uncompressed / %d compressed bytes",
			l.maxRatio, l.usedSize, l.compressed)
	}
	return nil
}

func parseLimits(params *structpb.Struct) (*limits, error) {
	maxSizeStr, err := util.OptStringParam(params, "max_size")
	if err != nil {
		return nil, err
	}
	l := &limits{maxSize: defaultMaxSize, maxEntries: defaultMaxEntries, maxRatio: defaultMaxRatio}
	if maxSizeStr != "" {
		n, perr := parseSize(maxSizeStr)
		if perr != nil {
			return nil, perr
		}
		if n <= 0 {
			return nil, fmt.Errorf("param %q: must be positive, got %d", "max_size", n)
		}
		l.maxSize = n
	}
	if n, present, ierr := util.OptIntParam(params, "max_entries"); ierr != nil {
		return nil, ierr
	} else if present {
		if n <= 0 {
			return nil, fmt.Errorf("param %q: must be positive, got %d", "max_entries", n)
		}
		l.maxEntries = n
	}
	// max_ratio: 0 is allowed (disable), negative is a config error.
	if n, present, ierr := util.OptIntParam(params, "max_ratio"); ierr != nil {
		return nil, ierr
	} else if present {
		if n < 0 {
			return nil, fmt.Errorf("param %q: must be >= 0 (0 disables), got %d", "max_ratio", n)
		}
		l.maxRatio = n
	}
	return l, nil
}

// countingReader wraps the source's compressed stream and accumulates actual
// COMPRESSED bytes read into lim.compressed. For tar.gz/tar.bz2 this is the
// only way to track compressed size incrementally (no per-entry size in the
// stream) — the gzip/bzip2 decoder reads exactly as much compressed data as
// needed for the uncompressed bytes emitted so far, so ratio checks stay live.
type countingReader struct {
	r   io.Reader
	lim *limits
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.lim.addCompressed(int64(n))
	return n, err
}

func extract(format, src, dest string, lim *limits) error {
	switch format {
	case "tar":
		f, err := os.Open(src)
		if err != nil {
			return fmt.Errorf("open %s: %v", src, err)
		}
		defer f.Close()
		return extractTar(f, dest, lim)
	case "tar.gz", "tgz":
		f, err := os.Open(src)
		if err != nil {
			return fmt.Errorf("open %s: %v", src, err)
		}
		defer f.Close()
		gz, err := gzip.NewReader(&countingReader{r: f, lim: lim})
		if err != nil {
			return fmt.Errorf("archive: gzip %s: %v", src, err)
		}
		defer gz.Close()
		return extractTar(gz, dest, lim)
	case "tar.bz2", "tbz2":
		f, err := os.Open(src)
		if err != nil {
			return fmt.Errorf("open %s: %v", src, err)
		}
		defer f.Close()
		return extractTar(bzip2.NewReader(&countingReader{r: f, lim: lim}), dest, lim)
	case "zip":
		return extractZip(src, dest, lim)
	default:
		return fmt.Errorf("archive: unknown format %q", format)
	}
}

func extractTar(r io.Reader, dest string, lim *limits) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("archive: read tar: %v", err)
		}
		if err := lim.countEntry(); err != nil {
			return err
		}
		target, err := resolveEntry(dest, hdr.Name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, dirMode(fs.FileMode(hdr.Mode))); err != nil {
				return fmt.Errorf("archive: mkdir %s: %v", target, err)
			}
		case tar.TypeReg:
			if err := writeFileEntry(target, tr, safeFileMode(fs.FileMode(hdr.Mode)), lim); err != nil {
				return err
			}
			if err := lim.checkRatio(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := writeSymlink(dest, target, hdr.Linkname); err != nil {
				return err
			}
		case tar.TypeLink, tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			return fmt.Errorf("archive: entry %q: unsupported type", hdr.Name)
		default:
			// PAX/GNU metadata headers (x/g/sparse): carry no file data,
			// skip silently — they don't affect the extracted tree.
			continue
		}
	}
}

func extractZip(src, dest string, lim *limits) error {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return fmt.Errorf("archive: open zip %s: %v", src, err)
	}
	defer zr.Close()
	for _, zf := range zr.File {
		if err := lim.countEntry(); err != nil {
			return err
		}
		target, err := resolveEntry(dest, zf.Name)
		if err != nil {
			return err
		}
		mode := zf.Mode()
		switch {
		case mode.IsDir():
			if err := os.MkdirAll(target, dirMode(mode)); err != nil {
				return fmt.Errorf("archive: mkdir %s: %v", target, err)
			}
		case mode&fs.ModeSymlink != 0:
			link, rerr := readZipSymlink(zf)
			if rerr != nil {
				return rerr
			}
			if err := writeSymlink(dest, target, link); err != nil {
				return err
			}
		case mode&fs.ModeNamedPipe != 0, mode&fs.ModeDevice != 0,
			mode&fs.ModeCharDevice != 0, mode&fs.ModeSocket != 0:
			return fmt.Errorf("archive: entry %q: unsupported type", zf.Name)
		default:
			rc, oerr := zf.Open()
			if oerr != nil {
				return fmt.Errorf("archive: open zip entry %q: %v", zf.Name, oerr)
			}
			werr := writeFileEntry(target, rc, safeFileMode(mode), lim)
			rc.Close()
			if werr != nil {
				return werr
			}
			// zip entry's compressed size is known from its header (per-entry) —
			// account for it and fail-fast check ratio after each file.
			lim.addCompressed(int64(zf.CompressedSize64))
			if err := lim.checkRatio(); err != nil {
				return err
			}
		}
	}
	return nil
}

// resolveEntry builds a safe write path inside dest and FAILS FAST if the
// entry escapes dest (`..` or absolute path). SecureJoin silently clamps
// escapes, so we don't rely on that — escapes are detected lexically instead
// (explicit error, not a silent clamp). An absolute path collapses invisibly
// in Join, so it's caught separately via IsAbs; `..` is caught by the result
// landing outside dest.
func resolveEntry(dest, name string) (string, error) {
	clean := filepath.ToSlash(name)
	if filepath.IsAbs(clean) || strings.HasPrefix(clean, "/") {
		return "", fmt.Errorf("archive: entry %q escapes dest", name)
	}
	naive := filepath.Clean(filepath.Join(dest, clean))
	if !withinDest(dest, naive) {
		return "", fmt.Errorf("archive: entry %q escapes dest", name)
	}
	joined, err := securejoin.SecureJoin(dest, name)
	if err != nil || joined != naive {
		return "", fmt.Errorf("archive: entry %q escapes dest", name)
	}
	return joined, nil
}

// writeSymlink creates symlink target → link, only if the resolved link stays
// within dest. target is already a safe path inside dest; link resolves
// relative to its directory (linkDir). An absolute link target is by
// definition outside dest → error; a relative one is resolved lexically from
// linkDir and checked against escaping dest.
func writeSymlink(dest, target, link string) error {
	if filepath.IsAbs(link) {
		return fmt.Errorf("archive: symlink %q target escapes dest", link)
	}
	linkDir := filepath.Dir(target)
	resolved := filepath.Clean(filepath.Join(linkDir, link))
	if !withinDest(dest, resolved) {
		return fmt.Errorf("archive: symlink %q target escapes dest", link)
	}
	if err := os.MkdirAll(linkDir, 0o755); err != nil {
		return fmt.Errorf("archive: mkdir %s: %v", linkDir, err)
	}
	_ = os.Remove(target)
	if err := os.Symlink(link, target); err != nil {
		return fmt.Errorf("archive: symlink %s: %v", target, err)
	}
	return nil
}

func withinDest(dest, p string) bool {
	rel, err := filepath.Rel(dest, p)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// writeFileEntry materializes a regular file entry. Size is bounded via
// io.LimitReader (budget+1); exceeding maxSize is a zip-bomb error.
func writeFileEntry(target string, r io.Reader, mode fs.FileMode, lim *limits) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("archive: mkdir %s: %v", filepath.Dir(target), err)
	}
	f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return fmt.Errorf("archive: create %s: %v", target, err)
	}
	// budget+1 detects overrun exactly at the limit (io.Copy reads one
	// extra byte, addSize catches it).
	budget := lim.maxSize - lim.usedSize + 1
	n, cerr := io.Copy(f, io.LimitReader(r, budget))
	if closeErr := f.Close(); closeErr != nil && cerr == nil {
		cerr = closeErr
	}
	if cerr != nil {
		return fmt.Errorf("archive: write %s: %v", target, cerr)
	}
	if err := os.Chmod(target, mode); err != nil {
		return fmt.Errorf("archive: chmod %s: %v", target, err)
	}
	return lim.addSize(n)
}

// readZipSymlink reads a zip symlink entry's body — the target path.
func readZipSymlink(zf *zip.File) (string, error) {
	rc, err := zf.Open()
	if err != nil {
		return "", fmt.Errorf("archive: open zip symlink %q: %v", zf.Name, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, 4096))
	if err != nil {
		return "", fmt.Errorf("archive: read zip symlink %q: %v", zf.Name, err)
	}
	return string(data), nil
}

// safeFileMode strips setuid/setgid/sticky from the archive mode
// (anti-privesc invariant), keeping only permission bits.
func safeFileMode(mode fs.FileMode) fs.FileMode {
	return mode & 0o777
}

// dirMode is the archive's directory mode (masked); zero/empty falls back to 0o755.
func dirMode(mode fs.FileMode) fs.FileMode {
	perm := mode & 0o777
	if perm == 0 {
		return 0o755
	}
	return perm
}

func detectFormat(path string) string {
	lower := strings.ToLower(path)
	switch {
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
		return "tar.gz"
	case strings.HasSuffix(lower, ".tar.bz2"), strings.HasSuffix(lower, ".tbz2"):
		return "tar.bz2"
	case strings.HasSuffix(lower, ".tar"):
		return "tar"
	case strings.HasSuffix(lower, ".zip"):
		return "zip"
	}
	return ""
}

func knownFormat(f string) bool {
	switch f {
	case "tar", "tar.gz", "tgz", "tar.bz2", "tbz2", "zip":
		return true
	}
	return false
}

// parseSize parses a human-readable size: a bare number (bytes) or a number
// with a KiB/MiB/GiB suffix (binary multipliers, case-insensitive). Decimal
// SI suffixes (KB/MB) and fractions aren't supported — an unrecognized
// remainder after stripping a known suffix is a config error, not a silent
// partial-parse.
func parseSize(s string) (int64, error) {
	str := strings.TrimSpace(s)
	if str == "" {
		return 0, fmt.Errorf("param %q: empty", "max_size")
	}
	upper := strings.ToUpper(str)
	var mult int64 = 1
	switch {
	case strings.HasSuffix(upper, "GIB"):
		mult, upper = 1<<30, strings.TrimSuffix(upper, "GIB")
	case strings.HasSuffix(upper, "MIB"):
		mult, upper = 1<<20, strings.TrimSuffix(upper, "MIB")
	case strings.HasSuffix(upper, "KIB"):
		mult, upper = 1<<10, strings.TrimSuffix(upper, "KIB")
	}
	upper = strings.TrimSpace(upper)
	// ParseInt on the full remainder: any unrecognized suffix/garbage/fraction
	// errors instead of silently parsing a prefix (footgun: "10MB" → 10 bytes).
	n, err := strconv.ParseInt(upper, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("param %q: invalid size %q (want a number or N[KiB|MiB|GiB])", "max_size", s)
	}
	// Guard against overflow on multiplication.
	if mult != 1 && n > (1<<62)/mult {
		return 0, fmt.Errorf("param %q: size %q too large", "max_size", s)
	}
	return n * mult, nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s: %v", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("read %s: %v", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
