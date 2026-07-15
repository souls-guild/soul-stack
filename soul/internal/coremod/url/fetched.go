package url

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
)

// applyFetched implements state `fetched`.
//
// Idempotency and order of operations:
//   - checksum given + path exists and hash matches → no-op (no download);
//   - checksum given, hash mismatch / file missing → download to temp →
//     verify → atomic rename (verify mismatch → failed, temp removed,
//     target path untouched — supply-chain safety);
//   - checksum NOT given → always download to temp → compare SHA-256 with
//     the existing file → write only on diff (correct changed).
//
// Download always goes to a temp file in path's directory; materialization
// is a rename (util.AtomicWrite pattern): the target path never sees a
// partial or unverified file.
func (m *Module) applyFetched(stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], req *pluginv1.ApplyRequest) error {
	allowHTTP, err := util.OptBoolParam(req.Params, "allow_http")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	insecureSkipVerify, err := util.OptBoolParam(req.Params, "insecure_skip_verify")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	allowPrivate, err := util.OptBoolParam(req.Params, "allow_private")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}

	rawURL, err := util.StringParam(req.Params, "url")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if verr := util.ValidateFetchURL(rawURL, allowHTTP); verr != nil {
		return util.SendFailed(stream, verr.Error())
	}
	path, err := util.StringParam(req.Params, "path")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	checksum, err := util.OptStringParam(req.Params, "checksum")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	modeStr, err := util.OptStringParam(req.Params, "mode")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	owner, err := util.OptStringParam(req.Params, "owner")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	group, err := util.OptStringParam(req.Params, "group")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	headers, err := util.OptStringMapParam(req.Params, "headers")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	timeoutStr, err := util.OptStringParam(req.Params, "timeout")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}

	mode, err := util.ParseMode(modeStr)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}

	timeout := defaultTimeout
	if timeoutStr != "" {
		timeout, err = parseTimeout(timeoutStr)
		if err != nil {
			return util.SendFailed(stream, err.Error())
		}
	}

	var algo, wantHex string
	if checksum != "" {
		algo, wantHex, err = parseChecksum(checksum)
		if err != nil {
			return util.SendFailed(stream, err.Error())
		}
	}

	// The client is built per-Apply from the parsed opt-out flags: each flag
	// weakens an independent guard (scheme / dial guard / TLS chain).
	clientOpts := util.HTTPClientOpts{
		AllowPrivate:       allowPrivate,
		InsecureSkipVerify: insecureSkipVerify,
		AllowHTTPRedirect:  allowHTTP,
	}
	// When a guard is lifted — a warning (host only) in the final event's
	// output: the operator sees the guard was weakened in RunResult
	// (core.repo/core.http convention). Wording and host-masking share a
	// single util helper.
	warnings := util.GuardWarnings(util.WarnHost(rawURL), clientOpts)
	client := m.NewClient(clientOpts)

	// State of the existing file. For the checksum branch, hash with the
	// declared algorithm; for the checksum-less branch, always SHA-256
	// (output format).
	existingHashHex, existingExists, herr := hashExisting(path, hashAlgoFor(algo))
	if herr != nil {
		return util.SendFailed(stream, herr.Error())
	}

	// checksum given + existing file already matches → content isn't
	// downloaded, but mode/owner are still converged to the declaration (as
	// in rendered).
	if checksum != "" && existingExists && strings.EqualFold(existingHashHex, wantHex) {
		attrChanged, aerr := m.converge(path, modeStr, mode, owner, group)
		if aerr != nil {
			return util.SendFailed(stream, aerr.Error())
		}
		sha, _ := canonicalSHA256(path, algo, existingHashHex)
		return finalOutput(stream, attrChanged, path, rawURL, sha, fileSize(path), warnings)
	}

	// Download to a temp file in path's directory, hashing on the fly.
	// notModified=true means a 304 on a conditional GET (If-None-Match via
	// headers): body not downloaded, temp not created.
	tmpName, sha256Hex, algoHex, size, notModified, derr := m.download(stream.Context(), client, rawURL, path, headers, timeout, algo)
	if derr != nil {
		return util.SendFailed(stream, derr.Error())
	}

	// 304 Not Modified: the operator set a conditional GET (If-None-Match/
	// If-Modified-Since in headers) and the server confirmed the local copy
	// is current.
	if notModified {
		if !existingExists {
			// Server said "not modified" but there's no local file: stale
			// If-None-Match with no cache. Can't download (no body) — fail-fast.
			return util.SendFailed(stream, fmt.Sprintf(
				"server returned 304 but no local file at %s: stale If-None-Match without cache", path))
		}
		// Content is current → no write needed, but mode/owner are converged.
		attrChanged, aerr := m.converge(path, modeStr, mode, owner, group)
		if aerr != nil {
			return util.SendFailed(stream, aerr.Error())
		}
		sha, serr := canonicalSHA256(path, algo, existingHashHex)
		if serr != nil {
			return util.SendFailed(stream, serr.Error())
		}
		return finalOutput(stream, attrChanged, path, rawURL, sha, fileSize(path), warnings)
	}

	cleanup := func() { _ = os.Remove(tmpName) }

	// Verify BEFORE publishing. Mismatch → failed, temp removed, target path
	// untouched. A bad hash is never materialized.
	if checksum != "" && !strings.EqualFold(algoHex, wantHex) {
		cleanup()
		return util.SendFailed(stream, fmt.Sprintf(
			"checksum mismatch for %s: want %s:%s, got %s:%s", rawURL, algo, wantHex, algo, algoHex))
	}

	// Checksum-less branch: if content matches the existing file, no write is
	// needed, but mode/owner are still checked/fixed (convergence). Compared
	// by SHA-256.
	if checksum == "" && existingExists && strings.EqualFold(existingHashHex, sha256Hex) {
		cleanup()
		attrChanged, aerr := m.converge(path, modeStr, mode, owner, group)
		if aerr != nil {
			return util.SendFailed(stream, aerr.Error())
		}
		return finalOutput(stream, attrChanged, path, rawURL, sha256Hex, size, warnings)
	}

	if err := os.Chmod(tmpName, mode); err != nil {
		cleanup()
		return util.SendFailed(stream, fmt.Sprintf("chmod temp for %s: %v", path, err))
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return util.SendFailed(stream, fmt.Sprintf("rename %s -> %s: %v", tmpName, path, err))
	}

	if owner != "" || group != "" {
		if _, oerr := util.ApplyOwnership(path, owner, group, m.LookupUser, m.LookupGroup); oerr != nil {
			return util.SendFailed(stream, oerr.Error())
		}
	}

	return finalOutput(stream, true, path, rawURL, sha256Hex, size, warnings)
}

// converge brings an existing file's mode/owner to the declaration when
// content already matched and no download/rewrite is needed. Semantics are
// 1:1 with rendered ([file/rendered.go]): mode is checked/fixed only when
// modeStr is set (empty mode doesn't force a default onto an existing file);
// owner/group via util.ApplyOwnership. changed = mode-diff || owner-diff.
func (m *Module) converge(path, modeStr string, mode fs.FileMode, owner, group string) (bool, error) {
	modeChanged := false
	if modeStr != "" {
		info, err := os.Stat(path)
		if err != nil {
			return false, fmt.Errorf("stat %s: %v", path, err)
		}
		if info.Mode().Perm() != mode {
			if cerr := os.Chmod(path, mode); cerr != nil {
				return false, fmt.Errorf("chmod %s: %v", path, cerr)
			}
			modeChanged = true
		}
	}

	ownerChanged := false
	if owner != "" || group != "" {
		changed, oerr := util.ApplyOwnership(path, owner, group, m.LookupUser, m.LookupGroup)
		if oerr != nil {
			return false, oerr
		}
		ownerChanged = changed
	}

	return modeChanged || ownerChanged, nil
}

// download performs a GET into a temp file next to path, hashing SHA-256
// (for output) in parallel and, if algo is set and isn't sha256, hashing by
// algo too (for verify). Returns the temp file name, SHA-256 hash, algo hash
// (=sha256Hex if algo is empty or sha256), size, and the notModified flag.
// The temp file is removed internally on any error.
//
// notModified=true (HTTP 304) is handled BEFORE the 2xx check: on a
// conditional GET (If-None-Match/If-Modified-Since in headers), 304 is the
// expected "not modified" response — body isn't downloaded, temp isn't
// created. The caller decides no-op vs. error (depends on local file
// presence).
//
// client is a per-call HTTP client built from the task's opt-out flags.
// headers are applied to the request but NEVER logged or returned
// (sensitive-by-construction, [ADR-010] §7.4).
func (m *Module) download(
	ctx context.Context, client util.HTTPDoer, rawURL, path string, headers map[string]string,
	timeout time.Duration, algo string,
) (tmpName, sha256Hex, algoHex string, size int64, notModified bool, err error) {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", "", "", 0, false, fmt.Errorf("build request for %s: %v", rawURL, err)
	}
	for k, v := range headers {
		httpReq.Header.Set(k, v)
	}

	resp, err := client.Do(httpReq)
	if err != nil {
		return "", "", "", 0, false, fmt.Errorf("fetch %s: %v", rawURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	// 304 comes before the 2xx check: a conditional GET (If-None-Match)
	// normally gets 304 — not an "unexpected status".
	if resp.StatusCode == http.StatusNotModified {
		return "", "", "", 0, true, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", "", 0, false, fmt.Errorf("fetch %s: unexpected status %d", rawURL, resp.StatusCode)
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return "", "", "", 0, false, fmt.Errorf("create temp for %s: %v", path, err)
	}
	tmpName = tmp.Name()

	sha := sha256.New()
	var extra hash.Hash
	if algo != "" && algo != "sha256" {
		extra = hashAlgoFor(algo)
	}
	writers := []io.Writer{tmp, sha}
	if extra != nil {
		writers = append(writers, extra)
	}

	n, copyErr := io.Copy(io.MultiWriter(writers...), resp.Body)
	if copyErr != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", "", "", 0, false, fmt.Errorf("download %s: %v", rawURL, copyErr)
	}
	if syncErr := tmp.Sync(); syncErr != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", "", "", 0, false, fmt.Errorf("fsync temp for %s: %v", path, syncErr)
	}
	if closeErr := tmp.Close(); closeErr != nil {
		_ = os.Remove(tmpName)
		return "", "", "", 0, false, fmt.Errorf("close temp for %s: %v", path, closeErr)
	}

	sha256Hex = hex.EncodeToString(sha.Sum(nil))
	algoHex = sha256Hex
	if extra != nil {
		algoHex = hex.EncodeToString(extra.Sum(nil))
	}
	return tmpName, sha256Hex, algoHex, n, false, nil
}

// parseChecksum parses the "<algo>:<hex>" form. sha256 and sha1 are
// supported (md5 deliberately unsupported — too weak for supply-chain use).
// hex is checked for correct length and alphabet.
func parseChecksum(s string) (algo, hexDigest string, err error) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("param %q: expected \"<algo>:<hex>\", got %q", "checksum", s)
	}
	algo = strings.ToLower(strings.TrimSpace(parts[0]))
	hexDigest = strings.TrimSpace(parts[1])

	var wantLen int
	switch algo {
	case "sha256":
		wantLen = sha256.Size * 2
	case "sha1":
		wantLen = sha1.Size * 2
	default:
		return "", "", fmt.Errorf("param %q: unsupported algo %q (want sha256|sha1)", "checksum", algo)
	}
	if len(hexDigest) != wantLen {
		return "", "", fmt.Errorf("param %q: %s hex must be %d chars, got %d", "checksum", algo, wantLen, len(hexDigest))
	}
	if _, derr := hex.DecodeString(hexDigest); derr != nil {
		return "", "", fmt.Errorf("param %q: invalid hex digest %q", "checksum", hexDigest)
	}
	return algo, hexDigest, nil
}

// hashAlgoFor returns the hash.Hash for a checksum algorithm. Empty or
// unknown algo → SHA-256 (the checksum-less branch always uses SHA-256).
func hashAlgoFor(algo string) hash.Hash {
	if algo == "sha1" {
		return sha1.New()
	}
	return sha256.New()
}

// hashExisting hashes an existing file with the given hash. Returns hex,
// an existence flag, and an error. Missing file → ("", false, nil).
func hashExisting(path string, h hash.Hash) (string, bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("open %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	if _, err := io.Copy(h, f); err != nil {
		return "", false, fmt.Errorf("read %s: %v", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), true, nil
}

// canonicalSHA256 returns the file's SHA-256 hash for output. In the no-op
// branch with a sha1 checksum, the already-computed sha1 hash won't do for
// output — the file's SHA-256 is recomputed. If algo is empty or sha256, the
// already-computed hash is returned.
func canonicalSHA256(path, algo, existingHashHex string) (string, error) {
	if algo == "" || algo == "sha256" {
		return existingHashHex, nil
	}
	sha, _, err := hashExisting(path, sha256.New())
	if err != nil {
		return "", err
	}
	return sha, nil
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// finalOutput assembles the module's output. headers are never included in
// output (sensitive-by-construction). url is echoed back without headers.
// warnings (if any) are host-only guard warnings from util.GuardWarnings,
// surfaced to the operator in RunResult.
func finalOutput(stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], changed bool, path, rawURL, sha256Hex string, size int64, warnings []string) error {
	out := map[string]any{
		"path":    path,
		"url":     rawURL,
		"sha256":  sha256Hex,
		"size":    size,
		"changed": changed,
		"fetched": true,
	}
	if len(warnings) > 0 {
		out["warnings"] = util.StringsToAny(warnings)
	}
	return util.SendFinal(stream, changed, out)
}
